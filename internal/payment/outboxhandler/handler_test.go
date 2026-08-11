package outboxhandler_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/ledger"
	"github.com/okamyuji/kessai/internal/outbox"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/outboxhandler"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
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
		log.Printf("pgcontainer未起動: %v", err)
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

type stubStripe struct{}

func (*stubStripe) CreatePaymentIntent(context.Context, stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	return &stripeclient.Intent{ID: "pi_x", ClientSecret: "cs_x", Status: "requires_confirmation"}, nil
}
func (*stubStripe) CapturePaymentIntent(context.Context, stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (*stubStripe) Refund(context.Context, stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	return nil, nil
}

// payment_intent.succeeded を受け取ると、payments.stateはcapturedになり、
// ledger_entriesに借方=PSP未収金/貸方=売上 の1組が追記される。
// 未対応イベント種別は成功扱いで無視されること
func TestOutboxHandler_UnknownEvent_Skipped(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	uc := payment.NewUseCase(store, &stubStripe{}, idgen.NewDefault(), "manual", time.Hour)
	prodID, paymentID, pi := seedProductPayment(t, ctx, pool, 500)
	_ = prodID
	// 未対応イベント種別
	whID := idgen.NewDefault().New()
	stripeEventID := "evt_unknown_" + paymentID
	stripePayload, _ := json.Marshal(map[string]any{
		"id": stripeEventID, "type": "charge.dispute.created",
		"data": map[string]any{"object": map[string]any{"id": pi, "amount": float64(500)}},
	})
	if _, err := q.TryInsertWebhookEvent(ctx, sqlc.TryInsertWebhookEventParams{
		ID: whID, StripeEventID: stripeEventID, EventType: "charge.dispute.created", Payload: stripePayload,
	}); err != nil {
		t.Fatalf("wh: %v", err)
	}
	obxPayload, _ := json.Marshal(map[string]string{"webhook_event_id": whID, "stripe_event_id": stripeEventID})
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID: idgen.NewDefault().New(), EventType: "charge.dispute.created", Payload: obxPayload,
	}); err != nil {
		t.Fatalf("enq: %v", err)
	}
	handler := outboxhandler.New(q, uc, store, idgen.NewDefault(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay := outbox.New(pool, q, handler.Handle, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := relay.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce n=%d err=%v", n, err)
	}
	// state 変更なし
	p, _ := q.GetPayment(ctx, paymentID)
	if string(p.State) != "created" {
		t.Fatalf("state=%s want created", p.State)
	}
}

// payment_intent.payment_failed でfailed遷移
func TestOutboxHandler_PaymentFailed_Transitions(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	uc := payment.NewUseCase(store, &stubStripe{}, idgen.NewDefault(), "manual", time.Hour)
	prodID, paymentID, pi := seedProductPayment(t, ctx, pool, 500)
	_ = prodID
	whID := idgen.NewDefault().New()
	stripeEventID := "evt_fail_" + paymentID
	stripePayload, _ := json.Marshal(map[string]any{
		"id": stripeEventID, "type": "payment_intent.payment_failed",
		"data": map[string]any{"object": map[string]any{"id": pi, "amount": float64(500)}},
	})
	if _, err := q.TryInsertWebhookEvent(ctx, sqlc.TryInsertWebhookEventParams{
		ID: whID, StripeEventID: stripeEventID, EventType: "payment_intent.payment_failed", Payload: stripePayload,
	}); err != nil {
		t.Fatalf("wh: %v", err)
	}
	obxPayload, _ := json.Marshal(map[string]string{"webhook_event_id": whID, "stripe_event_id": stripeEventID})
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID: idgen.NewDefault().New(), EventType: "payment_intent.payment_failed", Payload: obxPayload,
	}); err != nil {
		t.Fatalf("enq: %v", err)
	}
	handler := outboxhandler.New(q, uc, store, idgen.NewDefault(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay := outbox.New(pool, q, handler.Handle, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	p, _ := q.GetPayment(ctx, paymentID)
	if string(p.State) != "failed" {
		t.Fatalf("state=%s want failed", p.State)
	}
}

func TestOutboxHandler_PaymentIntentSucceeded_TransitionAndLedger(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	uc := payment.NewUseCase(store, &stubStripe{}, idgen.NewDefault(), "manual", time.Hour)

	prodID, paymentID, pi := seedProductPayment(t, ctx, pool, 1000)
	whID, stripeEventID := insertWebhook(t, ctx, q, pi)
	enqueueOutbox(t, ctx, q, whID, stripeEventID)
	_ = prodID // 参照維持

	handler := outboxhandler.New(q, uc, store, idgen.NewDefault(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	relay := outbox.New(pool, q, handler.Handle, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := relay.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce n=%d err=%v", n, err)
	}
	assertCapturedWithLedger(t, ctx, q, paymentID)

	// Outboxリトライで同一transfer_idの再挿入がON CONFLICTでスキップされることを再実行で確認
	enqueueOutbox(t, ctx, q, whID, stripeEventID)
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatalf("再run: %v", err)
	}
	les2, _ := q.ListLedgerByPayment(ctx, paymentID)
	if len(les2) != 2 {
		t.Fatalf("再run後もledgerは2件のはず: got %d", len(les2))
	}
}

// -- ヘルパ ----------------------------------------------------------------

func seedProductPayment(t *testing.T, ctx context.Context, pool *pgxpool.Pool, amountJPY int64) (string, string, string) {
	t.Helper()
	ids := idgen.NewDefault()
	prodID := ids.New()
	paymentID := ids.New()
	pi := "pi_ok_" + paymentID
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		prodID, "demo", amountJPY, `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy, stripe_payment_intent_id) VALUES ($1,$2,$3,$4)`,
		paymentID, prodID, amountJPY, pi); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return prodID, paymentID, pi
}

func insertWebhook(t *testing.T, ctx context.Context, q *sqlc.Queries, pi string) (string, string) {
	t.Helper()
	whID := idgen.NewDefault().New()
	stripeEventID := "evt_" + pi
	stripePayload, _ := json.Marshal(map[string]any{
		"id": stripeEventID, "type": "payment_intent.succeeded",
		"data": map[string]any{"object": map[string]any{"id": pi, "amount": float64(1000)}},
	})
	if _, err := q.TryInsertWebhookEvent(ctx, sqlc.TryInsertWebhookEventParams{
		ID: whID, StripeEventID: stripeEventID, EventType: "payment_intent.succeeded",
		Payload: stripePayload,
	}); err != nil {
		t.Fatalf("insert webhook: %v", err)
	}
	return whID, stripeEventID
}

func enqueueOutbox(t *testing.T, ctx context.Context, q *sqlc.Queries, whID, stripeEventID string) {
	t.Helper()
	obxPayload, _ := json.Marshal(map[string]string{
		"webhook_event_id": whID, "stripe_event_id": stripeEventID,
	})
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID: idgen.NewDefault().New(), EventType: "payment_intent.succeeded", Payload: obxPayload,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func assertCapturedWithLedger(t *testing.T, ctx context.Context, q *sqlc.Queries, paymentID string) {
	t.Helper()
	p, err := q.GetPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(p.State) != "captured" {
		t.Fatalf("state=%s want captured", p.State)
	}
	les, err := q.ListLedgerByPayment(ctx, paymentID)
	if err != nil || len(les) != 2 {
		t.Fatalf("ledger len=%d err=%v", len(les), err)
	}
	var debitAcct, creditAcct ledger.Account
	for _, l := range les {
		if l.Side == "debit" {
			debitAcct = ledger.Account(l.Account)
		} else {
			creditAcct = ledger.Account(l.Account)
		}
	}
	if debitAcct != ledger.AccountPSPReceivable || creditAcct != ledger.AccountSales {
		t.Fatalf("勘定不一致 debit=%s credit=%s", debitAcct, creditAcct)
	}
}
