package httpx_test

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/httpx"
	"github.com/okamyuji/kessai/internal/httpx/httpxproto"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
	"github.com/okamyuji/kessai/web/templates"
)

var sharedPG *pgcontainer.Container

func TestMain(m *testing.M) {
	if os.Getenv("KESSAI_SKIP_INTEGRATION") != "" {
		os.Exit(m.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgcontainer.Start(ctx)
	if err != nil {
		log.Printf("pgcontainer未起動でスキップ: %v", err)
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
		t.Skip("testcontainers未起動のためスキップ")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

type stubStripe struct{ callCount int }

func (s *stubStripe) CreatePaymentIntent(_ context.Context, req stripeclient.CreateIntentRequest) (*stripeclient.Intent, error) {
	s.callCount++
	return &stripeclient.Intent{
		ID:           "pi_" + req.IdempotencyMaster,
		ClientSecret: "cs_" + req.IdempotencyMaster,
		Status:       "requires_confirmation",
		AmountJPY:    req.Amount.Int64(),
	}, nil
}
func (s *stubStripe) CapturePaymentIntent(context.Context, stripeclient.CaptureRequest) (*stripeclient.Intent, error) {
	return nil, nil
}
func (s *stubStripe) Refund(context.Context, stripeclient.RefundRequest) (*stripeclient.Refund, error) {
	return nil, nil
}

func newServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *stubStripe) {
	t.Helper()
	q := sqlc.New(pool)
	store := payment.NewPGStore(pool, q)
	sc := &stubStripe{}
	uc := payment.NewUseCase(store, sc, idgen.NewDefault(), "manual", time.Hour)
	deps := httpx.CheckoutDeps{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries: q,
		UseCase: uc,
		IDGen:   idgen.NewDefault(),
		PubKey:  "pk_test_dummy",
		Tokusho: templates.TokushoSnapshot{
			Portion: "デモ1点", PriceWithShipping: "1,000円（送料無料）",
			PaymentTiming: "クレジット即時", DeliveryTiming: "決済完了と同時",
			WithdrawalAndReturns: "キャンセル不可",
		},
	}
	key := []byte("test-key-that-is-32-bytes-long!!01")
	h := httpx.NewMux(deps, key, false /* insecureCookie */, "../../web/static")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, sc
}

func seedProduct(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	ids := idgen.NewDefault()
	productID := ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		productID, "デモ", int64(1000), `{}`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return productID
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	// redirectを追跡しない（302の宛先を検証したいテスト向け）
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// GET / でCheckoutPageが返り、CSRFトークンが埋め込まれていること
func TestGetIndex_HasCSRFAndPrice(t *testing.T) {
	pool := requirePG(t)
	seedProduct(t, context.Background(), pool)
	srv, _ := newServer(t, pool)
	c := newClient(t)

	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "1,000円") {
		t.Fatalf("価格が表示されていない: %q", s[:min(200, len(s))])
	}
	if !strings.Contains(s, "name=\""+httpxproto.CSRFFormField+"\"") {
		t.Fatalf("CSRF hidden inputが無い")
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("CSPヘッダが無い")
	}
}

// CSRFトークン未添付POSTは401
func TestPostCheckout_NoCSRF_401(t *testing.T) {
	pool := requirePG(t)
	productID := seedProduct(t, context.Background(), pool)
	srv, _ := newServer(t, pool)
	c := newClient(t)
	// Cookieを取得
	_, _ = c.Get(srv.URL + "/")

	form := url.Values{"product_id": {productID}}
	req, _ := http.NewRequest("POST", srv.URL+"/checkout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", resp.StatusCode)
	}
}

// CSRFトークン付きPOSTは303 See Other → /pay/{payment_id}
func TestPostCheckout_WithCSRF_303ToConfirm(t *testing.T) {
	pool := requirePG(t)
	productID := seedProduct(t, context.Background(), pool)
	srv, stripe := newServer(t, pool)
	c := newClient(t)
	// 最初のGETでCSRFトークンをCookieとフォーム側に取り込む
	getResp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	body, _ := io.ReadAll(getResp.Body)
	token := extractCSRFToken(string(body))
	if token == "" {
		t.Fatalf("CSRF tokenが取得できない")
	}
	form := url.Values{"product_id": {productID}, httpxproto.CSRFFormField: {token}}
	req, _ := http.NewRequest("POST", srv.URL+"/checkout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/pay/") {
		t.Fatalf("Location=%q", loc)
	}
	if stripe.callCount != 1 {
		t.Fatalf("Stripe呼び出し数=%d want 1", stripe.callCount)
	}
}

// POST /checkout の303を追跡して /pay/{id} を取得し、confirmページのHTMLが返ることを確認
func TestGetConfirm_ShowsPaymentIntentAndCSP(t *testing.T) {
	pool := requirePG(t)
	productID := seedProduct(t, context.Background(), pool)
	srv, _ := newServer(t, pool)
	c := newClient(t)
	// 決済作成
	getResp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	body, _ := io.ReadAll(getResp.Body)
	token := extractCSRFToken(string(body))
	form := url.Values{"product_id": {productID}, httpxproto.CSRFFormField: {token}}
	req, _ := http.NewRequest("POST", srv.URL+"/checkout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postResp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = postResp.Body.Close() }()
	loc := postResp.Header.Get("Location")

	confResp, err := c.Get(srv.URL + loc)
	if err != nil {
		t.Fatalf("GET confirm: %v", err)
	}
	defer func() { _ = confResp.Body.Close() }()
	if confResp.StatusCode != 200 {
		t.Fatalf("confirm status=%d", confResp.StatusCode)
	}
	confBody, _ := io.ReadAll(confResp.Body)
	s := string(confBody)
	if !strings.Contains(s, "id=\"payment-element\"") {
		t.Fatalf("confirm ページに payment-element が無い: %q", s[:min(400, len(s))])
	}
	if confResp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("confirm ページに CSPが無い")
	}
}

// エクスポート済み薄ラッパを直接呼んで分類ロジックを網羅する
func TestClassifyPaymentError_AllBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"conflict", payment.ErrIdempotencyConflict, 409},
		{"in-progress", payment.ErrIdempotencyInProgress, 409},
		{"psp-unavailable", stripeclient.ErrPSPUnavailable, 503},
		{"internal", errors.New("unknown"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			httpx.TranslatePaymentErrorForTest(rec, slog.New(slog.NewTextHandler(io.Discard, nil)), tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d", rec.Code, tc.status)
			}
		})
	}
}

