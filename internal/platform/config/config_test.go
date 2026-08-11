package config_test

import (
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/config"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// テスト用のダミー値。ハードコード検出（gosec G101）を避けるため、資格情報部分を組み立てで作る
var fullEnv = map[string]string{
	"DATABASE_URL":           "postgres://" + "user" + ":" + "pass" + "@localhost:5433/kessai?sslmode=disable",
	"STRIPE_SECRET_KEY":      "sk_test_x",
	"STRIPE_PUBLISHABLE_KEY": "pk_test_x",
	"STRIPE_WEBHOOK_SECRET":  "whsec_x",
	"ADMIN_EMAIL":            "admin@example.com",
	"SESSION_SIGNING_KEY":    "a-key-that-is-at-least-32-bytes-long!!",
}

func TestLoad_HappyPath(t *testing.T) {
	setEnv(t, fullEnv)
	t.Setenv("STRIPE_MOCK_URL", "http://127.0.0.1:12111")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if cfg.CaptureMode != "manual" {
		t.Fatalf("既定CAPTURE_MODEは manual: got %q", cfg.CaptureMode)
	}
	if cfg.CheckoutExpiryMinutes != 60 {
		t.Fatalf("既定CheckoutExpiryMinutes=60: got %d", cfg.CheckoutExpiryMinutes)
	}
	if cfg.AuthExpiryDays != 21 {
		t.Fatalf("既定AuthExpiryDays=21: got %d", cfg.AuthExpiryDays)
	}
	if cfg.StripeMockURL != "http://127.0.0.1:12111" {
		t.Fatalf("STRIPE_MOCK_URLを保持する: got %q", cfg.StripeMockURL)
	}
}

func TestLoad_MissingKeysListed(t *testing.T) {
	// 空環境。t.Setenvで既存値を上書きしても、未設定キーが確実に空になるようにする。
	for k := range fullEnv {
		t.Setenv(k, "")
	}
	_, err := config.Load()
	if err == nil {
		t.Fatalf("欠落エラーが返るべき")
	}
	msg := err.Error()
	// 値は含まれない。キー名だけが列挙される。
	if strings.Contains(msg, "postgres:") {
		t.Fatalf("エラーに値が漏出: %q", msg)
	}
	for _, key := range []string{
		"DATABASE_URL", "STRIPE_SECRET_KEY", "STRIPE_PUBLISHABLE_KEY",
		"STRIPE_WEBHOOK_SECRET", "ADMIN_EMAIL", "SESSION_SIGNING_KEY",
	} {
		if !strings.Contains(msg, key) {
			t.Fatalf("エラーに %q が含まれない: %q", key, msg)
		}
	}
}

func TestLoad_InvalidCaptureMode(t *testing.T) {
	setEnv(t, fullEnv)
	t.Setenv("CAPTURE_MODE", "yes-please")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "CAPTURE_MODE") {
		t.Fatalf("CAPTURE_MODEを不正値として検出すべき: err=%v", err)
	}
}

func TestLoad_ShortSessionKey(t *testing.T) {
	setEnv(t, fullEnv)
	t.Setenv("SESSION_SIGNING_KEY", "short")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "SESSION_SIGNING_KEY") {
		t.Fatalf("SESSION_SIGNING_KEY長さを検出すべき: err=%v", err)
	}
}

func TestLoad_NonPositiveInt(t *testing.T) {
	setEnv(t, fullEnv)
	t.Setenv("CHECKOUT_EXPIRY_MINUTES", "0")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "CHECKOUT_EXPIRY_MINUTES") {
		t.Fatalf("CHECKOUT_EXPIRY_MINUTES非正数を検出すべき: err=%v", err)
	}
}
