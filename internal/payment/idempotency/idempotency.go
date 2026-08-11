// Package idempotency 冪等性キーの派生とリクエストハッシュを提供します（ADR-0007）。
// Stripeの冪等性キーはエンドポイント+パラメータの組で照合されるため、
// 決済1件のマスターキーから操作種別ごとに派生キーを導出して使います。
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Operation Stripeへの操作種別
type Operation string

const (
	// OpCreate PaymentIntent作成
	OpCreate Operation = "create"
	// OpCapture キャプチャ
	OpCapture Operation = "capture"
	// OpRefund 返金（returnはGoの予約語なのでOpRefundとする）
	OpRefund Operation = "refund"
)

// ErrInvalidMasterKey マスターキーが空文字
var ErrInvalidMasterKey = errors.New("idempotency: マスターキーは空にできない")

// ErrInvalidRefundSeq 返金連番が非正数
var ErrInvalidRefundSeq = errors.New("idempotency: 返金連番は1以上")

// Derive マスターキーと操作種別から派生キーを返します
// 返金は連番付きで {key}:refund:{seq} とします
func Derive(master string, op Operation, refundSeq int) (string, error) {
	if master == "" {
		return "", ErrInvalidMasterKey
	}
	switch op {
	case OpCreate, OpCapture:
		return fmt.Sprintf("%s:%s", master, op), nil
	case OpRefund:
		if refundSeq < 1 {
			return "", ErrInvalidRefundSeq
		}
		return fmt.Sprintf("%s:%s:%d", master, op, refundSeq), nil
	}
	return "", fmt.Errorf("idempotency: 未対応の操作 %q", op)
}

// HashRequest リクエスト本文（既に正規化・シリアライズ済みバイト列）のSHA-256ハッシュを返します
// idempotency_keysテーブルのrequest_hash列と同一リクエストか判定するために使います
func HashRequest(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

// HashRequestHex 上記の16進表記
func HashRequestHex(body []byte) string { return hex.EncodeToString(HashRequest(body)) }

// EqualHash 2つのハッシュを定数時間で比較します（keyの照合はセキュリティ観点で定数時間が望ましい）
func EqualHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// NormalizeKey 前後空白を除去し、キー長を検証します（ULID 26文字を想定）
func NormalizeKey(k string) (string, error) {
	trimmed := strings.TrimSpace(k)
	if trimmed == "" {
		return "", ErrInvalidMasterKey
	}
	return trimmed, nil
}
