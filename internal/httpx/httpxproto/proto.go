// Package httpxproto httpxパッケージとテンプレート間で共有するプロトコル定数を提供します。
// httpxはランタイム挙動を、templatesはHTML生成を担当し、両者がこの定数を参照します。
// このパッケージは外部依存を持たないため、循環インポートを起こしません。
package httpxproto

import "context"

const (
	// CSRFCookieName CSRFトークンCookie名
	CSRFCookieName = "kessai_csrf"
	// CSRFHeaderName htmxのhx-headers等から送るヘッダ名
	CSRFHeaderName = "X-CSRF-Token"
	// CSRFFormField POSTフォームで送るhidden inputの名前
	CSRFFormField = "csrf_token"
)

// csrfTokenContextKey CSRFトークンをContextへ載せる共有キー。
// httpxのミドルウェアが書き込み、adminなど別パッケージのハンドラが読む。
// 初回アクセスではCookieがまだリクエストに無いため、Contextがトークンの唯一の受け渡し経路になる。
type csrfTokenContextKey struct{}

// WithCSRFToken CSRFトークンを載せたContextを返します
func WithCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfTokenContextKey{}, token)
}

// CSRFTokenFromContext Contextから現在のCSRFトークンを取得します
func CSRFTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(csrfTokenContextKey{}).(string)
	return v
}
