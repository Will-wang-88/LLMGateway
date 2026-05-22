package balancer

import (
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
	PolicyRandom             Policy = "random"
)

type Balancer struct {
	mu       sync.Mutex
	counters map[string]*uint64 // per-model RR counter
}

func New() *Balancer {
	return &Balancer{counters: make(map[string]*uint64)}
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

// Choose picks one backend from candidates. Candidates should already be filtered
// to healthy, enabled and within concurrency budget.
func (b *Balancer) Choose(model string, policy Policy, candidates []*store.Backend) *store.Backend {
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
	case PolicyRoundRobin:
		idx := atomic.AddUint64(b.counter(model), 1) - 1
		return candidates[int(idx%uint64(len(candidates)))]
	case PolicyWeightedRoundRobin, "":
		return chooseWeightedRR(candidates, b.counter(model))
	default:
		return chooseWeightedRR(candidates, b.counter(model))
	}
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
