package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/okamyuji/kessai/internal/payment"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/problem"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/web/templates"
)

// CheckoutDeps CheckoutHandlerが必要とする依存
type CheckoutDeps struct {
	Logger     *slog.Logger
	Queries    *sqlc.Queries
	UseCase    *payment.UseCase
	IDGen      idgen.Generator
	PubKey     string
	StripeMock bool
	Tokusho    templates.TokushoSnapshot
	// WebhookHandler Stripe Webhook 受信用のハンドラ（未指定なら404）
	WebhookHandler http.Handler
	// AdminMux 管理画面用サブルータ（未指定なら/admin配下は404）
	AdminMux http.Handler
	// SSE イベントログ配信ハブ
	SSE *SSEHub
}

// NewMux ルーターを構築します
func NewMux(deps CheckoutDeps, csrfKey []byte, secureCookie bool, staticDir string) http.Handler {
	mux := http.NewServeMux()
	// ロードバランサ用の軽量ヘルスチェック（CSRF/DB非依存）
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", indexHandler(deps))
	mux.HandleFunc("GET /tokusho", tokushoHandler(deps))
	mux.HandleFunc("POST /checkout", checkoutSubmitHandler(deps))
	mux.HandleFunc("GET /pay/{payment_id}", confirmHandler(deps))
	mux.HandleFunc("GET /complete/{payment_id}", completeHandler(deps))
	// 静的ファイル配信
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	// Webhook 受信（CSRF検証を回避したいのでミドルウェア外で別ハンドラを持つ）
	if deps.SSE != nil {
		mux.HandleFunc("GET /admin/events", deps.SSE.Handler)
	}

	// 管理画面（AdminMuxで /admin プレフィックスを引き受ける）
	if deps.AdminMux != nil {
		mux.Handle("/admin/", http.StripPrefix("/admin", deps.AdminMux))
	}

	// ミドルウェア: セキュリティヘッダ→CSRF→アクセスログ
	stripeSrc := "https://js.stripe.com https://*.stripe.com"
	protected := AccessLog(deps.Logger)(NewCSRF(csrfKey, secureCookie, deps.Logger)(SecurityHeaders(stripeSrc)(mux)))

	// Webhook は CSRF/CSP なしで別ハンドラに割り当てる（署名検証で防御）
	if deps.WebhookHandler != nil {
		outer := http.NewServeMux()
		outer.Handle("POST /webhooks/stripe", AccessLog(deps.Logger)(deps.WebhookHandler))
		outer.Handle("/", protected)
		return outer
	}
	return protected
}

// indexHandler 商品1件目を出す。存在しなければ空表示。
func indexHandler(deps CheckoutDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		products, err := deps.Queries.ListProducts(r.Context(), 1)
		if err != nil {
			problem.Internal("商品取得失敗").Write(w, deps.Logger)
			return
		}
		if len(products) == 0 {
			problem.Validation("商品未登録").Write(w, deps.Logger)
			return
		}
		p := products[0]
		amount, err := money.New(p.PriceJpy)
		if err != nil {
			problem.Internal("価格不整合").Write(w, deps.Logger)
			return
		}
		token := CSRFTokenFromContext(r.Context())
		view := templates.ProductView{ID: p.ID, Name: p.Name, Price: amount}
		// 冪等性キーはこの描画時に発行してフォームへ埋め込む。二重送信は同一キーの
		// 再送となり、UseCase側のreplayで同じpayment_idに着地する（ADR-0007）
		if err := templates.CheckoutPage(deps.PubKey, view, deps.Tokusho, token, deps.IDGen.New()).Render(r.Context(), w); err != nil {
			deps.Logger.Error("render checkout", "err", err)
		}
	}
}

func tokushoHandler(deps CheckoutDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := templates.TokushoPage(deps.Tokusho).Render(r.Context(), w); err != nil {
			deps.Logger.Error("render tokusho", "err", err)
		}
	}
}

// idempotencyKeyRe サーバ発行キー（ULID）の形式。Crockford Base32の26文字
var idempotencyKeyRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// checkoutSubmitHandler POST /checkout: フォームの冪等性キーでPaymentIntent作成→/pay/:idへ302
func checkoutSubmitHandler(deps CheckoutDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			problem.Validation("フォーム解析失敗").Write(w, deps.Logger)
			return
		}
		productID := r.PostForm.Get("product_id")
		if productID == "" {
			problem.Validation("product_id必須").Write(w, deps.Logger)
			return
		}
		idemKey := r.PostForm.Get("idempotency_key")
		if !idempotencyKeyRe.MatchString(idemKey) {
			problem.Validation("idempotency_keyが不正").Write(w, deps.Logger)
			return
		}
		p, err := deps.Queries.GetProduct(r.Context(), productID)
		if err != nil {
			problem.Validation("商品が見つからない").Write(w, deps.Logger)
			return
		}
		amount, err := money.New(p.PriceJpy)
		if err != nil {
			problem.Internal("価格不整合").Write(w, deps.Logger)
			return
		}
		req := payment.StartCheckoutRequest{
			IdempotencyKey: idemKey,
			ProductID:      productID,
			Amount:         amount,
			Actor:          "customer",
		}
		res, err := deps.UseCase.StartCheckout(r.Context(), req)
		if err != nil {
			translatePaymentError(w, deps.Logger, err)
			return
		}
		// PaymentIDをQueryパラメータでは渡さず、パスパラメータとして302
		http.Redirect(w, r, fmt.Sprintf("/pay/%s", res.PaymentID), http.StatusSeeOther)
	}
}

