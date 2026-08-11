package admin

import (
	"sync"
	"time"
)

// RateLimiter ログイン試行のレート制限。アカウント単位・IP単位で使い分けます（03-basic-design 6章）。
// メモリ内カウンタで実装しています（単一プロセス想定。マルチノード化する場合はRedis等に差し替え可能）。
type RateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	events map[string][]time.Time
	now    func() time.Time
}

// NewRateLimiter windowとlimitを指定して構築
func NewRateLimiter(window time.Duration, limit int) *RateLimiter {
	return &RateLimiter{window: window, limit: limit, events: map[string][]time.Time{}, now: time.Now}
}

// Allow key（メール or IP）を1つ加算し、windowの中でlimit以内かを返す
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	kept := l.events[key][:0]
	for _, t := range l.events[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.events[key] = kept
	return len(kept) <= l.limit
}
