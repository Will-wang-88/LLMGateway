package compress

import (
	"sync"
	"time"
)

// RetrievalStore maps a marker hash to the original offloaded bytes so a
// dropped payload can be retrieved later (e.g. via GET /v1/retrieve/{hash}).
type RetrievalStore interface {
	// Put stores content and returns its hash (SHA-256[:12]). It is the
	// caller's responsibility to call this for each Marker returned by Compress.
	Put(content []byte) string
	// Get returns the stored content for a hash, or false if absent/expired.
	Get(hash string) ([]byte, bool)
}

type entry struct {
	content []byte
	expires time.Time
}

// MemoryRetrievalStore is an in-memory RetrievalStore with lazy TTL expiry.
type MemoryRetrievalStore struct {
	mu  sync.RWMutex
	m   map[string]entry
	ttl time.Duration
}

// NewMemoryRetrievalStore creates a store; ttl <= 0 falls back to 5 minutes.
func NewMemoryRetrievalStore(ttl time.Duration) *MemoryRetrievalStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &MemoryRetrievalStore{m: make(map[string]entry), ttl: ttl}
}

// Put stores content keyed by its content hash and returns that hash. Storing
// the same content twice simply refreshes its TTL.
func (s *MemoryRetrievalStore) Put(content []byte) string {
	h := contentHash(content)
	cp := make([]byte, len(content))
	copy(cp, content)
	s.mu.Lock()
	s.m[h] = entry{content: cp, expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return h
}

// Get returns stored content if present and unexpired. Expired entries are
// removed lazily on access.
func (s *MemoryRetrievalStore) Get(hash string) ([]byte, bool) {
	s.mu.RLock()
	e, ok := s.m[hash]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		s.mu.Lock()
		// Re-check under the write lock in case it was refreshed concurrently.
		if cur, still := s.m[hash]; still && time.Now().After(cur.expires) {
			delete(s.m, hash)
		}
		s.mu.Unlock()
		return nil, false
	}
	return e.content, true
}
