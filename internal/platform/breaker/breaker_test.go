package breaker_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/okamyuji/kessai/internal/platform/breaker"
)

// fakeClock 時刻を注入できる時計
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newBreaker(clk *fakeClock) *breaker.Breaker {
	return breaker.New(breaker.Config{
		FailureWindow:     5,
		FailureThreshold:  3,
		OpenDuration:      time.Second,
		HalfOpenMaxProbes: 1,
		Now:               clk.Now,
	})
}

func TestState_String(t *testing.T) {
	t.Parallel()
	cases := map[breaker.State]string{
		breaker.StateClosed:   "closed",
		breaker.StateOpen:     "open",
		breaker.StateHalfOpen: "half_open",
		breaker.State(99):     "unknown",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String()=%q want %q", int(s), got, want)
		}
	}
}

func TestClosed_FailuresBelowThreshold_StayClosed(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	for range 2 {
		p, err := b.Allow()
		if err != nil {
			t.Fatalf("Allow err=%v", err)
		}
		p.Failure()
	}
	if b.State() != breaker.StateClosed {
		t.Fatalf("state=%s want closed", b.State())
	}
}

func TestClosed_FailuresReachThreshold_Opens(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	for range 3 {
		p, err := b.Allow()
		if err != nil {
			t.Fatalf("Allow err=%v", err)
		}
		p.Failure()
	}
	if b.State() != breaker.StateOpen {
		t.Fatalf("state=%s want open", b.State())
	}
	if _, err := b.Allow(); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("Allow after open err=%v want ErrOpen", err)
	}
}

func TestOpen_TimerExpires_HalfOpen(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	for range 3 {
		p, _ := b.Allow()
		p.Failure()
	}
	// Openになる。時間を進めるとhalf-openへ遷移
	clk.Advance(time.Second)
	if b.State() != breaker.StateHalfOpen {
		t.Fatalf("state=%s want half_open", b.State())
	}
}

func TestHalfOpen_ProbeSuccess_ReturnsClosed(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	for range 3 {
		p, _ := b.Allow()
		p.Failure()
	}
	clk.Advance(time.Second)
	// half-open状態でprobeを1つ許可される
	p, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open Allow err=%v", err)
	}
	// 追加のAllowはprobes上限のためErrOpen
	if _, err := b.Allow(); !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("second probe err=%v want ErrOpen", err)
	}
	p.Success()
	if b.State() != breaker.StateClosed {
		t.Fatalf("state=%s want closed", b.State())
	}
}

func TestHalfOpen_ProbeFailure_ReturnsOpen(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	for range 3 {
		p, _ := b.Allow()
		p.Failure()
	}
	clk.Advance(time.Second)
	p, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow err=%v", err)
	}
	p.Failure()
	if b.State() != breaker.StateOpen {
		t.Fatalf("state=%s want open", b.State())
	}
}

func TestPermit_DoubleReport_Ignored(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	p, _ := b.Allow()
	p.Success()
	p.Success() // 2回目は無視される
	p.Failure() // これも無視
	if b.State() != breaker.StateClosed {
		t.Fatalf("state=%s want closed", b.State())
	}
}

func TestDefaultConfig_Values(t *testing.T) {
	t.Parallel()
	c := breaker.DefaultConfig()
	if c.FailureWindow != 10 || c.FailureThreshold != 5 {
		t.Fatalf("既定値: window=%d threshold=%d", c.FailureWindow, c.FailureThreshold)
	}
	if c.OpenDuration != 30*time.Second {
		t.Fatalf("既定OpenDuration=%v want 30s", c.OpenDuration)
	}
	if c.HalfOpenMaxProbes != 1 {
		t.Fatalf("既定HalfOpenMaxProbes=%d want 1", c.HalfOpenMaxProbes)
	}
	if c.Now == nil {
		t.Fatalf("既定Nowはnilではない")
	}
}

func TestNew_AppliesDefaultsForZeroConfig(t *testing.T) {
	t.Parallel()
	b := breaker.New(breaker.Config{})
	if b.State() != breaker.StateClosed {
		t.Fatalf("初期状態はclosed: got %s", b.State())
	}
}

func TestConcurrent_NoRace(t *testing.T) {
	// -raceで実行されることでレース検出を確認
	t.Parallel()
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(clk)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if p, err := b.Allow(); err == nil {
				p.Success()
			}
		})
	}
	wg.Wait()
	_ = b.State()
}
