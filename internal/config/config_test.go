package config

import (
	"strings"
	"testing"
)

// P2-1 (review): config-load must reject unknown capability_mode so an
// invalid YAML doesn't start the gateway with permission rules that
// silently degrade to passthrough.
func TestConfigRejectsInvalidCapabilityMode(t *testing.T) {
	c := Default()
	c.Backends = []BackendConfig{{
		ID: "b1", BaseURL: "http://b", Models: []string{"m1"},
	}}
	c.Models = []ModelConfig{{Name: "m1", CapabilityMode: "definitely-bogus"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject invalid capability_mode")
	}
	if !strings.Contains(err.Error(), "capability_mode") {
		t.Errorf("expected capability_mode error, got %v", err)
	}
}

func TestConfigAcceptsKnownCapabilityModes(t *testing.T) {
	for _, mode := range []string{"", "passthrough", "declared", "strict"} {
		c := Default()
		c.Backends = []BackendConfig{{
			ID: "b1", BaseURL: "http://b", Models: []string{"m1"},
		}}
		c.Models = []ModelConfig{{Name: "m1", CapabilityMode: mode}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected legitimate mode %q: %v", mode, err)
		}
	}
}

func TestConfigRejectsInvalidForwardingMode(t *testing.T) {
	c := Default()
	c.Backends = []BackendConfig{{
		ID: "b1", BaseURL: "http://b", Models: []string{"m1"},
	}}
	c.ModelAliases = []ModelAliasConfig{{Alias: "a", InternalModel: "m1", ForwardingMode: "garbage"}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject invalid forwarding_mode")
	}
}

func baseOrchestrationConfig() *Config {
	c := Default()
	c.Backends = []BackendConfig{{ID: "b1", BaseURL: "http://b", Models: []string{"qwen3.6-27b"}}}
	c.Orchestration = OrchestrationConfig{
		Enabled:             true,
		RouterModel:         "fugu-auto",
		ConductorModel:      "fugu-ultra",
		ConfidenceThreshold: 0.55,
		MaxSteps:            5,
		Workers:             []OrchestrationWorker{{ID: "qwen", Model: "qwen3.6-27b"}},
	}
	return c
}

func TestOrchestrationValidConfig(t *testing.T) {
	if err := baseOrchestrationConfig().Validate(); err != nil {
		t.Fatalf("expected valid orchestration config, got %v", err)
	}
}

func TestOrchestrationRejectsNoWorkers(t *testing.T) {
	c := baseOrchestrationConfig()
	c.Orchestration.Workers = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "no workers") {
		t.Fatalf("expected no-workers error, got %v", err)
	}
}

func TestOrchestrationRejectsTooManySteps(t *testing.T) {
	c := baseOrchestrationConfig()
	c.Orchestration.MaxSteps = 6
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("expected max_steps error, got %v", err)
	}
}

func TestOrchestrationRejectsSameVirtualModel(t *testing.T) {
	c := baseOrchestrationConfig()
	c.Orchestration.ConductorModel = c.Orchestration.RouterModel
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected differ error, got %v", err)
	}
}

func TestOrchestrationDisabledSkipsValidation(t *testing.T) {
	c := Default() // Orchestration.Enabled defaults false
	c.Orchestration.MaxSteps = 99
	c.Orchestration.Workers = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled orchestration should not validate, got %v", err)
	}
}
