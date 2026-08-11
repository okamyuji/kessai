package payment_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/money"
)

// TestStartCheckout_ValidationErrors validate段階で早期returnする経路のみを検証します
// （実DB・実Stripeに到達する経路は integration_test.go で扱います）。
func TestStartCheckout_ValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(r *payment.StartCheckoutRequest)
		want   string
	}{
		{"idempotency必須", func(r *payment.StartCheckoutRequest) { r.IdempotencyKey = "" }, "idempotency_key必須"},
		{"product必須", func(r *payment.StartCheckoutRequest) { r.ProductID = "" }, "product_id必須"},
		{"amount非正", func(r *payment.StartCheckoutRequest) { r.Amount = money.MustNew(0) }, "amountは正の整数"},
		{"capture不明", func(r *payment.StartCheckoutRequest) { r.CaptureMode = "sometimes" }, "capture_mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := payment.StartCheckoutRequest{
				IdempotencyKey: "K1", ProductID: "P1",
				Amount: money.MustNew(1000), CaptureMode: "manual",
			}
			tc.mutate(&req)
			uc := payment.NewUseCase(nil, nil, nil, "manual", time.Hour)
			_, err := uc.StartCheckout(context.Background(), req)
			if err == nil {
				t.Fatalf("エラーが返るべき")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%q want含む %q", err.Error(), tc.want)
			}
		})
	}
}

// Client型シグネチャの互換確認用（コンパイル時アサート）
var _ stripeclient.Client = (*stubStripe)(nil)

type stubStripe struct{}

func (*stubStripe) CreatePaymentIntent(context.Context, stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (*stubStripe) CapturePaymentIntent(context.Context, stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (*stubStripe) Refund(context.Context, stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	return nil, nil
}
