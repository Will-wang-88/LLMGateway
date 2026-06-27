package config

import "testing"

func TestLoadGatewayYAMLCompression(t *testing.T) {
	c, err := Load("../../config/gateway.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Global default present and disabled.
	if c.Compression.Enabled == nil || *c.Compression.Enabled {
		t.Fatalf("expected global compression disabled by default")
	}
	var found bool
	for _, m := range c.Models {
		if m.Name == "llama-3.1-70b" && m.Compression != nil {
			found = true
			if m.Compression.Enabled == nil || !*m.Compression.Enabled {
				t.Fatalf("expected llama-3.1-70b compression enabled")
			}
			if m.Compression.MinInputTokens == nil || *m.Compression.MinInputTokens != 8000 {
				t.Fatalf("expected min_input_tokens 8000")
			}
		}
	}
	if !found {
		t.Fatal("llama-3.1-70b compression block not parsed")
	}
}
