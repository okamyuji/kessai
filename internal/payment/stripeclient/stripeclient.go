// Package stripeclient Stripe APIとの薄いアダプタです。
// - Circuit Breakerで外部呼び出しを保護（ADR-0010）
// - 冪等性キーはStripeのIdempotency-Keyヘッダに引き継ぎ（ADR-0007）
// - stripe-go v86 の新API（stripe.NewClient + V1系サービス）を使います
// カード情報は取り扱いません。PaymentIntent作成のみで、確認はブラウザ側のStripe.jsが行います。
package stripeclient

import (
	"context"
	"errors"
	"fmt"

	stripe "github.com/stripe/stripe-go/v86"

	"github.com/okamyuji/kessai/internal/payment/idempotency"
	"github.com/okamyuji/kessai/internal/platform/breaker"
	"github.com/okamyuji/kessai/internal/platform/money"
)

// Client Stripe操作の公開インターフェース。テストでは差し替え可能にします。
type Client interface {
	CreatePaymentIntent(ctx context.Context, req CreateIntentRequest) (*Intent, error)
	CapturePaymentIntent(ctx context.Context, req CaptureRequest) (*Intent, error)
	Refund(ctx context.Context, req RefundRequest) (*Refund, error)
}

// CreateIntentRequest PaymentIntent作成に必要な情報
type CreateIntentRequest struct {
	IdempotencyMaster string
	Amount            money.JPY
	CaptureMode       string // "manual" or "auto"
	Description       string
}

// CaptureRequest 手動キャプチャの入力
type CaptureRequest struct {
	IdempotencyMaster string
	PaymentIntentID   string
	Amount            money.JPY
}

// RefundRequest 返金の入力
type RefundRequest struct {
	IdempotencyMaster string
	PaymentIntentID   string
	Amount            money.JPY
	RefundSeq         int
	Reason            string
}

// Intent Stripe PaymentIntentの必要な情報だけを抜き出したビュー
type Intent struct {
	ID           string
	ClientSecret string
	Status       string
	AmountJPY    int64
}

// Refund Stripe Refundの必要な情報だけを抜き出したビュー
type Refund struct {
	ID              string
	Status          string
	AmountJPY       int64
	PaymentIntentID string
}

// ErrPSPUnavailable Circuit BreakerがopenでStripeを呼べない
var ErrPSPUnavailable = errors.New("stripeclient: PSPが一時的に利用不能")

// realClient 実際のstripe-go呼び出し
type realClient struct {
	api *stripe.Client
	br  *breaker.Breaker
}

// New APIキーとBreakerからClientを構築します
func New(secretKey string, br *breaker.Breaker) Client {
	return &realClient{api: stripe.NewClient(secretKey), br: br}
}

// NewWithBackend APIキー・Backends（stripe-mock等のカスタムエンドポイント）・BreakerからClientを構築します
func NewWithBackend(secretKey string, backends *stripe.Backends, br *breaker.Breaker) Client {
	return &realClient{api: stripe.NewClient(secretKey, stripe.WithBackends(backends)), br: br}
}

func (c *realClient) CreatePaymentIntent(ctx context.Context, req CreateIntentRequest) (*Intent, error) {
	permit, err := c.br.Allow()
	if err != nil {
		return nil, ErrPSPUnavailable
	}
	derived, err := idempotency.Derive(req.IdempotencyMaster, idempotency.OpCreate, 0)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: idempotency: %w", err)
	}
	amount := req.Amount.Int64()
	params := &stripe.PaymentIntentCreateParams{
		Amount:             &amount,
		Currency:           stripe.String(string(stripe.CurrencyJPY)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		Description:        stripe.String(req.Description),
	}
	if req.CaptureMode == "manual" {
		params.CaptureMethod = stripe.String(string(stripe.PaymentIntentCaptureMethodManual))
	}
	params.SetIdempotencyKey(derived)
	pi, err := c.api.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: PaymentIntent作成失敗: %w", err)
	}
	permit.Success()
	return intentFromStripe(pi), nil
}

func (c *realClient) CapturePaymentIntent(ctx context.Context, req CaptureRequest) (*Intent, error) {
	permit, err := c.br.Allow()
	if err != nil {
		return nil, ErrPSPUnavailable
	}
	derived, err := idempotency.Derive(req.IdempotencyMaster, idempotency.OpCapture, 0)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: idempotency: %w", err)
	}
	amount := req.Amount.Int64()
	params := &stripe.PaymentIntentCaptureParams{AmountToCapture: &amount}
	params.SetIdempotencyKey(derived)
	pi, err := c.api.V1PaymentIntents.Capture(ctx, req.PaymentIntentID, params)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: Capture失敗: %w", err)
	}
	permit.Success()
	return intentFromStripe(pi), nil
}

func (c *realClient) Refund(ctx context.Context, req RefundRequest) (*Refund, error) {
	permit, err := c.br.Allow()
	if err != nil {
		return nil, ErrPSPUnavailable
	}
	derived, err := idempotency.Derive(req.IdempotencyMaster, idempotency.OpRefund, req.RefundSeq)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: idempotency: %w", err)
	}
	amount := req.Amount.Int64()
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(req.PaymentIntentID),
		Amount:        &amount,
	}
	if req.Reason != "" {
		params.Reason = stripe.String(req.Reason)
	}
	params.SetIdempotencyKey(derived)
	rf, err := c.api.V1Refunds.Create(ctx, params)
	if err != nil {
		permit.Failure()
		return nil, fmt.Errorf("stripeclient: Refund失敗: %w", err)
	}
	permit.Success()
	return &Refund{
		ID:              rf.ID,
		Status:          string(rf.Status),
		AmountJPY:       rf.Amount,
		PaymentIntentID: piIDFrom(rf.PaymentIntent),
	}, nil
}

func intentFromStripe(pi *stripe.PaymentIntent) *Intent {
	if pi == nil {
		return nil
	}
	return &Intent{
		ID:           pi.ID,
		ClientSecret: pi.ClientSecret,
		Status:       string(pi.Status),
		AmountJPY:    pi.Amount,
	}
}

func piIDFrom(pi *stripe.PaymentIntent) string {
	if pi == nil {
		return ""
	}
	return pi.ID
}
