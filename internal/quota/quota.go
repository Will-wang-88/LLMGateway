package quota

import (
	"sync"
	"time"
)

// Manager tracks per-key daily and monthly usage in memory.
// Counters auto-reset at UTC day / month boundaries.
type Manager struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	dayStart   time.Time
	monthStart time.Time
	dayReqs    int64
	monthReqs  int64
	dayToks    int64
	monthToks  int64
}

func New() *Manager {
	return &Manager{buckets: make(map[string]*bucket)}
}

// Check returns the violated field ("" if all ok).
// daily/monthly limits of 0 mean unlimited.
func (m *Manager) Check(keyID string, dailyReqLimit, dailyTokLimit, monthlyReqLimit, monthlyTokLimit int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.getLocked(keyID)
	m.rollLocked(b, time.Now())
	if dailyReqLimit > 0 && b.dayReqs >= dailyReqLimit {
		return "daily_request_quota_exceeded"
	}
	if monthlyReqLimit > 0 && b.monthReqs >= monthlyReqLimit {
		return "monthly_request_quota_exceeded"
	}
	if dailyTokLimit > 0 && b.dayToks >= dailyTokLimit {
		return "daily_token_quota_exceeded"
	}
	if monthlyTokLimit > 0 && b.monthToks >= monthlyTokLimit {
		return "monthly_token_quota_exceeded"
	}
	return ""
}

// AddRequest increments the request counters.
func (m *Manager) AddRequest(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.getLocked(keyID)
	m.rollLocked(b, time.Now())
	b.dayReqs++
	b.monthReqs++
}

// AddTokens increments the token counters.
func (m *Manager) AddTokens(keyID string, tokens int64) {
	if tokens <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.getLocked(keyID)
	m.rollLocked(b, time.Now())
	b.dayToks += tokens
	b.monthToks += tokens
}

// Usage returns the current counters for the key.
func (m *Manager) Usage(keyID string) (dayReqs, dayToks, monthReqs, monthToks int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.getLocked(keyID)
	m.rollLocked(b, time.Now())
	return b.dayReqs, b.dayToks, b.monthReqs, b.monthToks
}

func (m *Manager) getLocked(keyID string) *bucket {
	b, ok := m.buckets[keyID]
	if !ok {
		now := time.Now().UTC()
		b = &bucket{
			dayStart:   dayStart(now),
			monthStart: monthStart(now),
		}
		m.buckets[keyID] = b
	}
	return b
}

func (m *Manager) rollLocked(b *bucket, now time.Time) {
	now = now.UTC()
	if now.After(b.dayStart.Add(24 * time.Hour)) || now.Day() != b.dayStart.Day() || now.Month() != b.dayStart.Month() || now.Year() != b.dayStart.Year() {
		b.dayStart = dayStart(now)
		b.dayReqs = 0
		b.dayToks = 0
	}
	if now.Month() != b.monthStart.Month() || now.Year() != b.monthStart.Year() {
		b.monthStart = monthStart(now)
		b.monthReqs = 0
		b.monthToks = 0
	}
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
