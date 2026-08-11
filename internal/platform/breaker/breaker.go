// Package breaker 外部呼び出し用のCircuit Breakerを提供します（ADR-0010）。
// 3状態（Closed→Open→HalfOpen）遷移をテーブル駆動で表現し、
// Open中は即座に失敗を返して呼び出しを遮断します。
// Half-Openでは限定回数の試行を許可し、成功でClosedへ、失敗でOpenへ戻ります。
package breaker

import (
	"errors"
	"sync"
	"time"
)

// State ブレーカー状態
type State int

const (
	// StateClosed 通常状態。呼び出しは通過し、失敗を計数する
	StateClosed State = iota
	// StateOpen 遮断状態。即座に失敗を返し、外部を呼ばない
	StateOpen
	// StateHalfOpen 試行状態。限られた回数だけ通過を許可する
	StateHalfOpen
)

// String 診断表示用
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// Config Circuit Breakerの動作パラメータ
type Config struct {
	// FailureWindow 失敗率を計算する試行数窓
	FailureWindow int
	// FailureThreshold FailureWindow内の失敗数がこの値以上になったらOpenへ遷移
	FailureThreshold int
	// OpenDuration Open状態を維持する時間
	OpenDuration time.Duration
	// HalfOpenMaxProbes Half-Openで許可する試行回数
	HalfOpenMaxProbes int
	// Now 時刻取得（テストで差し替え可能）。nilなら time.Now を使う
	Now func() time.Time
}

// DefaultConfig 設計上の既定値（03-basic-design 2.2節）
func DefaultConfig() Config {
	return Config{
		FailureWindow:     10,
		FailureThreshold:  5,
		OpenDuration:      30 * time.Second,
		HalfOpenMaxProbes: 1,
		Now:               time.Now,
	}
}

// ErrOpen ブレーカーがOpenのため呼び出しが遮断された
var ErrOpen = errors.New("breaker: open state")

// Breaker スレッドセーフなCircuit Breaker実装
type Breaker struct {
	cfg           Config
	mu            sync.Mutex
	state         State
	outcomes      []bool // 直近FailureWindow件の結果（true=成功、false=失敗）
	openedAt      time.Time
	probesAllowed int
	probesRunning int
}

// New Configを使ってBreakerを構築します
func New(cfg Config) *Breaker {
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = 10
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	if cfg.HalfOpenMaxProbes <= 0 {
		cfg.HalfOpenMaxProbes = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Breaker{cfg: cfg, state: StateClosed}
}

// State 現在のブレーカー状態を返します。副作用としてOpen→HalfOpen遷移の判定を行います
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionOpenToHalfOpenLocked()
	return b.state
}

// Allow 呼び出し可否を判定します。可なら trueと、結果報告用の Record を返します
// Openなら false と ErrOpen を返します
func (b *Breaker) Allow() (permit *Permit, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionOpenToHalfOpenLocked()
	switch b.state {
	case StateOpen:
		return nil, ErrOpen
	case StateHalfOpen:
		if b.probesRunning >= b.probesAllowed {
			return nil, ErrOpen
		}
		b.probesRunning++
	}
	return &Permit{b: b}, nil
}

// Permit 呼び出し許可トークン。結果は Success か Failure で必ず報告する
type Permit struct {
	b        *Breaker
	reported bool
}

// Success 呼び出しが成功したことを報告します
func (p *Permit) Success() {
	if p == nil || p.reported {
		return
	}
	p.reported = true
	p.b.record(true)
}

// Failure 呼び出しが失敗したことを報告します
func (p *Permit) Failure() {
	if p == nil || p.reported {
		return
	}
	p.reported = true
	p.b.record(false)
}

func (b *Breaker) record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateHalfOpen:
		if b.probesRunning > 0 {
			b.probesRunning--
		}
		if success {
			b.transitionToClosedLocked()
		} else {
			b.transitionToOpenLocked()
		}
	case StateClosed:
		b.outcomes = append(b.outcomes, success)
		if len(b.outcomes) > b.cfg.FailureWindow {
			b.outcomes = b.outcomes[len(b.outcomes)-b.cfg.FailureWindow:]
		}
		failures := 0
		for _, ok := range b.outcomes {
			if !ok {
				failures++
			}
		}
		if failures >= b.cfg.FailureThreshold {
			b.transitionToOpenLocked()
		}
	case StateOpen:
		// Openでrecordが呼ばれるのはtransition直前に取ったPermitに限られる。
		// この場合はhalf-open判定にゆだねる。
	}
}

func (b *Breaker) transitionOpenToHalfOpenLocked() {
	if b.state != StateOpen {
		return
	}
	if b.cfg.Now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.state = StateHalfOpen
		b.probesAllowed = b.cfg.HalfOpenMaxProbes
		b.probesRunning = 0
	}
}

func (b *Breaker) transitionToOpenLocked() {
	b.state = StateOpen
	b.openedAt = b.cfg.Now()
	b.outcomes = nil
	b.probesAllowed = 0
	b.probesRunning = 0
}

func (b *Breaker) transitionToClosedLocked() {
	b.state = StateClosed
	b.outcomes = nil
	b.probesAllowed = 0
	b.probesRunning = 0
}
