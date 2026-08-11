package templates_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/web/templates"
)

func TestConfirmPageInStripeMockModeDoesNotStartStripeJS(t *testing.T) {
	var body bytes.Buffer
	err := templates.ConfirmPage(
		"pk_test_dummy",
		"pi_mock_secret",
		"payment-id",
		templates.ProductView{ID: "product-id", Name: "デモ", Price: money.MustNew(1000)},
		templates.TokushoSnapshot{},
		true,
	).Render(context.Background(), &body)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	html := body.String()
	if !strings.Contains(html, "Stripeモック接続") {
		t.Fatalf("モック接続の説明が無い: %q", html)
	}
	if strings.Contains(html, "stripe-init.js") || strings.Contains(html, "id=\"payment-element\"") {
		t.Fatalf("モック接続でStripe.jsの決済入力を開始している: %q", html)
	}
}
