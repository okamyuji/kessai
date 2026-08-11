package problem_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/problem"
)

func TestConstructors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		build      func() *problem.Problem
		wantType   problem.TypeURI
		wantStatus int
		wantRetry  bool
	}{
		{"idempotency-conflict", func() *problem.Problem { return problem.IdempotencyConflict("d") }, problem.TypeIdempotencyConflict, 409, false},
		{"idempotency-in-progress", func() *problem.Problem { return problem.IdempotencyInProgress("d") }, problem.TypeIdempotencyInProgress, 409, true},
		{"invalid-transition", func() *problem.Problem { return problem.InvalidTransition("d") }, problem.TypeInvalidTransition, 409, false},
		{"refund-exceeds-amount", func() *problem.Problem { return problem.RefundExceedsAmount("d") }, problem.TypeRefundExceedsAmount, 422, false},
		{"psp-unavailable", func() *problem.Problem { return problem.PSPUnavailable("d") }, problem.TypePSPUnavailable, 503, true},
		{"validation", func() *problem.Problem { return problem.Validation("d") }, problem.TypeValidation, 400, false},
		{"unauthorized", func() *problem.Problem { return problem.Unauthorized("d") }, problem.TypeUnauthorized, 401, false},
		{"internal", func() *problem.Problem { return problem.Internal("d") }, problem.TypeInternal, 500, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := tc.build()
			if p.Type != tc.wantType {
				t.Fatalf("type=%q want %q", p.Type, tc.wantType)
			}
			if p.Status != tc.wantStatus {
				t.Fatalf("status=%d want %d", p.Status, tc.wantStatus)
			}
			if p.Retryable != tc.wantRetry {
				t.Fatalf("retryable=%v want %v", p.Retryable, tc.wantRetry)
			}
			if p.Title == "" {
				t.Fatalf("title 空はNG")
			}
		})
	}
}

func TestWithChain(t *testing.T) {
	t.Parallel()
	p := problem.Validation("bad").
		WithInstance("/v1/pay/01H").
		WithPaymentID("01HZZ").
		WithRetryable(true)
	if p.Instance != "/v1/pay/01H" || p.PaymentID != "01HZZ" || !p.Retryable {
		t.Fatalf("chain設定が反映されていない: %+v", p)
	}
}

func TestWrite_JSONShape(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	p := problem.RefundExceedsAmount("100円 > 90円")
	p.Write(rec, nil)
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Fatalf("Content-Type=%q want %q", got, problem.ContentType)
	}
	if rec.Code != 422 {
		t.Fatalf("status=%d want 422", rec.Code)
	}
	var decoded problem.Problem
	if err := json.NewDecoder(rec.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Type != problem.TypeRefundExceedsAmount {
		t.Fatalf("type=%q want %q", decoded.Type, problem.TypeRefundExceedsAmount)
	}
	if !strings.Contains(decoded.Detail, "100円") {
		t.Fatalf("detail=%q", decoded.Detail)
	}
	if decoded.Retryable {
		t.Fatalf("retryable=true; want false for refund超過")
	}
}
