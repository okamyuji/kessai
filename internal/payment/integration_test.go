// integration_test 実PostgreSQLに接続してusecase層を検証します。
// testcontainers-goで起動したコンテナをTestMainで1つだけ立て、テスト間で使い回します。
// マイグレーションはdb/migrationsをfile://で直接適用し、コピーはしません。
package payment_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/statemachine"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
)

// パッケージ共有コンテナ
var (
	sharedPG *pgcontainer.Container
)

func TestMain(m *testing.M) {
	if os.Getenv("KESSAI_SKIP_INTEGRATION") != "" {
		os.Exit(m.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgcontainer.Start(ctx)
	if err != nil {
		log.Printf("pgcontainer未起動でスキップ: %v", err)
		os.Exit(m.Run())
	}
	sharedPG = c
	code := m.Run()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	_ = sharedPG.Stop(stopCtx)
	os.Exit(code)
}

// requirePG 共有コンテナが起動済みかを確認しつつリセットし、プールを返します
func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if sharedPG == nil {
		t.Skip("testcontainersが利用できない環境のためスキップ")
	}
	ctx := context.Background()
	if err := sharedPG.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

// intentStub 実DBに触れる統合テスト用の決済スタブ
type intentStub struct {
	callCount int
	err       error
}

func (s *intentStub) CreatePaymentIntent(_ context.Context, req stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	s.callCount++
	if s.err != nil {
		return nil, s.err
	}
	return &stripeclient.Intent{
		ID:           "pi_test_" + req.IdempotencyMaster,
		ClientSecret: "cs_test_" + req.IdempotencyMaster,
		Status:       "requires_confirmation",
		AmountJPY:    req.Amount.Int64(),
	}, nil
}
func (s *intentStub) CapturePaymentIntent(context.Context, stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (s *intentStub) Refund(context.Context, stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	return nil, nil
}

func setupProduct(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (productID, key string) {
	t.Helper()
	ids := idgen.NewDefault()
	productID = ids.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1, $2, $3, $4::jsonb)`,
		productID, "デモ商品", int64(1000), `{"seller":"kessai demo"}`)
	if err != nil {
		t.Fatalf("insert product: %v", err)
	}
	return productID, ids.New()
}

func TestIntegration_StartCheckout_HappyPath(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	productID, key := setupProduct(t, ctx, pool)

	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	stripe := &intentStub{}
	uc := payment.NewUseCase(store, stripe, idgen.NewDefault(), "manual", time.Hour)

	res, err := uc.StartCheckout(ctx, payment.StartCheckoutRequest{
		IdempotencyKey: key, ProductID: productID, Amount: money.MustNew(1000),
	})
	if err != nil {
		t.Fatalf("StartCheckout: %v", err)
	}
	if res.PaymentID == "" || res.ClientSecret == "" {
		t.Fatalf("res=%+v", res)
	}
	// 冪等再送で同じ結果が返り、Stripe呼び出しは追加されない
	res2, err := uc.StartCheckout(ctx, payment.StartCheckoutRequest{
		IdempotencyKey: key, ProductID: productID, Amount: money.MustNew(1000),
	})
	if err != nil {
		t.Fatalf("再送: %v", err)
	}
	if res2.PaymentID != res.PaymentID || res2.ClientSecret != res.ClientSecret {
		t.Fatalf("再送結果不一致 old=%+v new=%+v", res, res2)
	}
	if stripe.callCount != 1 {
		t.Fatalf("Stripe呼び出し数=%d want 1", stripe.callCount)
	}
}

func TestIntegration_StartCheckout_IdempotencyConflict(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	productID, key := setupProduct(t, ctx, pool)

	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	stripe := &intentStub{}
	uc := payment.NewUseCase(store, stripe, idgen.NewDefault(), "manual", time.Hour)

	if _, err := uc.StartCheckout(ctx, payment.StartCheckoutRequest{
		IdempotencyKey: key, ProductID: productID, Amount: money.MustNew(1000),
	}); err != nil {
		t.Fatalf("初回: %v", err)
	}
	_, err := uc.StartCheckout(ctx, payment.StartCheckoutRequest{
		IdempotencyKey: key, ProductID: productID, Amount: money.MustNew(2000),
	})
	if err != payment.ErrIdempotencyConflict {
		t.Fatalf("err=%v want ErrIdempotencyConflict", err)
	}
}

func TestIntegration_ApplyEvent_TransitionAndRow(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	productID, key := setupProduct(t, ctx, pool)

	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	stripe := &intentStub{}
	uc := payment.NewUseCase(store, stripe, idgen.NewDefault(), "manual", time.Hour)

	res, err := uc.StartCheckout(ctx, payment.StartCheckoutRequest{
		IdempotencyKey: key, ProductID: productID, Amount: money.MustNew(1000),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tx, err := store.StartTx(ctx)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	next, err := uc.ApplyEvent(ctx, tx, res.PaymentID, "webhook", statemachine.EventAuthorizeSucceeded)
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}
	if next != statemachine.StateAuthorized {
		t.Fatalf("next=%s want authorized", next)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := q.ListPaymentTransitions(ctx, res.PaymentID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("履歴 rows=%d err=%v", len(rows), err)
	}
	if rows[0].Event != string(statemachine.EventAuthorizeSucceeded) {
		t.Fatalf("event=%q", rows[0].Event)
	}
}
