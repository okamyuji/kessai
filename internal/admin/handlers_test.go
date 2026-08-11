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
	err      error
	seqs     []int
	captures []stripeclient.CaptureRequest
}

func (*stubStripeRefund) CreatePaymentIntent(context.Context, stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	return &stripeclient.Intent{ID: "pi_test", ClientSecret: "cs_test", Status: "requires_confirmation"}, nil
}
func (s *stubStripeRefund) CapturePaymentIntent(_ context.Context, req stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	s.captures = append(s.captures, req)
	if s.err != nil {
		return nil, s.err
	}
	return &stripeclient.Intent{ID: req.PaymentIntentID, Status: "succeeded", AmountJPY: req.Amount.Int64()}, nil
}
func (s *stubStripeRefund) Refund(_ context.Context, req stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	s.seqs = append(s.seqs, req.RefundSeq)
	if s.err != nil {
		return nil, s.err
	}
	return &stripeclient.Refund{ID: "re_" + req.IdempotencyMaster, Status: "succeeded", AmountJPY: req.Amount.Int64(), PaymentIntentID: req.PaymentIntentID}, nil
}

func setupCapturedPayment(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (paymentID string, deps *admin.Deps, uc *payment.UseCase, stripe *stubStripeRefund) {
	t.Helper()
	return setupPaymentInState(t, ctx, pool, "captured")
}

// setupPaymentInState 指定状態のpayments行と依存一式をシードする
func setupPaymentInState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state string) (paymentID string, deps *admin.Deps, uc *payment.UseCase, stripe *stubStripeRefund) {
	t.Helper()
	q := sqlc.New(pool)
	// 商品と payments(amount=1000, stripe_payment_intent_id=pi_x) をシード
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
		`INSERT INTO payments (id, product_id, amount_jpy, state, stripe_payment_intent_id) VALUES ($1,$2,$3,$4::payment_state,$5)`,
		paymentID, prodID, int64(1000), state, pi); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	stripe = &stubStripeRefund{}
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
	return paymentID, deps, uc, stripe
}

func TestExecuteRefund_PartialThenFull(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, _ := setupCapturedPayment(t, ctx, pool)

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

// TestExecuteRefund_WritesLedgerEntries 返金が借方refunds・貸方psp_receivableの2行を台帳へ追記することを検証する
func TestExecuteRefund_WritesLedgerEntries(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, _ := setupCapturedPayment(t, ctx, pool)

	if _, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(400), "顧客要望", "admin-1"); err != nil {
		t.Fatalf("refund: %v", err)
	}
	entries, err := sqlc.New(pool).ListLedgerByPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger entries=%d want 2", len(entries))
	}
	wantTID := paymentID + ":refund:1"
	var debitOK, creditOK bool
	for _, e := range entries {
		if e.TransferID != wantTID {
			t.Fatalf("transfer_id=%s want %s", e.TransferID, wantTID)
		}
		if e.AmountJpy != 400 {
			t.Fatalf("amount=%d want 400", e.AmountJpy)
		}
		switch {
		case e.Side == "debit" && e.Account == "refunds":
			debitOK = true
		case e.Side == "credit" && e.Account == "psp_receivable":
			creditOK = true
		}
	}
	if !debitOK || !creditOK {
		t.Fatalf("借方refunds/貸方psp_receivableの組になっていない: %+v", entries)
	}
}

// TestExecuteRefund_RefundSeqDeterministic 返金連番が1,2と決定的に増加しStripeの派生キーへ渡ることを検証する
func TestExecuteRefund_RefundSeqDeterministic(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupCapturedPayment(t, ctx, pool)

	if _, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(400), "1回目", "admin-1"); err != nil {
		t.Fatalf("refund1: %v", err)
	}
	if _, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(600), "2回目", "admin-1"); err != nil {
		t.Fatalf("refund2: %v", err)
	}
	if len(stripe.seqs) != 2 || stripe.seqs[0] != 1 || stripe.seqs[1] != 2 {
		t.Fatalf("seqs=%v want [1 2]", stripe.seqs)
	}
	entries, err := sqlc.New(pool).ListLedgerByPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	tids := map[string]bool{}
	for _, e := range entries {
		tids[e.TransferID] = true
	}
	if !tids[paymentID+":refund:1"] || !tids[paymentID+":refund:2"] {
		t.Fatalf("決定的なtransfer_idになっていない: %v", tids)
	}
}

