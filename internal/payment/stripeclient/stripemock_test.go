package stripeclient_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v86"

	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/breaker"
	"github.com/okamyuji/kessai/internal/platform/money"
)

// stripe-mock 統合テスト。docker composeでstripe-mockを起動してから実行します。
// KESSAI_STRIPE_MOCK_URL 未設定ならスキップします。
func stripeMockURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KESSAI_STRIPE_MOCK_URL")
	if url == "" {
		t.Skip("KESSAI_STRIPE_MOCK_URL 未設定のためスキップ")
	}
	// 疎通確認
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/v1/customers") // #nosec G704,G107 -- テスト用のstripe-mock（環境変数指定）
	if err != nil {
		t.Skipf("stripe-mock未起動 (%v)", err)
	}
	_ = resp.Body.Close()
	return url
}

func TestStripeMock_CreatePaymentIntent(t *testing.T) {
	url := stripeMockURL(t)
	backends := &stripe.Backends{
		API: stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(url)}),
	}
	br := breaker.New(breaker.DefaultConfig())
	client := stripeclient.NewWithBackend("sk_test_dummy", backends, br)
	intent, err := client.CreatePaymentIntent(context.Background(), stripeclient.CreateIntentRequest{
		IdempotencyMaster: "master_test_1",
		Amount:            money.MustNew(1000),
		CaptureMode:       "manual",
		Description:       "stripe-mock integration",
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent: %v", err)
	}
	if intent.ID == "" || intent.ClientSecret == "" {
		t.Fatalf("intent=%+v", intent)
	}
}
