// Package httpx HTTPミドルウェアとハンドラの補助関数を提供します。
// パッケージ名はGo標準のnet/httpとの衝突回避のためhttpxとしています。
package httpx

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/okamyuji/kessai/internal/httpx/httpxproto"
	"github.com/okamyuji/kessai/internal/platform/problem"
)

// SecurityHeaders CSP・その他のセキュリティヘッダを付与するミドルウェアです。
// Stripe.js を許可し、それ以外の外部スクリプトはブロックします。
// PCI DSS SAQ Aの要求（外部スクリプトを制御下に置く）に対応します。
func SecurityHeaders(stripeSrc string) func(http.Handler) http.Handler {
	// content-security-policy: script-src と frame-src はStripe.jsと3DS用iframeのために許可する
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' " + stripeSrc,
		"style-src 'self' 'unsafe-inline'",
		"frame-src " + stripeSrc,
		"connect-src 'self' " + stripeSrc,
		"img-src 'self' data:",
		"base-uri 'self'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; ")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			next.ServeHTTP(w, r)
		})
	}
}

// -- CSRF ---------------------------------------------------------------------

// 互換用の別名（既存の参照を壊さないため）
const (
	CSRFCookieName = httpxproto.CSRFCookieName
	CSRFHeaderName = httpxproto.CSRFHeaderName
	CSRFFormField  = httpxproto.CSRFFormField
)

// csrfContextKey コンテキストへ格納するキー型
type csrfContextKey struct{}

// CSRFTokenFromContext ハンドラ・テンプレから現在のCSRFトークンを取得します
func CSRFTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(csrfContextKey{}).(string)
	return v
}

// NewCSRF 署名付きCSRFトークンを発行・検証するミドルウェアを返します。
// signingKeyは32バイト以上のランダムシークレット。cookieはHttpOnly・SameSite=Laxで発行します。
// 検証は状態変更メソッド（POST/PUT/PATCH/DELETE）のみに行い、Cookieの値とヘッダかフォームフィールドの値がHMAC上一致することを求めます。
func NewCSRF(signingKey []byte, secureCookie bool, logger *slog.Logger) func(http.Handler) http.Handler {
	if len(signingKey) < 32 {
		panic("httpx: CSRF signing key must be >= 32 bytes")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := readCSRFCookie(r)
			if token == "" {
				token = generateCSRFToken(signingKey)
				setCSRFCookie(w, token, secureCookie)
			}
			if isStateChanging(r.Method) {
				if !verifyCSRF(r, token, signingKey) {
					problem.Unauthorized("CSRFトークン不一致").Write(w, logger)
					return
				}
			}
			ctx := context.WithValue(r.Context(), csrfContextKey{}, token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func readCSRFCookie(r *http.Request) string {
	c, err := r.Cookie(CSRFCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func setCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	// gosec G124: secureフラグは呼び出し側から注入。開発時のみ false で運用する。
	// HttpOnly=true, SameSite=Lax は常に設定済み。
	c := &http.Cookie{ // #nosec G124 -- Secureは呼び出し側で切替
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(12 * time.Hour),
	}
	http.SetCookie(w, c)
}

// generateCSRFToken 32バイトのランダム値 + HMAC-SHA256 を base64url でつなげて返す
func generateCSRFToken(key []byte) string {
	buf := make([]byte, 32)
	_, _ = cryptorand.Read(buf)
	mac := hmac.New(sha256.New, key)
	mac.Write(buf)
	sig := mac.Sum(nil)
	// フォーマット: base64url(random) + "." + base64url(sig)
	return base64.RawURLEncoding.EncodeToString(buf) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// verifyCSRF Cookieのトークンと、リクエスト側（ヘッダ or フォーム）の値が一致し、
// かつ署名がsigningKeyから正しく計算されたものであることを検証する
func verifyCSRF(r *http.Request, cookieToken string, key []byte) bool {
	provided := r.Header.Get(CSRFHeaderName)
	if provided == "" {
		if err := r.ParseForm(); err == nil {
			provided = r.PostForm.Get(CSRFFormField)
		}
	}
	if provided == "" {
		return false
	}
	if !hmac.Equal([]byte(provided), []byte(cookieToken)) {
		return false
	}
	return validateSignature(cookieToken, key)
}

func validateSignature(token string, key []byte) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	random, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(random)
	return hmac.Equal(sig, mac.Sum(nil))
}

// -- ロギング -----------------------------------------------------------------

// AccessLog リクエストのメソッド・パス・ステータスをJSONログに残します。
// SSE等のFlusherを必要とするハンドラのために、ResponseWriterはFlusherを透過させます。
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := newStatusRecorder(w)
			next.ServeHTTP(rec, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status()),
				slog.Duration("dur", time.Since(started)),
			)
		})
	}
}

// statusRecorder ステータスコード保存 + Flusher/Hijacker 透過用のラッパ
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func newStatusRecorder(w http.ResponseWriter) *flushableRecorder {
	base := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	if fl, ok := w.(http.Flusher); ok {
		return &flushableRecorder{statusRecorder: base, fl: fl}
	}
	return &flushableRecorder{statusRecorder: base}
}

// Status 記録済みステータス
func (r *statusRecorder) Status() int { return r.status }

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// flushableRecorder http.Flusher を透過的に実装
type flushableRecorder struct {
	*statusRecorder
	fl http.Flusher
}

// Flush フラッシャがあれば呼ぶ
func (r *flushableRecorder) Flush() {
	if r.fl != nil {
		r.fl.Flush()
	}
}

// -- 便宜 --------------------------------------------------------------------

// WriteProblem 内部エラー用の便利関数
func WriteProblem(w http.ResponseWriter, logger *slog.Logger, p *problem.Problem) {
	p.Write(w, logger)
}

// ErrEmptySigningKey signingKeyが未設定
var ErrEmptySigningKey = errors.New("httpx: 空のsigning key")