// TestExecuteCapture_AuthorizedToCaptured 手動キャプチャでcapturedへ遷移し台帳・監査ログが記録されることを検証する
func TestExecuteCapture_AuthorizedToCaptured(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupPaymentInState(t, ctx, pool, "authorized")

	res, err := deps.ExecuteCapture(ctx, paymentID, "admin-1")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.State != "captured" {
		t.Fatalf("state=%s want captured", res.State)
	}
	if len(stripe.captures) != 1 || stripe.captures[0].PaymentIntentID != "pi_test" || stripe.captures[0].Amount.Int64() != 1000 {
		t.Fatalf("Stripeキャプチャ呼び出しが不正: %+v", stripe.captures)
	}
	q := sqlc.New(pool)
	entries, err := q.ListLedgerByPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	// Webhook経路(outboxhandler)と同じtransfer_id導出であること（二重記帳防止の根拠）
	wantTID := paymentID + ":capture:1"
	if len(entries) != 2 || entries[0].TransferID != wantTID || entries[1].TransferID != wantTID {
		t.Fatalf("台帳が2行・transfer_id=%sでない: %+v", wantTID, entries)
	}
	logs, err := q.ListAuditLogs(ctx, 10)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, l := range logs {
		if l.Action == "capture" && l.SubjectID == paymentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("監査ログにcaptureが無い: %+v", logs)
	}
}

// TestExecuteCapture_InvalidState authorized以外の状態ではキャプチャできないことを検証する
func TestExecuteCapture_InvalidState(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupCapturedPayment(t, ctx, pool)

	if _, err := deps.ExecuteCapture(ctx, paymentID, "admin-1"); !errors.Is(err, admin.ErrNotCapturable) {
		t.Fatalf("err=%v want ErrNotCapturable", err)
	}
	if len(stripe.captures) != 0 {
		t.Fatalf("不正状態でStripeを呼んでいる")
	}
}

// TestExecuteCapture_StripeError StripeキャプチャAPIが失敗したらエラーを返しDBを変更しないことを検証する
func TestExecuteCapture_StripeError(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupPaymentInState(t, ctx, pool, "authorized")
	stripe.err = errors.New("stripe down")

	if _, err := deps.ExecuteCapture(ctx, paymentID, "admin-1"); err == nil {
		t.Fatalf("エラーが返らない")
	}
	q := sqlc.New(pool)
	pay, err := q.GetPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if string(pay.State) != "authorized" {
		t.Fatalf("失敗時に状態が変わっている: %s", pay.State)
	}
	entries, err := q.ListLedgerByPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失敗時に台帳へ記帳されている: %d件", len(entries))
	}
}

// TestExecuteCapture_PaymentNotFound 存在しない決済へのキャプチャはErrPaymentNotFoundになることを検証する
func TestExecuteCapture_PaymentNotFound(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	_, deps, _, _ := setupPaymentInState(t, ctx, pool, "authorized")
	if _, err := deps.ExecuteCapture(ctx, "nonexistent-id", "admin-1"); !errors.Is(err, admin.ErrPaymentNotFound) {
		t.Fatalf("err=%v want ErrPaymentNotFound", err)
	}
}

// TestExecuteRefund_StripeError Stripe返金APIが失敗したらエラーを返しDBを変更しないことを検証する
func TestExecuteRefund_StripeError(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupCapturedPayment(t, ctx, pool)
	stripe.err = errors.New("stripe down")

	if _, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(400), "失敗系", "admin-1"); err == nil {
		t.Fatalf("エラーが返らない")
	}
	q := sqlc.New(pool)
	pay, err := q.GetPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("get payment: %v", err)
	}
	if pay.RefundedJpy != 0 || string(pay.State) != "captured" {
		t.Fatalf("失敗時にDBが変更されている: refunded=%d state=%s", pay.RefundedJpy, pay.State)
	}
	entries, err := q.ListLedgerByPayment(ctx, paymentID)
	if err != nil {
		t.Fatalf("ledger list: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("失敗時に台帳へ記帳されている: %d件", len(entries))
	}
}

// TestExecuteRefund_NoPaymentIntent PaymentIntent未設定の決済への返金はエラーになることを検証する
func TestExecuteRefund_NoPaymentIntent(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	_, deps, _, _ := setupCapturedPayment(t, ctx, pool)
	// stripe_payment_intent_id無しの決済を別途シード
	ids := idgen.NewDefault()
	prodID2 := ids.New()
	payID2 := ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		prodID2, "デモ2", int64(500), `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy, state) VALUES ($1,$2,$3,'captured'::payment_state)`,
		payID2, prodID2, int64(500)); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if _, err := deps.ExecuteRefund(ctx, payID2, money.MustNew(100), "", "admin-1"); err == nil {
		t.Fatalf("PaymentIntent未設定でエラーが返らない")
	}
}

