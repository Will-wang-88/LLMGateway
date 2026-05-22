package store

import "testing"

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
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.value); got != c.want {
			t.Errorf("matchPattern(%q,%q)=%v want %v", c.pattern, c.value, got, c.want)
		}
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
	// 3 successes -> healthy
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