// classifyPaymentError の分類パスを直接検証
func TestClassifyPaymentError_ViaHTTP(t *testing.T) {
	// httpx.classifyPaymentError は private だが、handler経由で挙動を確認する。
	// ここではhttptest.NewRecorderで translatePaymentError を実質検証するため、
	// 誤ったproductIDを送ってValidation経路をカバーする（他は既存テストでカバー）。
	pool := requirePG(t)
	srv, _ := newServer(t, pool)
	c := newClient(t)
	_, _ = c.Get(srv.URL + "/") // CSRF Cookie発行
	form := url.Values{"product_id": {"nonexistent-id"}}
	// CSRFトークンをGET応答から抜く
	getResp, _ := c.Get(srv.URL + "/")
	defer func() { _ = getResp.Body.Close() }()
	body, _ := io.ReadAll(getResp.Body)
	form.Set(httpxproto.CSRFFormField, extractCSRFToken(string(body)))
	req, _ := http.NewRequest("POST", srv.URL+"/checkout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 400 {
		t.Fatalf("status=%d 400番台を期待", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" && !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type=%q エラー応答として妥当な形式ではない", ct)
	}
}

func extractCSRFToken(html string) string {
	needle := "name=\"" + httpxproto.CSRFFormField + "\" value=\""
	i := strings.Index(html, needle)
	if i < 0 {
		return ""
	}
	j := i + len(needle)
	k := strings.Index(html[j:], "\"")
	if k < 0 {
		return ""
	}
	return html[j : j+k]
}
