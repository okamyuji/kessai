package admin

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/okamyuji/kessai/internal/httpx/httpxproto"
	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/statemachine"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/problem"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// SessionCookieName セッションIDを載せるCookie名
const SessionCookieName = "kessai_admin_sid"

// htmlLoginTmpl はログインフォーム。動的値は {{.CSRFToken}} を通し、html/templateが自動エスケープ
var htmlLoginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html><html lang="ja"><body>
<h1>管理者ログイン</h1>
<form method="POST" action="/admin/login">
<input type="hidden" name="{{.CSRFField}}" value="{{.CSRFToken}}"/>
<label>メール<input name="email" type="email" required></label>
<label>パスワード<input name="password" type="password" required></label>
<button type="submit">ログイン</button>
</form></body></html>`))

// htmlDashboardTmpl 決済一覧
var htmlDashboardTmpl = template.Must(template.New("dash").Parse(`<!DOCTYPE html><html lang="ja"><body>
<h1>決済一覧</h1>
<table>
<tr><th>ID</th><th>状態</th><th>金額</th><th>返金累計</th></tr>
{{range .Payments}}
<tr><td>{{.ID}}</td><td>{{.State}}</td><td>{{.AmountJpy}}</td><td>{{.RefundedJpy}}</td></tr>
{{end}}
</table></body></html>`))

// Deps 管理画面ハンドラの依存
type Deps struct {
	Logger       *slog.Logger
	Queries      *sqlc.Queries
	Sessions     SessionStore
	IDs          idgen.Generator
	Stripe       stripeclient.Client
	UseCase      *payment.UseCase
	Store        payment.Store
	LoginLimiter *RateLimiter // アカウント単位
	IPLimiter    *RateLimiter // IP単位
	SessionTTL   time.Duration
	SecureCookie bool
}

// -- ミドルウェア -------------------------------------------------------------

type ctxKeyAdminID struct{}

// UserIDFromContext 現在のadmin_user_idを取り出します
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAdminID{}).(string)
	return v
}

// RequireAuth セッションを検証し、未認証は /admin/login にリダイレクトします
func (d *Deps) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		uid, err := d.Sessions.LookupSession(r.Context(), c.Value)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyAdminID{}, uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// -- ハンドラ -----------------------------------------------------------------

// LoginForm GET /admin/login
// XSS対策のため html/template ですべての動的値をエスケープします。
func (d *Deps) LoginForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// httpxミドルウェアが埋めるContext上のCSRFトークン。無ければCookieから直接読む。
	token, _ := r.Context().Value(csrfTokenContextKey{}).(string)
	if token == "" {
		if c, err := r.Cookie(httpxproto.CSRFCookieName); err == nil {
			token = c.Value
		}
	}
	if err := htmlLoginTmpl.Execute(w, map[string]string{
		"CSRFField": httpxproto.CSRFFormField,
		"CSRFToken": token,
	}); err != nil {
		d.Logger.Error("login form", "err", err)
	}
}

// csrfTokenContextKey httpxとの疎結合を保ちつつテストで注入するための鍵型
type csrfTokenContextKey struct{}

// Login POST /admin/login: 認証成功でCookie発行→/admin/へリダイレクト
func (d *Deps) Login(w http.ResponseWriter, r *http.Request) {
	email, pwd, p := parseLoginForm(r)
	if p != nil {
		p.Write(w, d.Logger)
		return
	}
	if p := d.applyLoginRateLimit(w, r, email); p != nil {
		p.Write(w, d.Logger)
		return
	}
	user, p := d.lookupAdminUser(r.Context(), email, pwd)
	if p != nil {
		p.Write(w, d.Logger)
		return
	}
	sid, err := d.Sessions.CreateSession(r.Context(), user.ID, d.SessionTTL)
	if err != nil {
		problem.Internal("session発行失敗").Write(w, d.Logger)
		return
	}
	d.setSessionCookie(w, sid)
	_ = d.Queries.InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		ID: d.IDs.New(), Actor: user.ID, Action: "login",
		SubjectType: "admin_user", SubjectID: user.ID, Detail: []byte(`{}`),
	})
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func parseLoginForm(r *http.Request) (string, string, *problem.Problem) {
	if err := r.ParseForm(); err != nil {
		return "", "", problem.Validation("フォーム解析失敗")
	}
	email := r.PostForm.Get("email")
	pwd := r.PostForm.Get("password")
	if email == "" || pwd == "" {
		return "", "", problem.Validation("email/password必須")
	}
	return email, pwd, nil
}

func (d *Deps) applyLoginRateLimit(w http.ResponseWriter, r *http.Request, email string) *problem.Problem {
	if !d.IPLimiter.Allow(clientIP(r)) || !d.LoginLimiter.Allow(email) {
		w.WriteHeader(http.StatusTooManyRequests)
		return problem.Validation("レート制限")
	}
	return nil
}

func (d *Deps) lookupAdminUser(ctx context.Context, email, pwd string) (sqlc.AdminUser, *problem.Problem) {
	user, err := d.Queries.GetAdminUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.AdminUser{}, problem.Unauthorized("認証失敗")
		}
		return sqlc.AdminUser{}, problem.Internal("DBエラー")
	}
	ok, err := VerifyPassword(pwd, user.PasswordHash)
	if err != nil || !ok {
		return sqlc.AdminUser{}, problem.Unauthorized("認証失敗")
	}
	return user, nil
}

// Logout POST /admin/logout
func (d *Deps) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		_ = d.Sessions.DestroySession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- SecureはCLI/環境で切替
		Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: d.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// Dashboard GET /admin/
func (d *Deps) Dashboard(w http.ResponseWriter, r *http.Request) {
	list, err := d.Queries.ListPayments(r.Context(), sqlc.ListPaymentsParams{Limit: 50, Offset: 0})
	if err != nil {
		problem.Internal("一覧取得失敗").Write(w, d.Logger)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := htmlDashboardTmpl.Execute(w, map[string]any{"Payments": list}); err != nil {
		d.Logger.Error("dashboard tmpl", "err", err)
	}
	_ = strconv.Itoa // strconvを外さないため参照維持
}

// Refund POST /admin/payments/{payment_id}/refund
// フォーム: amount_jpy=N, reason=<任意>
func (d *Deps) Refund(w http.ResponseWriter, r *http.Request) {
	paymentID := r.PathValue("payment_id")
	if err := r.ParseForm(); err != nil {
		problem.Validation("フォーム解析失敗").Write(w, d.Logger)
		return
	}
	amountRaw := r.PostForm.Get("amount_jpy")
	amount64, err := strconv.ParseInt(amountRaw, 10, 64)
	if err != nil || amount64 <= 0 {
		problem.Validation("amount_jpyは正の整数").Write(w, d.Logger)
		return
	}
	amt, _ := money.New(amount64)
	reason := r.PostForm.Get("reason")
	adminID := UserIDFromContext(r.Context())
	res, err := d.ExecuteRefund(r.Context(), paymentID, amt, reason, adminID)
	if err != nil {
		d.translateRefundError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// RefundResult 返金APIの応答
type RefundResult struct {
	PaymentID   string `json:"payment_id"`
	State       string `json:"state"`
	RefundedJPY int64  `json:"refunded_jpy"`
	StripeRef   string `json:"stripe_refund_id"`
}

// ErrExceedsAmount 返金累計が取引金額を超過
var ErrExceedsAmount = errors.New("admin: refund超過")

// ErrPaymentNotFound 決済見つからず
var ErrPaymentNotFound = errors.New("admin: payment not found")

// ExecuteRefund 業務ロジック本体。テストからも直接呼べる。
// 手順: (1) 取引取得と超過検証、(2) StripeへRefund、(3) 遷移テーブル駆動でstate更新+履歴+
// refunded_jpy加算+ledger記帳、を同一トランザクションで反映。
func (d *Deps) ExecuteRefund(ctx context.Context, paymentID string, amount money.JPY, reason, adminID string) (*RefundResult, error) {
	pay, err := d.Queries.GetPayment(ctx, paymentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPaymentNotFound
		}
		return nil, err
	}
	if pay.RefundedJpy+amount.Int64() > pay.AmountJpy {
		return nil, ErrExceedsAmount
	}
	if pay.StripePaymentIntentID == nil {
		return nil, errors.New("admin: PaymentIntent未設定")
	}
	// 返金連番はUseCase側で持たないので、ここでは単純にrefunded_jpyの回数を数える簡易実装。
	// 実利用ではrefund履歴テーブルから連番を取得する。当面は epoch を混ぜて一意性を担保する。
	refundSeq := int(time.Now().UnixNano() & 0x7fffffff)
	rf, err := d.Stripe.Refund(ctx, stripeclient.RefundRequest{
		IdempotencyMaster: paymentID,
		PaymentIntentID:   *pay.StripePaymentIntentID,
		Amount:            amount,
		RefundSeq:         refundSeq,
		Reason:            reason,
	})
	if err != nil {
		return nil, err
	}
	tx, err := d.Store.StartTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qTx := tx.Queries()
	// 返金累計を更新
	upd, err := qTx.AddRefundedAmount(ctx, sqlc.AddRefundedAmountParams{ID: paymentID, RefundedJpy: amount.Int64()})
	if err != nil {
		return nil, err
	}
	// 状態遷移: 累計=全額 → RefundFull、それ以外 → RefundPartial
	event := statemachine.EventRefundPartial
	if upd.RefundedJpy == upd.AmountJpy {
		event = statemachine.EventRefundFull
	}
	next, err := d.UseCase.ApplyEvent(ctx, tx, paymentID, adminID, event)
	if err != nil {
		return nil, err
	}
	// 監査ログ
	detail, _ := json.Marshal(map[string]any{
		"amount_jpy": amount.Int64(), "reason": reason, "stripe_refund_id": rf.ID,
	})
	if err := qTx.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		ID: d.IDs.New(), Actor: adminID, Action: "refund",
		SubjectType: "payment", SubjectID: paymentID, Detail: detail,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &RefundResult{
		PaymentID: paymentID, State: string(next),
		RefundedJPY: upd.RefundedJpy, StripeRef: rf.ID,
	}, nil
}

func (d *Deps) translateRefundError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPaymentNotFound):
		problem.Validation("決済が見つからない").Write(w, d.Logger)
	case errors.Is(err, ErrExceedsAmount):
		problem.RefundExceedsAmount(err.Error()).Write(w, d.Logger)
	case errors.Is(err, statemachine.ErrInvalidTransition):
		problem.InvalidTransition(err.Error()).Write(w, d.Logger)
	case errors.Is(err, stripeclient.ErrPSPUnavailable):
		problem.PSPUnavailable(err.Error()).Write(w, d.Logger)
	default:
		d.Logger.Error("refund失敗", "err", err)
		problem.Internal("内部エラー").Write(w, d.Logger)
	}
}

func (d *Deps) setSessionCookie(w http.ResponseWriter, sid string) {
	c := &http.Cookie{ // #nosec G124 -- SecureはCLI/環境で切替
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   d.SecureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(d.SessionTTL),
	}
	http.SetCookie(w, c)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return fwd
	}
	return r.RemoteAddr
}
