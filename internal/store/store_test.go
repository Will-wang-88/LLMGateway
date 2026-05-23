package store

import (
	"testing"

	"github.com/will-wang-88/llmgateway/internal/config"
)

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"llama-3.1-70b", "llama-3.1-70b", true},
		{"llama-3.1-70b", "llama-3.1-8b", false},
		{"llama-*", "llama-3.1-70b", true},
		{"llama-*", "qwen-72b", false},
		{"qwen-*", "qwen-72b", true},
		{"qwen-*", "Qwen-72b", false},
		// general wildcards
		{"*-prod", "model-prod", true},
		{"*-prod", "model-test", false},
		{"gpt-*-mini", "gpt-4-mini", true},
		{"gpt-*-mini", "gpt-4-large", false},
		{"gpt-*-mini", "claude-mini", false},
		{"*-*", "a-b", true},
		{"a*b*c", "a___b___c", true},
		{"a*b*c", "a___c", false},
		// empty pattern is never a match
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.value); got != c.want {
			t.Errorf("matchPattern(%q,%q)=%v want %v", c.pattern, c.value, got, c.want)
		}
	}
}

func TestResolveAliasChains(t *testing.T) {
	s := New("")
	s.UpsertModelAlias(&ModelAlias{Alias: "a", InternalModel: "b", ForwardingMode: "use_internal", Enabled: true})
	s.UpsertModelAlias(&ModelAlias{Alias: "b", InternalModel: "c", ForwardingMode: "use_internal", Enabled: true})
	s.UpsertModelAlias(&ModelAlias{Alias: "c", InternalModel: "real-model", ForwardingMode: "use_internal", Enabled: true})

	got, forward := s.ResolveAlias("a")
	if got != "real-model" || forward != "real-model" {
		t.Errorf("expected chain to resolve a->b->c->real-model, got (%q, %q)", got, forward)
	}

	// Cycle: should not infinite-loop.
	s.UpsertModelAlias(&ModelAlias{Alias: "x", InternalModel: "y", ForwardingMode: "use_internal", Enabled: true})
	s.UpsertModelAlias(&ModelAlias{Alias: "y", InternalModel: "x", ForwardingMode: "use_internal", Enabled: true})
	got, _ = s.ResolveAlias("x")
	if got == "" {
		t.Error("cycle should not return empty")
	}
}

func TestResolveAliasKeepExternal(t *testing.T) {
	s := New("")
	s.UpsertModelAlias(&ModelAlias{Alias: "company-model", InternalModel: "llama-3.1-70b", ForwardingMode: "keep_external", Enabled: true})
	internal, forward := s.ResolveAlias("company-model")
	if internal != "llama-3.1-70b" {
		t.Errorf("internal=%q want llama-3.1-70b", internal)
	}
	if forward != "company-model" {
		t.Errorf("forward=%q want company-model", forward)
	}
}

func TestAPIKeyModelAllowed(t *testing.T) {
	k := &APIKey{
		AllowedModels: []string{"llama-*", "qwen-72b"},
		DeniedModels:  []string{"llama-3.1-405b"},
	}
	if !k.ModelAllowed("llama-3.1-70b") {
		t.Error("expected llama-3.1-70b to be allowed")
	}
	if !k.ModelAllowed("qwen-72b") {
		t.Error("expected qwen-72b to be allowed")
	}
	if k.ModelAllowed("llama-3.1-405b") {
		t.Error("expected llama-3.1-405b to be denied")
	}
	if k.ModelAllowed("gpt-4") {
		t.Error("expected gpt-4 to be denied (not in allow list)")
	}

	// Empty allow list = allow all (except denied)
	k2 := &APIKey{DeniedModels: []string{"forbidden-*"}}
	if !k2.ModelAllowed("any-model") {
		t.Error("empty allow list should allow any model")
	}
	if k2.ModelAllowed("forbidden-x") {
		t.Error("denied model should not be allowed")
	}
}

func TestBackendHealthTransitions(t *testing.T) {
	b := &Backend{Enabled: true, status: StatusUnknown}
	// Default success_threshold=2: a backend in unknown state must
	// observe two successive probes before being marked healthy.
	b.RecordHealthCheck(true, 5, "")
	b.RecordHealthCheck(true, 5, "")
	if b.Status() != StatusHealthy {
		t.Errorf("expected healthy after 2 successes from unknown, got %s", b.Status())
	}
	// 3 consecutive failures -> unhealthy
	b.RecordHealthCheck(false, 0, "err")
	b.RecordHealthCheck(false, 0, "err")
	b.RecordHealthCheck(false, 0, "err")
	if b.Status() != StatusUnhealthy {
		t.Errorf("expected unhealthy after 3 failures, got %s", b.Status())
	}
	// 2 successes -> healthy again
	b.RecordHealthCheck(true, 5, "")
	b.RecordHealthCheck(true, 5, "")
	if b.Status() != StatusHealthy {
		t.Errorf("expected healthy after recovery, got %s", b.Status())
	}
}

// P1-6 (review): unknown -> healthy must respect success_threshold,
// no startup-shortcut bypass.
func TestUnknownBackendRequiresSuccessThreshold(t *testing.T) {
	threshold := 3
	b := &Backend{
		Enabled:     true,
		status:      StatusUnknown,
		HealthCheck: &config.HealthCheckConfig{SuccessThreshold: threshold},
	}
	// First probe: must not flip to healthy.
	b.RecordHealthCheck(true, 5, "")
	if b.Status() != StatusUnknown {
		t.Errorf("expected unknown after 1 success with threshold=%d, got %s", threshold, b.Status())
	}
	// Second probe: still under threshold.
	b.RecordHealthCheck(true, 5, "")
	if b.Status() != StatusUnknown {
		t.Errorf("expected unknown after 2 successes with threshold=%d, got %s", threshold, b.Status())
	}
	// Third probe: hits threshold -> healthy.
	b.RecordHealthCheck(true, 5, "")
	if b.Status() != StatusHealthy {
		t.Errorf("expected healthy after 3 successes with threshold=%d, got %s", threshold, b.Status())
	}
}

func TestBackendAcquireSlot(t *testing.T) {
	b := &Backend{MaxConcurrentRequests: 2, Enabled: true}
	if !b.AcquireSlot() || !b.AcquireSlot() {
		t.Fatal("expected first two slots to succeed")
	}
	if b.AcquireSlot() {
		t.Fatal("expected third slot to be rejected")
	}
	b.ReleaseSlot(true)
	if !b.AcquireSlot() {
		t.Fatal("expected slot to become available after release")
	}
}
