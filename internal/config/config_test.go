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
