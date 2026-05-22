package balancer

import (
	"testing"

	"github.com/will-wang-88/llmgateway/internal/store"
)

func TestWeightedRoundRobinDistribution(t *testing.T) {
	b := New()
	b1 := &store.Backend{ID: "a", Weight: 3, Enabled: true}
	b2 := &store.Backend{ID: "b", Weight: 1, Enabled: true}
	b1.SetStatus(store.StatusHealthy)
	b2.SetStatus(store.StatusHealthy)
	cs := []*store.Backend{b1, b2}

	count := map[string]int{}
	const n = 4000
	for i := 0; i < n; i++ {
		picked := b.Choose("m", PolicyWeightedRoundRobin, cs)
		count[picked.ID]++
	}
	// expected ratio ~3:1
	a, bn := count["a"], count["b"]
	if a == 0 || bn == 0 {
		t.Fatalf("expected both backends to receive traffic: a=%d b=%d", a, bn)
	}
	ratio := float64(a) / float64(bn)
	if ratio < 2.5 || ratio > 3.5 {
		t.Errorf("expected ratio ~3, got %.2f (a=%d b=%d)", ratio, a, bn)
	}
}

func TestRoundRobinEven(t *testing.T) {
	b := New()
	b1 := &store.Backend{ID: "a", Weight: 1, Enabled: true}
	b2 := &store.Backend{ID: "b", Weight: 1, Enabled: true}
	b3 := &store.Backend{ID: "c", Weight: 1, Enabled: true}
	cs := []*store.Backend{b1, b2, b3}

	count := map[string]int{}
	for i := 0; i < 3000; i++ {
		count[b.Choose("m", PolicyRoundRobin, cs).ID]++
	}
	for id, c := range count {
		if c < 900 || c > 1100 {
			t.Errorf("expected ~1000 for %s, got %d", id, c)
		}
	}
}

func TestLeastConnections(t *testing.T) {
	b := New()
	b1 := &store.Backend{ID: "a", Weight: 1, Enabled: true}
	b2 := &store.Backend{ID: "b", Weight: 1, Enabled: true}
	b1.AcquireSlot()
	b1.AcquireSlot()
	b1.AcquireSlot()
	cs := []*store.Backend{b1, b2}
	for i := 0; i < 5; i++ {
		picked := b.Choose("m", PolicyLeastConnections, cs)
		if picked.ID != "b" {
			t.Errorf("expected b (less loaded), got %s", picked.ID)
		}
	}
}

func TestChooseEmpty(t *testing.T) {
	b := New()
	if b.Choose("m", PolicyWeightedRoundRobin, nil) != nil {
		t.Error("expected nil for empty candidates")
	}
}
