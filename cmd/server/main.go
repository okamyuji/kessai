// kessai HTTPサーバのエントリポイントです。
// 起動時に環境変数から設定を読み込み、DB接続・Stripeクライアント・HTTPルータを組み立てて listen します。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	stripe "github.com/stripe/stripe-go/v86"

	"github.com/okamyuji/kessai/internal/admin"
	"github.com/okamyuji/kessai/internal/housekeeping"
	"github.com/okamyuji/kessai/internal/httpx"
	"github.com/okamyuji/kessai/internal/outbox"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/outboxhandler"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/breaker"
	"github.com/okamyuji/kessai/internal/platform/config"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/logger"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/webhook"
	"github.com/okamyuji/kessai/web/templates"
)

func main() {
	// .envがあればロード（本番相当ではシークレットマネージャで注入する想定）
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("設定読込失敗: %v", err)
	}
	lg := logger.New(cfg.LogLevel, os.Stdout)

	// 起動時にマイグレーション適用（ECS/K8s共通で使える方式）
	if err := runMigrations(cfg.DatabaseURL, lg); err != nil {
		lg.Error("マイグレーション失敗", "err", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		lg.Error("DB接続失敗", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	br := breaker.New(breaker.DefaultConfig())
	sc := newStripeClient(cfg, br)
	uc := payment.NewUseCase(store, sc, idgen.NewDefault(), cfg.CaptureMode, time.Hour)

	tokusho := templates.TokushoSnapshot{
		Portion:              "デジタルコンテンツ1点",
		PriceWithShipping:    "商品ページに表示（送料無料）",
		PaymentTiming:        "クレジットカード（購入手続き時に決済）",
		DeliveryTiming:       "決済完了と同時",
		ApplicationPeriod:    "",
		WithdrawalAndReturns: "商品の性質上、購入後のキャンセルは受け付けません",
	}

	// SSEハブ
	sseHub := httpx.NewSSEHub(lg, 32)

	// Webhook受信
	webhookRecv := webhook.New(lg, pool, q, idgen.NewDefault(), cfg.StripeWebhookSecret)

	// Admin ハンドラ
	adminDeps := &admin.Deps{
		Logger: lg, Queries: q, Sessions: admin.NewPGSessionStore(q, idgen.NewDefault()),
		IDs: idgen.NewDefault(), Stripe: sc, UseCase: uc, Store: store,
		LoginLimiter: admin.NewRateLimiter(15*time.Minute, 5),
		IPLimiter:    admin.NewRateLimiter(15*time.Minute, 20),
		SessionTTL:   12 * time.Hour, SecureCookie: os.Getenv("KESSAI_INSECURE_COOKIE") == "",
	}
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /login", adminDeps.LoginForm)
	adminMux.HandleFunc("POST /login", adminDeps.Login)
	adminMux.HandleFunc("POST /logout", adminDeps.Logout)
	adminMux.Handle("GET /", adminDeps.RequireAuth(http.HandlerFunc(adminDeps.Dashboard)))
	adminMux.Handle("POST /payments/{payment_id}/refund", adminDeps.RequireAuth(http.HandlerFunc(adminDeps.Refund)))
	adminMux.Handle("POST /payments/{payment_id}/capture", adminDeps.RequireAuth(http.HandlerFunc(adminDeps.Capture)))

	deps := httpx.CheckoutDeps{
		Logger: lg, Queries: q, UseCase: uc, IDGen: idgen.NewDefault(),
		PubKey: cfg.StripePublishableKey, StripeMock: cfg.StripeMockURL != "", Tokusho: tokusho,
		WebhookHandler: webhookRecv, AdminMux: adminMux, SSE: sseHub,
	}
	secure := os.Getenv("KESSAI_INSECURE_COOKIE") == ""
	handler := httpx.NewMux(deps, []byte(cfg.SessionSigningKey), secure, "web/static")

	// Outbox リレー・housekeeping を goroutine で起動
	obxHandler := outboxhandler.New(q, uc, store, idgen.NewDefault(), lg)
	relay := outbox.New(pool, q, func(ctx context.Context, tx pgx.Tx, evt sqlc.FetchPendingOutboxRow) error {
		err := obxHandler.Handle(ctx, tx, evt)
		if err == nil {
			// 状態更新完了をSSEへ通知（購読者は管理画面のイベントログ）
			payload, _ := json.Marshal(map[string]any{"type": evt.EventType, "id": evt.ID})
			sseHub.Publish("outbox.processed", string(payload))
		}
		return err
	}, lg)
	hkRunner := housekeeping.New(q, &housekeeping.PoolAdapter{Pool: pool}, idgen.NewDefault(), lg,
		housekeeping.ExpiryPolicy{CheckoutExpiryMinutes: cfg.CheckoutExpiryMinutes, AuthExpiryDays: cfg.AuthExpiryDays})

	bgCtx, bgCancel := context.WithCancel(context.Background())
	go relay.Loop(bgCtx, 2*time.Second)
	go func() {
		tick := time.NewTicker(1 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-tick.C:
				if res, err := hkRunner.RunOnce(bgCtx); err != nil {
					lg.Error("housekeeping", "err", err)
				} else if res.ExpiredCreated+res.ExpiredAuthorized+res.DeletedIdempotency > 0 {
					lg.Info("housekeeping", "result", res)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 起動
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		lg.Info("listen", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("listen失敗", "err", err)
			shutdownCh <- syscall.SIGTERM
		}
	}()
	<-shutdownCh

	// グレースフルシャットダウン
	bgCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		lg.Error("shutdown失敗", "err", err)
	}
	lg.Info("stopped")
}

// runMigrations db/migrations をfile://で適用します。冪等（既適用ならNoChange）。
// ECS/コンテナ環境用にworking directory相対とバイナリ相対の両方を試します。
func runMigrations(dsn string, lg interface {
	Info(msg string, args ...any)
}) error {
	// 優先順位: (1) 環境変数 KESSAI_MIGRATIONS_PATH、(2) /app/db/migrations（distrolessコンテナ内）、(3) db/migrations（開発時）
	candidates := []string{"db/migrations", "/app/db/migrations"}
	if p := os.Getenv("KESSAI_MIGRATIONS_PATH"); p != "" {
		candidates = append([]string{p}, candidates...)
	}
	var lastErr error
	for _, path := range candidates {
		path = filepath.Clean(path)
		// #nosec G703 -- 候補パスは固定値と運用者が設定する環境変数のみで、外部入力を受けない
		if _, err := os.Stat(path); err != nil {
			lastErr = err
			continue
		}
		m, err := migrate.New("file://"+path, migrateDSN(dsn))
		if err != nil {
			lastErr = err
			continue
		}
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		lg.Info("migrations applied", "path", path)
		return nil
	}
	return lastErr
}

// migrateDSN pgx形式 postgres://... はそのままgolang-migrateのpostgresドライバでも動きます
func migrateDSN(dsn string) string { return dsn }

func newStripeClient(cfg config.Config, br *breaker.Breaker) stripeclient.Client {
	if cfg.StripeMockURL == "" {
		return stripeclient.New(cfg.StripeSecretKey, br)
	}
	backends := &stripe.Backends{
		API: stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
			URL: stripe.String(cfg.StripeMockURL),
		}),
	}
	return stripeclient.NewWithBackend(cfg.StripeSecretKey, backends, br)
}
