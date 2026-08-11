package idempotency_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/payment/idempotency"
)

func TestDerive_Create(t *testing.T) {
	t.Parallel()
	k, err := idempotency.Derive("01H", idempotency.OpCreate, 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if k != "01H:create" {
		t.Fatalf("k=%q", k)
	}
}

func TestDerive_Capture(t *testing.T) {
	t.Parallel()
	k, _ := idempotency.Derive("01H", idempotency.OpCapture, 0)
	if k != "01H:capture" {
		t.Fatalf("k=%q", k)
	}
}

func TestDerive_Refund_RequiresSeq(t *testing.T) {
	t.Parallel()
	if _, err := idempotency.Derive("01H", idempotency.OpRefund, 0); !errors.Is(err, idempotency.ErrInvalidRefundSeq) {
		t.Fatalf("refund seq=0 err=%v want ErrInvalidRefundSeq", err)
	}
	k, err := idempotency.Derive("01H", idempotency.OpRefund, 3)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if k != "01H:refund:3" {
		t.Fatalf("k=%q", k)
	}
}

func TestDerive_EmptyMaster(t *testing.T) {
	t.Parallel()
	if _, err := idempotency.Derive("", idempotency.OpCreate, 0); !errors.Is(err, idempotency.ErrInvalidMasterKey) {
		t.Fatalf("empty master err=%v", err)
	}
}

func TestDerive_UnknownOp(t *testing.T) {
	t.Parallel()
	if _, err := idempotency.Derive("01H", "explode", 0); err == nil {
		t.Fatalf("未対応の操作を検出すべき")
	}
}

func TestHashRequest_Stable(t *testing.T) {
	t.Parallel()
	body := []byte(`{"amount":1000,"product":"demo"}`)
	h1 := idempotency.HashRequest(body)
	h2 := idempotency.HashRequest(body)
	if !idempotency.EqualHash(h1, h2) {
		t.Fatalf("同一入力のハッシュが不一致")
	}
	hex1 := idempotency.HashRequestHex(body)
	if len(hex1) != 64 {
		t.Fatalf("SHA-256の16進は64文字であるべき: %d", len(hex1))
	}
}

func TestHashRequest_DifferentBodies(t *testing.T) {
	t.Parallel()
	if idempotency.EqualHash(idempotency.HashRequest([]byte("a")), idempotency.HashRequest([]byte("b"))) {
		t.Fatalf("異なる入力のハッシュが一致してはならない")
	}
}

func TestEqualHash_LengthMismatch(t *testing.T) {
	t.Parallel()
	if idempotency.EqualHash([]byte{1, 2}, []byte{1, 2, 3}) {
		t.Fatalf("長さ違いはfalse")
	}
}

func TestNormalizeKey(t *testing.T) {
	t.Parallel()
	k, err := idempotency.NormalizeKey("  01H  ")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if k != "01H" {
		t.Fatalf("k=%q want 01H", k)
	}
	if _, err := idempotency.NormalizeKey("   "); !errors.Is(err, idempotency.ErrInvalidMasterKey) {
		t.Fatalf("空白のみはErrInvalidMasterKey")
	}
	if strings.Contains(k, " ") {
		t.Fatalf("空白が残っている")
	}
}
