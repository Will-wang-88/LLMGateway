package config

import "testing"

// Ensures the shipped sample config validates under all cross-checks
// (orchestration collision + worker reachability included).
func TestSampleGatewayConfigLoads(t *testing.T) {
	c, err := Load("../../config/gateway.yaml")
	if err != nil {
		t.Fatalf("sample config failed to load/validate: %v", err)
	}
	if len(c.Backends) == 0 {
		t.Fatal("expected backends in sample config")
	}
}
