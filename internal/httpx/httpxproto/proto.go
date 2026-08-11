// Package httpxproto httpxパッケージとテンプレート間で共有するプロトコル定数を提供します。
// httpxはランタイム挙動を、templatesはHTML生成を担当し、両者がこの定数を参照します。
// このパッケージは外部依存を持たないため、循環インポートを起こしません。
package httpxproto

const (
	// CSRFCookieName CSRFトークンCookie名
	CSRFCookieName = "kessai_csrf"
	// CSRFHeaderName htmxのhx-headers等から送るヘッダ名
	CSRFHeaderName = "X-CSRF-Token"
	// CSRFFormField POSTフォームで送るhidden inputの名前
	CSRFFormField = "csrf_token"
)
