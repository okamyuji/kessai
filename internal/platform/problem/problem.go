// Package problem RFC 9457 Problem Detailsに従ったJSONエラー応答を扱います（ADR-0012）。
// detailには内部情報（SQL・スタックトレース・PSP生エラー）を含めません。
// 拡張メンバー retryable（真偽値）と payment_id を定義しています。
package problem

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// TypeURI RFC 9457のtypeメンバー用URI
type TypeURI string

// カタログ 03-basic-design 5章に対応
const (
	TypeIdempotencyConflict   TypeURI = "/problems/idempotency-conflict"
	TypeIdempotencyInProgress TypeURI = "/problems/idempotency-in-progress"
	TypeInvalidTransition     TypeURI = "/problems/invalid-transition"
	TypeRefundExceedsAmount   TypeURI = "/problems/refund-exceeds-amount"
	TypePSPUnavailable        TypeURI = "/problems/psp-unavailable"
	TypeValidation            TypeURI = "/problems/validation"
	TypeUnauthorized          TypeURI = "/problems/unauthorized"
	TypeInternal              TypeURI = "/problems/internal"
)

// ContentType RFC 9457が定めるメディアタイプ
const ContentType = "application/problem+json"

// Problem RFC 9457のProblem Details表現
type Problem struct {
	Type      TypeURI `json:"type"`
	Title     string  `json:"title"`
	Status    int     `json:"status"`
	Detail    string  `json:"detail,omitempty"`
	Instance  string  `json:"instance,omitempty"`
	Retryable bool    `json:"retryable"`
	PaymentID string  `json:"payment_id,omitempty"`
}

// New Problemを組み立てて返します
func New(t TypeURI, title string, status int, detail string) *Problem {
	return &Problem{Type: t, Title: title, Status: status, Detail: detail}
}

// WithInstance instanceメンバーを設定します
func (p *Problem) WithInstance(instance string) *Problem { p.Instance = instance; return p }

// WithRetryable retryable拡張メンバーを設定します
func (p *Problem) WithRetryable(v bool) *Problem { p.Retryable = v; return p }

// WithPaymentID payment_id拡張メンバーを設定します
func (p *Problem) WithPaymentID(id string) *Problem { p.PaymentID = id; return p }

// Write ProblemをHTTPレスポンスとして送出します
func (p *Problem) Write(w http.ResponseWriter, logger *slog.Logger) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil && logger != nil {
		logger.Error("problem書き出しに失敗", "type", string(p.Type), "err", err)
	}
}

// IdempotencyConflict 冪等性キーの本文不一致
func IdempotencyConflict(detail string) *Problem {
	return New(TypeIdempotencyConflict, "冪等性キーの本文が既存と異なる", http.StatusConflict, detail).WithRetryable(false)
}

// IdempotencyInProgress 先行リクエストが処理中
func IdempotencyInProgress(detail string) *Problem {
	return New(TypeIdempotencyInProgress, "同一冪等性キーの先行リクエストが処理中", http.StatusConflict, detail).WithRetryable(true)
}

// InvalidTransition 状態遷移テーブルにない要求
func InvalidTransition(detail string) *Problem {
	return New(TypeInvalidTransition, "許可されていない状態遷移", http.StatusConflict, detail).WithRetryable(false)
}

// RefundExceedsAmount 返金累計が取引金額を超過
func RefundExceedsAmount(detail string) *Problem {
	return New(TypeRefundExceedsAmount, "返金累計が取引金額を超過", http.StatusUnprocessableEntity, detail).WithRetryable(false)
}

// PSPUnavailable Circuit BreakerがopenかPSP障害
func PSPUnavailable(detail string) *Problem {
	return New(TypePSPUnavailable, "決済代行サービスが一時的に利用できない", http.StatusServiceUnavailable, detail).WithRetryable(true)
}

// Validation 入力検証エラー
func Validation(detail string) *Problem {
	return New(TypeValidation, "入力検証エラー", http.StatusBadRequest, detail).WithRetryable(false)
}

// Unauthorized 未認証
func Unauthorized(detail string) *Problem {
	return New(TypeUnauthorized, "認証が必要", http.StatusUnauthorized, detail).WithRetryable(false)
}

// Internal 内部エラー。detailは呼び出し側で汎化した安全な文言のみ渡すこと
func Internal(detail string) *Problem {
	return New(TypeInternal, "内部エラー", http.StatusInternalServerError, detail).WithRetryable(true)
}
