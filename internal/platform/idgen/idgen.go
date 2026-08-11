// Package idgen はULIDを主キーとして生成します（ADR-0004）。
// ULIDは26文字の文字列で時系列ソート可能なため、payments・ledger等の追記中心テーブルに向きます。
package idgen

import (
	cryptorand "crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Generator ULID生成のインターフェースです。テスト時に差し替え可能にします。
type Generator interface {
	New() string
}

type defaultGenerator struct {
	mu      sync.Mutex
	entropy io.Reader
}

// NewDefault crypto/randをエントロピー源とした生成器を返します。
// 単調性（同一ミリ秒内でも増加）は保証しませんが、payments用途では衝突確率は事実上ゼロです。
func NewDefault() Generator {
	return &defaultGenerator{entropy: cryptorand.Reader}
}

func (g *defaultGenerator) New() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	// ミリ秒精度のULIDを生成します。crypto/randからの16バイトが末尾ランダム部です。
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), g.entropy).String()
}