func TestExecuteRefund_ExceedsAmount(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, _ := setupCapturedPayment(t, ctx, pool)
	_, err := deps.ExecuteRefund(ctx, paymentID, money.MustNew(1001), "over", "admin-1")
	if !errors.Is(err, admin.ErrExceedsAmount) {
		t.Fatalf("err=%v want ErrExceedsAmount", err)
	}
}

func TestExecuteRefund_PaymentNotFound(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	_, deps, _, _ := setupCapturedPayment(t, ctx, pool)
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

// TestLogin_RateLimit_429Problem レート制限超過時にHTTP 429とProblem本文のstatusが一致することを検証する
func TestLogin_RateLimit_429Problem(t *testing.T) {
	pool := requirePG(t)
	q := sqlc.New(pool)
	deps := &admin.Deps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries: q, Sessions: admin.NewPGSessionStore(q, idgen.NewDefault()),
		IDs:          idgen.NewDefault(),
		LoginLimiter: admin.NewRateLimiter(time.Minute, 5),
		IPLimiter:    admin.NewRateLimiter(time.Minute, 20),
		SessionTTL:   time.Hour,
	}
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = recordAndCall(deps.Login, newPOST(t, "/admin/login", "email=rate@example.com&password=x"))
	}
	if last.Code != 429 {
		t.Fatalf("status=%d want 429", last.Code)
	}
	body := last.Body.String()
	if !strings.Contains(body, "/problems/rate-limited") {
		t.Fatalf("type URIが/problems/rate-limitedでない: %s", body)
	}
	if !strings.Contains(body, `"status":429`) {
		t.Fatalf("Problem本文のstatusが429でない: %s", body)
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

// TestAdminHTTP_ErrorTranslation 業務エラーがRFC 9457のHTTPステータスへ変換されることを検証する
func TestAdminHTTP_ErrorTranslation(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, stripe := setupCapturedPayment(t, ctx, pool)

	// capturedへのキャプチャ → 遷移不可(409)
	r := newPOST(t, "/admin/payments/"+paymentID+"/capture", "")
	r.SetPathValue("payment_id", paymentID)
	if rr := recordAndCall(deps.Capture, r); rr.Code != 409 {
		t.Fatalf("capture invalid state status=%d want 409", rr.Code)
	}
	// 存在しない決済への返金 → 400
	r2 := newPOST(t, "/admin/payments/nonexistent/refund", "amount_jpy=100")
	r2.SetPathValue("payment_id", "nonexistent")
	if rr := recordAndCall(deps.Refund, r2); rr.Code != 400 {
		t.Fatalf("not found status=%d want 400", rr.Code)
	}
	// PSP停止 → 503
	stripe.err = stripeclient.ErrPSPUnavailable
	r3 := newPOST(t, "/admin/payments/"+paymentID+"/refund", "amount_jpy=100")
	r3.SetPathValue("payment_id", paymentID)
	if rr := recordAndCall(deps.Refund, r3); rr.Code != 503 {
		t.Fatalf("psp unavailable status=%d want 503", rr.Code)
	}
	// その他のエラー → 500
	stripe.err = errors.New("unexpected")
	r4 := newPOST(t, "/admin/payments/"+paymentID+"/refund", "amount_jpy=100")
	r4.SetPathValue("payment_id", paymentID)
	if rr := recordAndCall(deps.Refund, r4); rr.Code != 500 {
		t.Fatalf("internal status=%d want 500", rr.Code)
	}
}

// TestExecuteCapture_NoPaymentIntent PaymentIntent未設定のauthorized決済はキャプチャできないことを検証する
func TestExecuteCapture_NoPaymentIntent(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	_, deps, _, _ := setupPaymentInState(t, ctx, pool, "authorized")
	ids := idgen.NewDefault()
	prodID2 := ids.New()
	payID2 := ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		prodID2, "デモ2", int64(500), `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy, state) VALUES ($1,$2,$3,'authorized'::payment_state)`,
		payID2, prodID2, int64(500)); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	if _, err := deps.ExecuteCapture(ctx, payID2, "admin-1"); err == nil {
		t.Fatalf("PaymentIntent未設定でエラーが返らない")
	}
}

func TestRefund_HTTP_ExceedsAmount(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	paymentID, deps, _, _ := setupCapturedPayment(t, ctx, pool)

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
