package ratelimit

import (
	"sync"
	"time"
)

// Limiter implements per-key fixed-window request, token, daily-request and
// daily-token counters. Counters reset on window expiry (1 minute for the
// rolling window; midnight UTC for the daily window).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	mu          sync.Mutex
	windowStart time.Time
	dayStart    time.Time
	count       int
	tokens      int64
	dayCount    int64
	dayTokens   int64
}

func New() *Limiter {
	l := &Limiter{buckets: make(map[string]*bucket)}
	go l.gcLoop()
	return l
}

func (l *Limiter) get(key string) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		now := time.Now()
		b = &bucket{windowStart: now, dayStart: utcDayStart(now)}
		l.buckets[key] = b
	}
	return b
}

func (l *Limiter) gcLoop() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		now := time.Now()
		for k, b := range l.buckets {
			b.mu.Lock()
			// Reap if the bucket has been idle for >5min AND its daily
			// counter is empty (so we don't lose quota state mid-day).
			idle := now.Sub(b.windowStart) > 5*time.Minute && b.dayCount == 0 && b.dayTokens == 0
			b.mu.Unlock()
			if idle {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

func (b *bucket) rollLocked(now time.Time) {
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart = now
		b.count = 0
		b.tokens = 0
	}
	day := utcDayStart(now)
	if !day.Equal(b.dayStart) {
		b.dayStart = day
		b.dayCount = 0
		b.dayTokens = 0
	}
}

func utcDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// CheckAndReserve atomically validates ALL rate-limit dimensions for the key
// and, only if every check passes, increments the request counters.
//
// Limits of 0 mean unlimited. Returns the violated dimension code, or "" if
// the request is admitted.
func (l *Limiter) CheckAndReserve(key string, rpm int, tpm int64, dailyReq int64, dailyTok int64) string {
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.rollLocked(now)

	if rpm > 0 && b.count >= rpm {
		return "rate_limit_exceeded"
	}
	if tpm > 0 && b.tokens >= tpm {
		return "token_rate_limit_exceeded"
	}
	if dailyReq > 0 && b.dayCount >= dailyReq {
		return "daily_request_limit_exceeded"
	}
	if dailyTok > 0 && b.dayTokens >= dailyTok {
		return "daily_token_limit_exceeded"
	}
	b.count++
	b.dayCount++
	return ""
}

// AddTokens adds usage tokens to the current windows (per-minute + daily).
func (l *Limiter) AddTokens(key string, tokens int64) {
	if tokens <= 0 {
		return
	}
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(time.Now())
	b.tokens += tokens
	b.dayTokens += tokens
}

// Stats returns the current per-minute and per-day counters.
func (l *Limiter) Stats(key string) (rpmCount int, tpmTokens int64, dayCount int64, dayTokens int64) {
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(time.Now())
	return b.count, b.tokens, b.dayCount, b.dayTokens
}

// Concurrency tracks in-flight request counts per key. Keys with limit <= 0
// (unlimited) are not tracked to avoid unbounded map growth.
type Concurrency struct {
	mu      sync.Mutex
	current map[string]int
}

func NewConcurrency() *Concurrency {
	return &Concurrency{current: make(map[string]int)}
}

// Acquire returns true and reserves a slot. For unlimited keys we always
// return true without recording anything (so the map cannot leak).
func (c *Concurrency) Acquire(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current[key] >= limit {
		return false
	}
	c.current[key]++
	return true
}

// Release decrements the slot for a previously-acquired key. Callers must
// pass the same limit value they passed to Acquire so unlimited keys (which
// were not recorded) are a no-op here too.
func (c *Concurrency) Release(key string, limit int) {
	if limit <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current[key] > 0 {
		c.current[key]--
	}
	if c.current[key] == 0 {
		delete(c.current, key)
	}
}

// Get returns the current concurrency count for the key.
func (c *Concurrency) Get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current[key]
}