// confirmHandler GET /pay/{payment_id}: idempotency_keysのsnapshotからclient_secretを再取得して表示
func confirmHandler(deps CheckoutDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		paymentID := r.PathValue("payment_id")
		clientSecret, product, err := loadPaymentForConfirm(r.Context(), deps, paymentID)
		if err != nil {
			translatePaymentError(w, deps.Logger, err)
			return
		}
		if renderErr := templates.ConfirmPage(deps.PubKey, clientSecret, paymentID, product, deps.Tokusho, deps.StripeMock).Render(r.Context(), w); renderErr != nil {
			deps.Logger.Error("render confirm", "err", renderErr)
		}
	}
}

func completeHandler(deps CheckoutDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		paymentID := r.PathValue("payment_id")
		if err := templates.CompletePage(paymentID).Render(r.Context(), w); err != nil {
			deps.Logger.Error("render complete", "err", err)
		}
	}
}

// loadPaymentForConfirm 決済IDから client_secret と商品ビューを取り出します。
// 冪等性キーの response_snapshot に保存した client_secret（payment.persistCheckoutResultで書き込む）を復元します。
func loadPaymentForConfirm(ctx context.Context, deps CheckoutDeps, paymentID string) (string, templates.ProductView, error) {
	if paymentID == "" {
		return "", templates.ProductView{}, errors.New("payment_id必須")
	}
	pay, err := deps.Queries.GetPayment(ctx, paymentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", templates.ProductView{}, errPaymentNotFound
		}
		return "", templates.ProductView{}, fmt.Errorf("get payment: %w", err)
	}
	prod, err := deps.Queries.GetProduct(ctx, pay.ProductID)
	if err != nil {
		return "", templates.ProductView{}, fmt.Errorf("get product: %w", err)
	}
	amount, err := money.New(pay.AmountJpy)
	if err != nil {
		return "", templates.ProductView{}, err
	}
	view := templates.ProductView{ID: prod.ID, Name: prod.Name, Price: amount}
	idem, err := deps.Queries.GetIdempotencyByPaymentID(ctx, &paymentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 冪等性キーの期限切れ削除後は決済ページを再表示できない
			return "", view, errPaymentNotFound
		}
		return "", view, fmt.Errorf("get idempotency response: %w", err)
	}
	if idem.ResponseSnapshot == nil {
		return "", view, errors.New("PaymentIntent未生成")
	}
	var checkout payment.StartCheckoutResult
	if err := json.Unmarshal(idem.ResponseSnapshot, &checkout); err != nil {
		return "", view, fmt.Errorf("decode checkout response: %w", err)
	}
	if checkout.ClientSecret == "" {
		return "", view, errors.New("client_secret未保存")
	}
	return checkout.ClientSecret, view, nil
}

// translatePaymentError payment/usecase の型エラーをRFC 9457へ変換
func translatePaymentError(w http.ResponseWriter, logger *slog.Logger, err error) {
	p := classifyPaymentError(err)
	if p == nil {
		logger.Error("unclassified", "err", err)
		p = problem.Internal("内部エラー")
	}
	p.Write(w, logger)
}

// TranslatePaymentErrorForTest テスト用の公開ラッパ。translatePaymentErrorを外部から呼びます。
func TranslatePaymentErrorForTest(w http.ResponseWriter, logger *slog.Logger, err error) {
	translatePaymentError(w, logger, err)
}

// classifyPaymentError paymentのsentinel error群をRFC 9457のProblemへ写します
// errPaymentNotFound 決済IDに対応する表示可能な決済が無い
var errPaymentNotFound = errors.New("決済が見つからない")

func classifyPaymentError(err error) *problem.Problem {
	switch {
	case errors.Is(err, errPaymentNotFound):
		return problem.Validation(err.Error())
	case errors.Is(err, payment.ErrIdempotencyConflict):
		return problem.IdempotencyConflict(err.Error())
	case errors.Is(err, payment.ErrIdempotencyInProgress):
		return problem.IdempotencyInProgress(err.Error())
	case errors.Is(err, stripeclient.ErrPSPUnavailable):
		return problem.PSPUnavailable(err.Error())
	}
	return nil
}
