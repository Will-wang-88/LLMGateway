package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTimeout is returned when a request waits longer than its queue timeout.
var ErrTimeout = errors.New("queue timeout")

// ErrFull is returned when a queue is at capacity.
var ErrFull = errors.New("queue full")

// Manager is a per-scope (typically per-model) wait-list. Requests acquire a
// slot before being routed to a backend; this provides backpressure when all
// backends are saturated.
type Manager struct {
	mu             sync.Mutex
	scopes         map[string]*scope
	defaultTimeout time.Duration
	defaultMaxSize int
	defaultMaxConc int
}

type scope struct {
	sem         chan struct{}
	maxSize     int
	queued      int64
	maxConc     int
	defaultWait time.Duration
}

func New(defaultTimeoutMS, defaultMaxSize, defaultMaxConc int) *Manager {
	if defaultTimeoutMS <= 0 {
		defaultTimeoutMS = 30000
	}
	if defaultMaxSize <= 0 {
		defaultMaxSize = 1000
	}
	return &Manager{
		scopes:         make(map[string]*scope),
		defaultTimeout: time.Duration(defaultTimeoutMS) * time.Millisecond,
		defaultMaxSize: defaultMaxSize,
		defaultMaxConc: defaultMaxConc,
	}
}

func (m *Manager) get(scopeID string) *scope {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.scopes[scopeID]
	if !ok {
		maxConc := m.defaultMaxConc
		if maxConc <= 0 {
			maxConc = 64
		}
		s = &scope{
			sem:         make(chan struct{}, maxConc),
			maxSize:     m.defaultMaxSize,
			maxConc:     maxConc,
			defaultWait: m.defaultTimeout,
		}
		m.scopes[scopeID] = s
	}
	return s
}

// Acquire blocks until a slot is available or the queue timeout fires.
// Returns nil on success and a release function. The release function MUST be
// called by the caller, normally via defer.
func (m *Manager) Acquire(ctx context.Context, scopeID string) (func(), error) {
	s := m.get(scopeID)
	queued := atomic.AddInt64(&s.queued, 1)
	defer atomic.AddInt64(&s.queued, -1)
	if s.maxSize > 0 && queued > int64(s.maxSize) {
		return nil, ErrFull
	}
	timer := time.NewTimer(s.defaultWait)
	defer timer.Stop()
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-timer.C:
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Depth returns the current pending count for a scope.
func (m *Manager) Depth(scopeID string) int64 {
	m.mu.Lock()
	s, ok := m.scopes[scopeID]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	return atomic.LoadInt64(&s.queued)
}

// Scopes returns the current scope ids and their depths.
func (m *Manager) Scopes() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.scopes))
	for k, v := range m.scopes {
		out[k] = atomic.LoadInt64(&v.queued)
	}
	return out
}
