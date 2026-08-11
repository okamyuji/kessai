package housekeeping_test

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/housekeeping"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
)

var sharedPG *pgcontainer.Container

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

func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if sharedPG == nil {
		t.Skip("testcontainers未起動")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

func newRunner(pool *pgxpool.Pool, q *sqlc.Queries, policy housekeeping.ExpiryPolicy) *housekeeping.Runner {
	return housekeeping.New(q, &housekeeping.PoolAdapter{Pool: pool}, idgen.NewDefault(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), policy)
}

func seedPayment(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state string, updatedAt time.Time) string {
	t.Helper()
	ids := idgen.NewDefault()
	productID := ids.New()
	paymentID := ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		productID, "seed", int64(1000), `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy, state, updated_at) VALUES ($1,$2,$3,$4::payment_state,$5)`,
		paymentID, productID, int64(1000), state, updatedAt); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return paymentID
}

// dupIDGen 常に同じIDを返すスタブ（履歴INSERTのPK衝突を意図的に起こす）
type dupIDGen struct{ id string }

func (g *dupIDGen) New() string { return g.id }

// TestHousekeeping_ExpireAtomic 履歴INSERT失敗時に状態更新もロールバックされることを検証する
func TestHousekeeping_ExpireAtomic(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	old := time.Now().Add(-2 * time.Hour)
	paymentID := seedPayment(t, ctx, pool, "created", old)
	// 事前にpayment_transitionsへ行を入れ、同じIDでの履歴INSERTを失敗させる
	otherPayment := seedPayment(t, ctx, pool, "captured", time.Now())
	if _, err := pool.Exec(ctx,
		`INSERT INTO payment_transitions (id, payment_id, from_state, to_state, event, actor)
		 VALUES ('DUP-TRANSITION-ID', $1, 'authorized'::payment_state, 'captured'::payment_state, 'Capture', 'webhook')`,
		otherPayment); err != nil {
		t.Fatalf("seed transition: %v", err)
	}
	r := housekeeping.New(q, &housekeeping.PoolAdapter{Pool: pool}, &dupIDGen{id: "DUP-TRANSITION-ID"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		housekeeping.ExpiryPolicy{CheckoutExpiryMinutes: 60, AuthExpiryDays: 21})

	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatalf("履歴INSERT失敗でエラーが返らない")
	}
	pay, err := q.GetPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if string(pay.State) != "created" {
		t.Fatalf("履歴INSERT失敗なのに状態が%sへ変わっている（原子性違反）", pay.State)
	}
}

// 期限切れ idempotency_keys の削除
func TestHousekeeping_DeletesExpiredIdempotency(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	payID := seedPayment(t, ctx, pool, "created", time.Now())
	// 有効期限が過去のキーと未来のキー
	pastKey := idgen.NewDefault().New()
	futureKey := idgen.NewDefault().New()
	if _, err := q.TryInsertIdempotency(ctx, sqlc.TryInsertIdempotencyParams{
		Key: pastKey, RequestHash: []byte{1}, PaymentID: &payID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("past: %v", err)
	}
	if _, err := q.TryInsertIdempotency(ctx, sqlc.TryInsertIdempotencyParams{
		Key: futureKey, RequestHash: []byte{2}, PaymentID: &payID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("future: %v", err)
	}
	r := newRunner(pool, q, housekeeping.ExpiryPolicy{CheckoutExpiryMinutes: 60, AuthExpiryDays: 21})
	res, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.DeletedIdempotency < 1 {
		t.Fatalf("削除件数=%d want >=1", res.DeletedIdempotency)
	}
	// 過去分は消え、未来分は残る
	if _, err := q.GetIdempotency(ctx, pastKey); err == nil {
		t.Fatalf("過去分が残っている")
	}
	if _, err := q.GetIdempotency(ctx, futureKey); err != nil {
		t.Fatalf("未来分が消えた: %v", err)
	}
}

// created のまま古い payments が expired に遷移し、履歴が追記されること
func TestHousekeeping_ExpiresCreated(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	// updated_at を100分前に置く（CheckoutExpiryMinutes=60 なので対象）
	oldPay := seedPayment(t, ctx, pool, "created", time.Now().Add(-100*time.Minute))
	// updated_at を今にした payment は対象外
	freshPay := seedPayment(t, ctx, pool, "created", time.Now())

	r := newRunner(pool, q, housekeeping.ExpiryPolicy{CheckoutExpiryMinutes: 60, AuthExpiryDays: 21})
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if p, err := q.GetPayment(ctx, oldPay); err != nil {
		t.Fatalf("get: %v", err)
	} else if string(p.State) != "expired" {
		t.Fatalf("state=%s want expired", p.State)
	}
	if p, err := q.GetPayment(ctx, freshPay); err != nil {
		t.Fatalf("get fresh: %v", err)
	} else if string(p.State) != "created" {
		t.Fatalf("fresh state=%s want created", p.State)
	}
	// 履歴に Expire イベントが追記されている
	rows, err := q.ListPaymentTransitions(ctx, oldPay)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(rows) != 1 || rows[0].Event != "Expire" {
		t.Fatalf("history=%+v", rows)
	}
}
