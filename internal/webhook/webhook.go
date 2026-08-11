// Package webhook StripeからのWebhook受信・署名検証・冪等記録を担当します（FR-B2）。
// - 生ボディで署名検証
// - stripe_event_id の一意制約で at-least-once 配信を冪等化
// - 重い処理はOutbox経由でワーカーへ回す（本ハンドラでは即200を返す）
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	stripe "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/problem"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// Receiver Webhookハンドラの依存
type Receiver struct {
	Logger        *slog.Logger
	Queries       *sqlc.Queries
	IDGen         idgen.Generator
	SigningSecret string
	Verify        func(payload []byte, header, secret string) (stripe.Event, error)
}

// New Verifyに stripe-go の ConstructEvent を注入した Receiver を返します。
// テストではVerifyを差し替えて署名検証をスキップ・ダミー化できます。
func New(logger *slog.Logger, q *sqlc.Queries, ids idgen.Generator, secret string) *Receiver {
	return &Receiver{
		Logger:        logger,
		Queries:       q,
		IDGen:         ids,
		SigningSecret: secret,
		Verify:        webhook.ConstructEvent,
	}
}

// ServeHTTP Stripe Webhookエンドポイント。生ボディで署名を検証し、
// stripe_event_id の一意制約で再配信を冪等化し、Outboxへ処理を委譲します。
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		problem.Validation("body読取失敗").Write(w, r.Logger)
		return
	}
	sig := req.Header.Get("Stripe-Signature")
	evt, err := r.Verify(body, sig, r.SigningSecret)
	if err != nil {
		r.Logger.Warn("webhook署名検証失敗", "err", err)
		problem.Validation("署名不正").Write(w, r.Logger)
		return
	}
	if err := r.record(req.Context(), evt, body); err != nil {
		r.Logger.Error("webhook記録失敗", "err", err, "stripe_event_id", evt.ID)
		problem.Internal("記録失敗").Write(w, r.Logger)
		return
	}
	// Stripeは200-299を成功と判定する。同期処理はここで打ち切り、後続はOutboxで実行。
	w.WriteHeader(http.StatusOK)
}

// record webhook_eventsへ挿入し、成功時はOutboxへ処理イベントを追記します。
// 再配信はstripe_event_id一意制約で0行になり、pgx.ErrNoRowsを返します。
func (r *Receiver) record(ctx context.Context, evt stripe.Event, raw []byte) error {
	// jsonb格納用にpayloadは既にJSONバイト列（raw body）
	inserted, err := r.Queries.TryInsertWebhookEvent(ctx, sqlc.TryInsertWebhookEventParams{
		ID:            r.IDGen.New(),
		StripeEventID: evt.ID,
		EventType:     string(evt.Type),
		Payload:       json.RawMessage(raw),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 再配信。処理済みなので成功扱い
			r.Logger.Debug("webhook再配信をスキップ", "stripe_event_id", evt.ID)
			return nil
		}
		return fmt.Errorf("TryInsertWebhookEvent: %w", err)
	}
	// Outboxに処理イベントを積む。ペイロードは受信IDのみ（詳細はwebhook_eventsから引く）
	payload, _ := json.Marshal(map[string]string{
		"webhook_event_id": inserted.ID,
		"stripe_event_id":  evt.ID,
	})
	if _, err := r.Queries.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID:        r.IDGen.New(),
		EventType: string(evt.Type),
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("EnqueueOutboxEvent: %w", err)
	}
	return nil
}
