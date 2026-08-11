// Package logger は log/slog をベースにJSON構造化ログを出力します。
// カード番号（PANパターン）と Stripe シークレット風文字列を送信前にレダクトし、
// ログ経由でのカード情報流出（FR-D1）を機械的に防ぎます。
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// レダクト対象パターン。
// - 12〜19桁のカード番号相当（ハイフンやスペースを含む形式も検出）
// - Stripe シークレット風文字列（sk_test_/sk_live_ + [A-Za-z0-9]{16+})
var (
	panRe    = regexp.MustCompile(`\b(?:\d[ -]?){12,19}\b`)
	stripeRe = regexp.MustCompile(`\b(?:sk|rk|whsec)_(?:test|live)_[A-Za-z0-9]{16,}\b`)
)

const redactedMarker = "[REDACTED]"

// New Config.LogLevel 相当を受け取ってレダクト機能付きslog.Loggerを返します。
func New(levelStr string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	h := &redactHandler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})}
	return slog.New(h)
}

// Redact 外部利用のため公開します。ログ出力以外でも危険文字列の除去に使えます。
func Redact(s string) string {
	s = stripeRe.ReplaceAllString(s, redactedMarker)
	s = panRe.ReplaceAllStringFunc(s, func(match string) string {
		// 数字だけ抜き出して桁数を再確認します。ハイフンやスペースが混じった短い数字並びを誤検出しないためです。
		digits := 0
		for _, r := range match {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits < 12 {
			return match
		}
		return redactedMarker
	})
	return s
}

type redactHandler struct{ inner slog.Handler }

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	newRecord := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		newRecord.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, newRecord)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(redacted)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, Redact(a.Value.String()))
	}
	return a
}
