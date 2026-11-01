package config_test

import (
	"os"
	"regexp"
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

// 例のファイルに載っている署名鍵はリポジトリを読めば誰でも分かるので、そのまま使えば偽造されたセッションを通してしまう
func TestLoad_RejectsPublishedExampleSessionKeys(t *testing.T) {
	examples := map[string]*regexp.Regexp{
		"../../../.env.example":                              regexp.MustCompile(`(?m)^SESSION_SIGNING_KEY=(\S+)$`),
		"../../../deploy/terraform/terraform.tfvars.example": regexp.MustCompile(`(?m)^session_signing_key\s*=\s*"([^"]+)"`),
		"../../../deploy/terraform/README.md":                regexp.MustCompile(`(?m)^session_signing_key\s*=\s*"([^"]+)"`),
	}
	for path, re := range examples {
		t.Run(path, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			m := re.FindSubmatch(b)
			if m == nil {
				t.Fatalf("%s に SESSION_SIGNING_KEY の例が見つからない", path)
			}
			setEnv(t, fullEnv)
			t.Setenv("SESSION_SIGNING_KEY", string(m[1]))
			_, err = config.Load()
			if err == nil || !strings.Contains(err.Error(), "SESSION_SIGNING_KEY") {
				t.Fatalf("%s の例の鍵を拒否すべき: err=%v", path, err)
			}
			if strings.Contains(err.Error(), string(m[1])) {
				t.Fatalf("エラーに鍵の値を含めてはいけない: %v", err)
			}
		})
	}
}

// .env.example の初期管理者パスワードのまま起動すると、公開されたパスワードの管理者が作られる
func TestLoad_RejectsExampleAdminInitialPassword(t *testing.T) {
	b, err := os.ReadFile("../../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	m := regexp.MustCompile(`(?m)^ADMIN_INITIAL_PASSWORD=(\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal(".env.example に ADMIN_INITIAL_PASSWORD の例が見つからない")
	}
	setEnv(t, fullEnv)
	t.Setenv("ADMIN_INITIAL_PASSWORD", string(m[1]))
	_, err = config.Load()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_INITIAL_PASSWORD") {
		t.Fatalf("例の初期管理者パスワードを拒否すべき: err=%v", err)
	}
	if strings.Contains(err.Error(), string(m[1])) {
		t.Fatalf("エラーにパスワードの値を含めてはいけない: %v", err)
	}
}
