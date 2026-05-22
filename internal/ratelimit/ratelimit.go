package ratelimit

import (
	"sync"
	"time"
)

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	mu         sync.Mutex
	windowStart time.Time
	count      int
	tokens     int64
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
		b = &bucket{windowStart: time.Now()}
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
			if now.Sub(b.windowStart) > 5*time.Minute {
				delete(l.buckets, k)
			}
			b.mu.Unlock()
		}
		l.mu.Unlock()
	}
}

// AllowRequest returns false if the per-minute request limit is exceeded.
// limit <= 0 means unlimited.
func (l *Limiter) AllowRequest(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart = now
		b.count = 0
		b.tokens = 0
	}
	if b.count >= limit {
		return false
	}
	b.count++
	return true
}

// AddTokens adds usage tokens to the current window for the given key.
func (l *Limiter) AddTokens(key string, tokens int64) {
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart = now
		b.count = 0
		b.tokens = 0
	}
	b.tokens += tokens
}

// TokensRemaining returns whether the token-per-minute limit has been exceeded.
func (l *Limiter) AllowTokens(key string, limit int64) bool {
	if limit <= 0 {
		return true
	}
	b := l.get(key)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.windowStart) >= time.Minute {
		b.windowStart = now
		b.count = 0
		b.tokens = 0
	}
	return b.tokens < limit
}

type Concurrency struct {
	mu      sync.Mutex
	current map[string]int
}

func NewConcurrency() *Concurrency {
	return &Concurrency{current: make(map[string]int)}
}

func (c *Concurrency) Acquire(key string, limit int) bool {
	if limit <= 0 {
		c.mu.Lock()
		c.current[key]++
		c.mu.Unlock()
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

func (c *Concurrency) Release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current[key] > 0 {
		c.current[key]--
	}
}

func (c *Concurrency) Get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current[key]
}
