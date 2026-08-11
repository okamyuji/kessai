// Package config は環境変数から設定を読み込み、必須項目の欠落をアプリ起動時に検出します（FR-D4）。
// エラーメッセージには値を含めず、欠落したキー名だけを表示します。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config アプリの全設定を保持する不変値です。
type Config struct {
	DatabaseURL           string
	HTTPAddr              string
	LogLevel              string
	StripeSecretKey       string
	StripePublishableKey  string
	StripeWebhookSecret   string
	CaptureMode           string // "manual" or "auto"
	CheckoutExpiryMinutes int
	AuthExpiryDays        int
	AdminEmail            string
	AdminInitialPassword  string // 初期作成後は削除可能
	SessionSigningKey     string
}

// Load 環境変数から設定を読み込み、必須項目の欠落や不正値をまとめて返します。
// 値そのものはエラーに含めません。
func Load() (Config, error) {
	var missing []string
	var invalid []string

	get := func(key string, required bool) string {
		v := strings.TrimSpace(os.Getenv(key))
		if required && v == "" {
			missing = append(missing, key)
		}
		return v
	}

	getInt := func(key string, def int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			invalid = append(invalid, key)
			return def
		}
		return n
	}

	cfg := Config{
		DatabaseURL:           get("DATABASE_URL", true),
		HTTPAddr:              firstNonEmpty(get("HTTP_ADDR", false), "127.0.0.1:8080"),
		LogLevel:              firstNonEmpty(get("LOG_LEVEL", false), "info"),
		StripeSecretKey:       get("STRIPE_SECRET_KEY", true),
		StripePublishableKey:  get("STRIPE_PUBLISHABLE_KEY", true),
		StripeWebhookSecret:   get("STRIPE_WEBHOOK_SECRET", true),
		CaptureMode:           firstNonEmpty(get("CAPTURE_MODE", false), "manual"),
		CheckoutExpiryMinutes: getInt("CHECKOUT_EXPIRY_MINUTES", 60),
		AuthExpiryDays:        getInt("AUTH_EXPIRY_DAYS", 21),
		AdminEmail:            get("ADMIN_EMAIL", true),
		AdminInitialPassword:  get("ADMIN_INITIAL_PASSWORD", false), // 初期作成時のみ必須
		SessionSigningKey:     get("SESSION_SIGNING_KEY", true),
	}

	if cfg.CaptureMode != "manual" && cfg.CaptureMode != "auto" {
		invalid = append(invalid, "CAPTURE_MODE")
	}
	if len(cfg.SessionSigningKey) < 32 {
		invalid = append(invalid, "SESSION_SIGNING_KEY(32文字以上)")
	}

	if len(missing) > 0 || len(invalid) > 0 {
		var b strings.Builder
		b.WriteString("config: ")
		if len(missing) > 0 {
			fmt.Fprintf(&b, "必須欠落 [%s]", strings.Join(missing, ", "))
		}
		if len(invalid) > 0 {
			if b.Len() > len("config: ") {
				b.WriteString(" / ")
			}
			fmt.Fprintf(&b, "不正値 [%s]", strings.Join(invalid, ", "))
		}
		return Config{}, errors.New(b.String())
	}
	return cfg, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
