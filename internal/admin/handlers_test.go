package admin_test

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/admin"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
)

var sharedPG *pgcontainer.Container

// newPOST フォームPOSTリクエストヘルパ
func newPOST(t *testing.T, target, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// recordAndCall ResponseRecorderでハンドラを呼ぶ
func recordAndCall(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h(rr, r)
	return rr
}

func TestMain(m *testing.M) {
	if os.Getenv("KESSAI_SKIP_INTEGRATION") != "" {
		os.Exit(m.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgcontainer.Start(ctx)
	if err != nil {
		log.Printf("pgcontainer未起動: %v", err)
		os.Exit(m.Run())
	}
	sharedPG = c
	code := m.Run()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	_ = sharedPG.Stop(stopCtx)
	os.Exit(code)
}

func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if sharedPG == nil {
		t.Skip("testcontainers未起動")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

// stubStripeRefund 返金だけスタブする
type stubStripeRefund struct {
	err error
}

func (*stubStripeRefund) CreatePaymentIntent(context.Context, stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	return &stripeclient.Intent{ID: "pi_test", ClientSecret: "cs_test", Status: "requires_confirmation"}, nil
}
func (*stubStripeRefund) CapturePaymentIntent(context.Context, stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (s *stubStripeRefund) Refund(_ context.Context, req stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &stripeclient.Refund{ID: "re_" + req.IdempotencyMaster, Status: "succeeded", AmountJPY: req.Amount.Int64(), PaymentIntentID: req.PaymentIntentID}, nil
}

func setupCapturedPayment(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (paymentID string, deps *admin.Deps, uc *payment.UseCase) {
	t.Helper()
	q := sqlc.New(pool)
	// 商品と payments(captured, amount=1000, stripe_payment_intent_id=pi_x) をシード
	ids := idgen.NewDefault()
	prodID := ids.New()
	paymentID = ids.New()
	pi := "pi_test"
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		prodID, "デモ", int64(1000), `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy, state, stripe_payment_intent_id) VALUES ($1,$2,$3,'captured'::payment_state,$4)`,
		paymentID, prodID, int64(1000), pi); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	stripe := &stubStripeRefund{}
	store := payment.NewPGStore(pool, q)
	uc = payment.NewUseCase(store, stripe, idgen.NewDefault(), "manual", time.Hour)
	deps = &admin.Deps{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:      q,
		Sessions:     admin.NewPGSessionStore(q, idgen.NewDefault()),
		IDs:          idgen.NewDefault(),
		Stripe:       stripe,
		UseCase:      uc,
		Store:        store,
		LoginLimiter: admin.NewRateLimiter(time.Minute, 5),
		IPLimiter:    admin.NewRateLimiter(time.Minute, 20),
		SessionTTL:   time.Hour,
	}
	return paymentID, deps, uc
}

func TestExecuteRefund_PartialThenFull(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _ := setupCapturedPayment(t, ctx, pool)

	// 半額返金 → partially_refunded
	res1, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(400), "顧客要望", "admin-1")
	if err != nil {
		t.Fatalf("refund1: %v", err)
	}
	if res1.State != "partially_refunded" || res1.RefundedJPY != 400 {
		t.Fatalf("res1=%+v", res1)
	}
	// 残額全部 → refunded
	res2, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(600), "全額返金", "admin-1")
	if err != nil {
		t.Fatalf("refund2: %v", err)
	}
	if res2.State != "refunded" || res2.RefundedJPY != 1000 {
		t.Fatalf("res2=%+v", res2)
	}
	// 監査ログが2件（refund action）
	q := sqlc.New(pool)
	logs, err := q.ListAuditLogs(ctx, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	refundCount := 0
	for _, l := range logs {
		if l.Action == "refund" {
			refundCount++
		}
	}
	if refundCount != 2 {
		t.Fatalf("audit refund count=%d want 2", refundCount)
	}
}

func TestExecuteRefund_ExceedsAmount(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _ := setupCapturedPayment(t, ctx, pool)
	_, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(1001), "over", "admin-1")
	if !errors.Is(err, admin.ErrExceedsAmount) {
		t.Fatalf("err=%v want ErrExceedsAmount", err)
	}
}

func TestExecuteRefund_PaymentNotFound(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	_, deps, _ := setupCapturedPayment(t, ctx, pool)
	_, err := deps.ExecuteRefund(ctx, "nonexistent-id", money.MustNew(100), "", "admin-1")
	if !errors.Is(err, admin.ErrPaymentNotFound) {
		t.Fatalf("err=%v want ErrPaymentNotFound", err)
	}
}

// Login/Refund HTTPハンドラのfake+HTTPテスト（CRAPカバレッジ用途を兼ねる）
func TestLogin_FullFlow(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	deps := &admin.Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries: q, Sessions: admin.NewPGSessionStore(q, idgen.NewDefault()),
		IDs:          idgen.NewDefault(),
		LoginLimiter: admin.NewRateLimiter(time.Minute, 5),
		IPLimiter:    admin.NewRateLimiter(time.Minute, 20),
		SessionTTL:   time.Hour,
	}
	// ユーザー登録
	pw := "Sec$re-pass!"
	p := admin.DefaultArgon2Params()
	p.Memory = 8 * 1024
	p.Time = 1
	hash, _ := admin.HashPassword(pw, p)
	if _, err := q.CreateAdminUser(ctx, sqlc.CreateAdminUserParams{
		ID: idgen.NewDefault().New(), Email: "admin@example.com", PasswordHash: hash,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 正しい認証
	form := "email=admin@example.com&password=" + pw
	r := newPOST(t, "/admin/login", form)
	rr := recordAndCall(deps.Login, r)
	if rr.Code != 303 {
		t.Fatalf("login status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(rr.Result().Cookies()) == 0 {
		t.Fatalf("session cookie未発行")
	}
	// 誤った認証
	rr2 := recordAndCall(deps.Login, newPOST(t, "/admin/login", "email=admin@example.com&password=wrong"))
	if rr2.Code != 401 {
		t.Fatalf("bad login status=%d", rr2.Code)
	}
}

// LoginFormのレンダリングを軽く叩く（CRAPカバレッジのため）
func TestLoginForm_Renders(t *testing.T) {
	deps := &admin.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r := httptest.NewRequest("GET", "/admin/login", nil)
	rr := recordAndCall(deps.LoginForm, r)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "管理者ログイン") {
		t.Fatalf("body=%s", body[:min(200, len(body))])
	}
}

func TestRefund_HTTP_ExceedsAmount(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _ := setupCapturedPayment(t, ctx, pool)

	form := "amount_jpy=9999&reason=over"
	r := newPOST(t, "/admin/payments/"+paymentID+"/refund", form)
	r.SetPathValue("payment_id", paymentID)
	// UserIDFromContextで空文字が返ってもOK（監査ログのactorが空になるだけ）
	rr := recordAndCall(deps.Refund, r)
	if rr.Code != 422 {
		t.Fatalf("status=%d want 422", rr.Code)
	}
}

func TestRateLimiter_Allow(t *testing.T) {
	t.Parallel()
	rl := admin.NewRateLimiter(time.Minute, 3)
	for range 3 {
		if !rl.Allow("k") {
			t.Fatalf("最初の3回は許容")
		}
	}
	if rl.Allow("k") {
		t.Fatalf("4回目は拒否")
	}
	// 別keyは影響を受けない
	if !rl.Allow("other") {
		t.Fatalf("独立key")
	}
}
