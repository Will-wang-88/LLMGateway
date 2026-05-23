package balancer

import (
	"hash/fnv"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/will-wang-88/llmgateway/internal/store"
)

type Policy string

const (
	PolicyRoundRobin         Policy = "round_robin"
	PolicyWeightedRoundRobin Policy = "weighted_round_robin"
	PolicyLeastConnections   Policy = "least_connections"
	PolicyLeastLatency       Policy = "least_latency"
	PolicyRandom             Policy = "random"
	PolicyHash               Policy = "hash"
	PolicySticky             Policy = "sticky"
)

type Balancer struct {
	mu       sync.Mutex
	counters map[string]*uint64 // per-model RR counter

	stickyMu sync.Mutex
	sticky   map[string]string // "model|key" -> backendID
}

func New() *Balancer {
	return &Balancer{
		counters: make(map[string]*uint64),
		sticky:   make(map[string]string),
	}
}

func (b *Balancer) counter(key string) *uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.counters[key]
	if !ok {
		var v uint64
		c = &v
		b.counters[key] = c
	}
	return c
}

// Hint provides optional context used by hash/sticky policies.
type Hint struct {
	APIKeyID string
	User     string
}

// Choose picks one backend from candidates. Candidates should already be
// filtered to healthy, enabled and within concurrency budget.
func (b *Balancer) Choose(model string, policy Policy, candidates []*store.Backend, hint Hint) *store.Backend {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	switch policy {
	case PolicyRandom:
		return candidates[rand.Intn(len(candidates))]
	case PolicyLeastConnections:
		return chooseLeast(candidates)
	case PolicyLeastLatency:
		return chooseLeastLatency(candidates)
	case PolicyRoundRobin:
		idx := atomic.AddUint64(b.counter(model), 1) - 1
		return candidates[int(idx%uint64(len(candidates)))]
	case PolicyHash:
		return chooseHash(candidates, hint)
	case PolicySticky:
		return b.chooseSticky(model, candidates, hint)
	case PolicyWeightedRoundRobin, "":
		return chooseWeightedRR(candidates, b.counter(model))
	default:
		return chooseWeightedRR(candidates, b.counter(model))
	}
}

func chooseLeastLatency(cs []*store.Backend) *store.Backend {
	var best *store.Backend
	var bestLat int64 = -1
	for _, c := range cs {
		_, _, _, _, lat, _ := c.Stats()
		if best == nil || (lat > 0 && (bestLat < 0 || lat < bestLat)) {
			best = c
			if lat > 0 {
				bestLat = lat
			}
		}
	}
	return best
}

func chooseHash(cs []*store.Backend, hint Hint) *store.Backend {
	key := hint.APIKeyID + "|" + hint.User
	if key == "|" {
		return cs[0]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := int(h.Sum32() % uint32(len(cs)))
	return cs[idx]
}

func (b *Balancer) chooseSticky(model string, cs []*store.Backend, hint Hint) *store.Backend {
	if hint.APIKeyID == "" {
		return chooseWeightedRR(cs, b.counter(model))
	}
	key := model + "|" + hint.APIKeyID
	b.stickyMu.Lock()
	defer b.stickyMu.Unlock()
	if id, ok := b.sticky[key]; ok {
		for _, c := range cs {
			if c.ID == id {
				return c
			}
		}
	}
	picked := chooseWeightedRR(cs, b.counter(model))
	if picked != nil {
		b.sticky[key] = picked.ID
	}
	return picked
}

func chooseLeast(cs []*store.Backend) *store.Backend {
	var best *store.Backend
	var bestActive int64 = -1
	for _, c := range cs {
		a := c.ActiveRequests()
		if best == nil || a < bestActive {
			best = c
			bestActive = a
		}
	}
	return best
}

func chooseWeightedRR(cs []*store.Backend, counter *uint64) *store.Backend {
	totalWeight := 0
	for _, c := range cs {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
	}
	if totalWeight <= 0 {
		return cs[0]
	}
	idx := atomic.AddUint64(counter, 1) - 1
	target := int(idx % uint64(totalWeight))
	for _, c := range cs {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		if target < w {
			return c
		}
		target -= w
	}
	return cs[len(cs)-1]
}
