// Package outboxhandler Outboxイベントを状態遷移と複式簿記記帳に変換します。
// Stripeイベント種別と遷移イベントの対応表は docs/03-basic-design.md 2.1.1節に定義。
package outboxhandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/okamyuji/kessai/internal/ledger"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/statemachine"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// Handler outbox.Handler として使えるハンドラを生成します。
type Handler struct {
	Queries *sqlc.Queries
	UseCase *payment.UseCase
	Store   payment.Store
	IDs     idgen.Generator
	Logger  *slog.Logger
}

// New Handlerを構築します
func New(q *sqlc.Queries, uc *payment.UseCase, store payment.Store, ids idgen.Generator, logger *slog.Logger) *Handler {
	return &Handler{Queries: q, UseCase: uc, Store: store, IDs: ids, Logger: logger}
}

// Handle 1件のOutboxイベントを処理します。outbox.Relayが呼び出します。
// event_type はStripeイベント種別（例: payment_intent.succeeded）、
// payload は {"webhook_event_id":..., "stripe_event_id":...} のJSON。
func (h *Handler) Handle(ctx context.Context, tx pgx.Tx, evt sqlc.FetchPendingOutboxRow) error {
	var meta struct {
		WebhookEventID string `json:"webhook_event_id"`
		StripeEventID  string `json:"stripe_event_id"`
	}
	if err := json.Unmarshal(evt.Payload, &meta); err != nil {
		return fmt.Errorf("outboxhandler: payload parse: %w", err)
	}
	whRow, err := h.Queries.WithTx(tx).ListWebhookEvents(ctx, 1000) // 少数件想定
	if err != nil {
		return fmt.Errorf("list webhook: %w", err)
	}
	var payment map[string]any
	for _, r := range whRow {
		if r.StripeEventID == meta.StripeEventID {
			if err := json.Unmarshal(r.Payload, &payment); err != nil {
				return fmt.Errorf("payload: %w", err)
			}
			break
		}
	}
	if payment == nil {
		return errors.New("outboxhandler: 対応するwebhook_event未検出")
	}
	// stripe event.data.object.id, event.data.object.amount 等を安全に抜く
	piID, amount, err := extractIntent(payment)
	if err != nil {
		return err
	}
	// PaymentIntent ID → 自社の payment_id
	rows, err := h.queryPaymentIDByStripe(ctx, tx, piID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		// 未知のPaymentIntent。冪等化のため成功扱いにするか、無視するかは運用判断。
		h.Logger.Warn("outboxhandler: 未知のPaymentIntent", "pi", piID)
		return nil
	}
	paymentID := rows[0]
	return h.applyByEventType(ctx, tx, paymentID, evt.EventType, amount)
}

// applyByEventType Stripeイベント種別ごとに遷移＋記帳
func (h *Handler) applyByEventType(ctx context.Context, tx pgx.Tx, paymentID, stripeType string, amount int64) error {
	txStore := &pgxTxStore{tx: tx, q: h.Queries.WithTx(tx)}
	switch stripeType {
	case "payment_intent.amount_capturable_updated":
		_, err := h.UseCase.ApplyEvent(ctx, txStore, paymentID, "webhook", statemachine.EventAuthorizeSucceeded)
		return err
	case "payment_intent.succeeded":
		// createdからの場合は AuthorizeSucceeded → Capture の連続適用
		if _, err := h.UseCase.ApplyEvent(ctx, txStore, paymentID, "webhook", statemachine.EventAuthorizeSucceeded); err != nil {
			if !errors.Is(err, statemachine.ErrInvalidTransition) {
				return err
			}
		}
		if _, err := h.UseCase.ApplyEvent(ctx, txStore, paymentID, "webhook", statemachine.EventCapture); err != nil {
			if !errors.Is(err, statemachine.ErrInvalidTransition) {
				return err
			}
		}
		return h.writeCaptureLedger(ctx, tx, paymentID, amount)
	case "payment_intent.payment_failed":
		_, err := h.UseCase.ApplyEvent(ctx, txStore, paymentID, "webhook", statemachine.EventAuthorizeFailed)
		return err
	case "payment_intent.canceled":
		_, err := h.UseCase.ApplyEvent(ctx, txStore, paymentID, "webhook", statemachine.EventCancel)
		return err
	}
	// 未対応イベントは成功扱い（Outboxに残さない）
	h.Logger.Debug("outboxhandler: 未対応イベント", "type", stripeType)
	return nil
}

// writeCaptureLedger キャプチャに対応する複式簿記エントリを1組追記
func (h *Handler) writeCaptureLedger(ctx context.Context, tx pgx.Tx, paymentID string, amountJPY int64) error {
	if amountJPY <= 0 {
		return nil
	}
	amt, err := money.New(amountJPY)
	if err != nil {
		return err
	}
	// 連番は現状1固定（複数キャプチャ未対応、部分キャプチャなし）
	transfer, err := ledger.BuildCaptureTransfer(paymentID, 1, amt)
	if err != nil {
		return err
	}
	qTx := h.Queries.WithTx(tx)
	for _, e := range []ledger.Entry{transfer.Debit, transfer.Credit} {
		if err := qTx.InsertLedgerEntry(ctx, sqlc.InsertLedgerEntryParams{
			ID:         h.IDs.New(),
			TransferID: e.TransferID,
			Account:    sqlc.LedgerAccount(e.Account),
			Side:       sqlc.LedgerSide(e.Side),
			AmountJpy:  e.Amount.Int64(),
			PaymentID:  e.PaymentID,
		}); err != nil {
			return fmt.Errorf("insert ledger: %w", err)
		}
	}
	return nil
}

// queryPaymentIDByStripe stripe_payment_intent_idから自社payment_idを探す
func (h *Handler) queryPaymentIDByStripe(ctx context.Context, tx pgx.Tx, piID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM payments WHERE stripe_payment_intent_id = $1 LIMIT 1`, piID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func extractIntent(payload map[string]any) (piID string, amount int64, err error) {
	// Stripeイベント: {"data":{"object":{"id":"pi_x","amount":1000}}}
	data, _ := payload["data"].(map[string]any)
	obj, _ := data["object"].(map[string]any)
	id, _ := obj["id"].(string)
	if id == "" {
		return "", 0, errors.New("outboxhandler: PaymentIntent ID未検出")
	}
	amt := int64(0)
	switch v := obj["amount"].(type) {
	case float64:
		amt = int64(v)
	case int64:
		amt = v
	}
	return id, amt, nil
}

// pgxTxStore payment.TxStore のうち Handler で使うのは Queries() のみ。
// Commit/Rollback は呼び出さない（親のTxで管理）。
type pgxTxStore struct {
	tx pgx.Tx
	q  *sqlc.Queries
}

func (t *pgxTxStore) Queries() *sqlc.Queries           { return t.q }
func (t *pgxTxStore) Commit(_ context.Context) error   { return nil }
func (t *pgxTxStore) Rollback(_ context.Context) error { return nil }

// _ payment.TxStore実装の証明
var _ payment.TxStore = (*pgxTxStore)(nil)
