package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/breaker"
	"github.com/okamyuji/kessai/internal/platform/config"
	"github.com/okamyuji/kessai/internal/platform/money"
)

func TestNewStripeClientUsesConfiguredMockURL(t *testing.T) {
	requests := make(chan string, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"pi_mock","client_secret":"pi_mock_secret","status":"requires_payment_method","amount":1000}`))
	}))
	t.Cleanup(mock.Close)

	client := newStripeClient(config.Config{
		StripeSecretKey: "not-a-real-key",
		StripeMockURL:   mock.URL,
	}, breaker.New(breaker.DefaultConfig()))
	intent, err := client.CreatePaymentIntent(context.Background(), stripeclient.CreateIntentRequest{
		IdempotencyMaster: "master_mock_url_test",
		Amount:            money.MustNew(1000),
		CaptureMode:       "manual",
		Description:       "mock URL test",
	})
	if err != nil {
		t.Fatalf("CreatePaymentIntent: %v", err)
	}
	if request := <-requests; request != "POST /v1/payment_intents" {
		t.Fatalf("request = %q", request)
	}
	if intent.ID != "pi_mock" || intent.ClientSecret != "pi_mock_secret" {
		t.Fatalf("intent = %+v", intent)
	}
}
