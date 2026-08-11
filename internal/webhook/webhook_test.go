package webhook_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	stripe "github.com/stripe/stripe-go/v86"

	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
	"github.com/okamyuji/kessai/internal/webhook"
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
		t.Skip("testcontainers未起動のためスキップ")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

// fixedVerify 常に指定した Event を返す偽 Verify（実際のHMACを避けて手軽に検証）
func fixedVerify(evt stripe.Event) func([]byte, string, string) (stripe.Event, error) {
	return func(_ []byte, _, _ string) (stripe.Event, error) { return evt, nil }
}

func newRecv(t *testing.T, pool *pgxpool.Pool, verify func([]byte, string, string) (stripe.Event, error)) *webhook.Receiver {
	t.Helper()
	q := sqlc.New(pool)
	r := webhook.New(slog.New(slog.NewTextHandler(io.Discard, nil)), pool, q, idgen.NewDefault(), "whsec_dummy")
	if verify != nil {
		r.Verify = verify
	}
	return r
}

func TestWebhook_InvalidSignature_Returns400(t *testing.T) {
	pool := requirePG(t)
	r := newRecv(t, pool, func(_ []byte, _, _ string) (stripe.Event, error) {
		return stripe.Event{}, errors.New("bad signature")
	})
	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Stripe-Signature", "t=1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestWebhook_ValidSignature_Records200(t *testing.T) {
	pool := requirePG(t)
	q := sqlc.New(pool)
	evt := stripe.Event{ID: "evt_A", Type: stripe.EventTypePaymentIntentSucceeded}
	r := newRecv(t, pool, fixedVerify(evt))

	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader([]byte(`{"id":"evt_A"}`)))
	req.Header.Set("Stripe-Signature", "t=1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// webhook_events に1件、outbox_events に1件記録される
	rows, err := q.ListWebhookEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListWebhookEvents: %v", err)
	}
	if len(rows) != 1 || rows[0].StripeEventID != "evt_A" {
		t.Fatalf("webhook_events=%+v", rows)
	}
	obx, err := q.ListOutboxEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListOutboxEvents: %v", err)
	}
	if len(obx) != 1 {
		t.Fatalf("outbox_events=%d want 1", len(obx))
	}
}

// seqIDGen 事前に並べたIDを順に返すスタブ。尽きたら実生成にフォールバックする
type seqIDGen struct {
	ids  []string
	i    int
	real idgen.Generator
}

func (g *seqIDGen) New() string {
	if g.i < len(g.ids) {
		v := g.ids[g.i]
		g.i++
		return v
	}
	if g.real == nil {
		g.real = idgen.NewDefault()
	}
	return g.real.New()
}

// TestWebhook_OutboxInsertFailure_Atomic Outbox登録が失敗したらwebhook_events記録も残らず、再配信で回復できることを検証する
func TestWebhook_OutboxInsertFailure_Atomic(t *testing.T) {
	pool := requirePG(t)
	q := sqlc.New(pool)
	ctx := context.Background()
	// 既存のoutbox行とID衝突させて、2番目のINSERT(Outbox登録)だけを失敗させる
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID: "OUTBOX-DUP-ID", EventType: "noop", Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}
	evt := stripe.Event{ID: "evt_C", Type: stripe.EventTypePaymentIntentSucceeded}
	r := newRecv(t, pool, fixedVerify(evt))
	r.IDGen = &seqIDGen{ids: []string{"WEBHOOK-ROW-ID", "OUTBOX-DUP-ID"}}

	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader([]byte(`{"id":"evt_C"}`)))
	req.Header.Set("Stripe-Signature", "t=1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	rows, err := q.ListWebhookEvents(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, row := range rows {
		if row.StripeEventID == "evt_C" {
			t.Fatalf("Outbox登録失敗なのにwebhook_events記録が残っている（原子性違反）")
		}
	}
	// Stripeの再配信（健全なID生成）で両方の行が入る
	r2 := newRecv(t, pool, fixedVerify(evt))
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader([]byte(`{"id":"evt_C"}`)))
	req2.Header.Set("Stripe-Signature", "t=1")
	r2.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("再配信 status=%d want 200", rec2.Code)
	}
	rows2, err := q.ListWebhookEvents(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := 0
	for _, row := range rows2 {
		if row.StripeEventID == "evt_C" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("再配信後のwebhook_events(evt_C)=%d want 1", found)
	}
	obx, err := q.ListOutboxEvents(ctx, 10)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if len(obx) != 2 { // 事前シード1件 + 再配信での登録1件
		t.Fatalf("outbox_events=%d want 2", len(obx))
	}
}

func TestWebhook_Redelivery_Idempotent(t *testing.T) {
	pool := requirePG(t)
	q := sqlc.New(pool)
	evt := stripe.Event{ID: "evt_B", Type: stripe.EventTypePaymentIntentSucceeded}
	r := newRecv(t, pool, fixedVerify(evt))

	do := func() int {
		req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader([]byte(`{"id":"evt_B"}`)))
		req.Header.Set("Stripe-Signature", "t=1")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != 200 {
		t.Fatalf("1回目 status=%d", code)
	}
	if code := do(); code != 200 {
		t.Fatalf("2回目 status=%d（再配信も200を期待）", code)
	}
	rows, err := q.ListWebhookEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("webhook_events は1件のみのはず: got %d", len(rows))
	}
	obx, err := q.ListOutboxEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	if len(obx) != 1 {
		t.Fatalf("Outboxも1件だけであるべき（重複追記なし）: got %d", len(obx))
	}
}
