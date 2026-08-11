// Package money はJPY（円単位のBIGINT整数）を扱う小さな値型です（ADR-0006）。
// 通貨は当面JPYのみです。負値は許容しません（返金額の符号もJPY値では表現せず、勘定側で扱います）。
package money

import (
	"errors"
	"fmt"
	"strconv"
)

// JPY 0以上の円単位整数を表す値型です。
type JPY struct {
	amount int64
}

// ErrNegative 負値のJPYを生成しようとしたときに返るエラーです。
var ErrNegative = errors.New("money: 負値のJPYは表現できない")

// New 非負値からJPYを構築します。負値は ErrNegative を返します。
func New(amount int64) (JPY, error) {
	if amount < 0 {
		return JPY{}, ErrNegative
	}
	return JPY{amount: amount}, nil
}

// MustNew テストコード等での定数構築用です。負値ならpanicします。
func MustNew(amount int64) JPY {
	m, err := New(amount)
	if err != nil {
		panic(err)
	}
	return m
}

// Int64 保持している金額の整数値を返します。DBのBIGINTと直接マップします。
func (m JPY) Int64() int64 { return m.amount }

// String 「1,234円」形式の日本語表記を返します。管理画面や監査ログで使います。
func (m JPY) String() string {
	s := strconv.FormatInt(m.amount, 10)
	// 3桁カンマ区切りを手書き実装します。負値はNewで弾いているため考慮不要です。
	n := len(s)
	if n <= 3 {
		return s + "円"
	}
	buf := make([]byte, 0, n+n/3+3)
	firstGroup := n % 3
	if firstGroup == 0 {
		firstGroup = 3
	}
	buf = append(buf, s[:firstGroup]...)
	for i := firstGroup; i < n; i += 3 {
		buf = append(buf, ',')
		buf = append(buf, s[i:i+3]...)
	}
	buf = append(buf, "円"...)
	return string(buf)
}

// Add 加算した新しいJPYを返します。オーバーフロー時はエラーを返します。
func (m JPY) Add(o JPY) (JPY, error) {
	sum := m.amount + o.amount
	if sum < m.amount || sum < o.amount {
		return JPY{}, fmt.Errorf("money: 加算オーバーフロー %d + %d", m.amount, o.amount)
	}
	return JPY{amount: sum}, nil
}

// Sub 減算した新しいJPYを返します。結果が負になる場合はエラーです。
func (m JPY) Sub(o JPY) (JPY, error) {
	if o.amount > m.amount {
		return JPY{}, fmt.Errorf("money: 減算結果が負になる %d - %d", m.amount, o.amount)
	}
	return JPY{amount: m.amount - o.amount}, nil
}

// Equal 同値比較です。
func (m JPY) Equal(o JPY) bool { return m.amount == o.amount }

// LessThan m < o を判定します。
func (m JPY) LessThan(o JPY) bool { return m.amount < o.amount }
