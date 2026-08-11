package logger_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/logger"
)

func TestRedact_PAN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		gone string
		keep string
	}{
		{"card=4242424242424242 ok", "4242424242424242", "[REDACTED]"},
		{"masked 4242-4242-4242-4242 end", "4242-4242-4242-4242", "[REDACTED]"},
		{"short 12345 should stay", "", "12345"},
	}
	for _, tc := range cases {
		got := logger.Redact(tc.in)
		if tc.gone != "" && strings.Contains(got, tc.gone) {
			t.Fatalf("残存: %q in %q", tc.gone, got)
		}
		if !strings.Contains(got, tc.keep) {
			t.Fatalf("欠落: %q in %q", tc.keep, got)
		}
	}
}

func TestRedact_StripeSecret(t *testing.T) {
	t.Parallel()
	in := "using " + "sk_" + "test_" + "abcdefghijklmnopqrstuvwx and pk_test_x"
	got := logger.Redact(in)
	if strings.Contains(got, "sk_test_"+"abcdefghij") {
		t.Fatalf("Stripeシークレットが残存: %q", got)
	}
	if !strings.Contains(got, "pk_test_x") {
		t.Fatalf("Publishableキーはレダクト対象外: %q", got)
	}
}

func TestLoggerHandler_WithAttrsAndGroup(t *testing.T) {
	// WithAttrs / WithGroup 経由でもレダクトが継続することを確認
	var buf bytes.Buffer
	l := logger.New("debug", &buf).With("secret", "sk_"+"test_"+"abcdefghijklmnop12").WithGroup("g")
	l.Info("hello 4242424242424242 x")
	out := buf.String()
	if strings.Contains(out, "sk_test_"+"abcdefghij") {
		t.Fatalf("With経由でシークレット残存: %s", out)
	}
	if strings.Contains(out, "4242424242424242") {
		t.Fatalf("WithGroup経由でPAN残存: %s", out)
	}
}

func TestLoggerHandler_LevelFilter(t *testing.T) {
	// warn以下は出力されないことを確認しつつ、Enabledのカバレッジを補完
	var buf bytes.Buffer
	l := logger.New("warn", &buf)
	l.Info("info msg")
	if buf.Len() != 0 {
		t.Fatalf("warn未満で出力された: %s", buf.String())
	}
	l.Error("err msg")
	if buf.Len() == 0 {
		t.Fatalf("error出力がされない")
	}
}

func TestLoggerHandler_RedactsMessageAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := logger.New("info", &buf)
	l.Info("charge with 4242424242424242 card", "secret", "sk_"+"live_"+"1234567890abcdef1234")
	out := buf.String()
	if strings.Contains(out, "4242424242424242") {
		t.Fatalf("メッセージ内のPANが残存: %s", out)
	}
	if strings.Contains(out, "sk_"+"live_"+"1234567890abcdef1234") {
		t.Fatalf("属性内のStripeシークレットが残存: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("REDACTEDマーカーが無い: %s", out)
	}
}
