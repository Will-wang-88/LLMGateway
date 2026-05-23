package main

import (
	"testing"

	"github.com/will-wang-88/llmgateway/internal/config"
)

// P2-3 (review): --healthcheck must observe LLMGATEWAY_LISTEN so the
// probe targets the same port the gateway actually binds to. The fix
// extracted env overrides into applyEnvOverrides, called before
// runHealthCheck. We verify the env override path here.
func TestHealthCheckUsesListenEnvOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.Port = 8080
	t.Setenv("LLMGATEWAY_LISTEN", "127.0.0.1:9090")
	applyEnvOverrides(cfg)
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9090 {
		t.Errorf("env override didn't apply: host=%q port=%d", cfg.Server.Host, cfg.Server.Port)
	}
}

// P2-4 (review): SMTP password must be loadable from env so operators
// don't have to put it in the YAML.
func TestSMTPPasswordCanBeLoadedFromEnv(t *testing.T) {
	cfg := config.Default()
	t.Setenv("LLMGATEWAY_SMTP_PASSWORD", "from-env")
	applyEnvOverrides(cfg)
	if cfg.Notifications.Email.Password != "from-env" {
		t.Errorf("expected SMTP password from LLMGATEWAY_SMTP_PASSWORD, got %q",
			cfg.Notifications.Email.Password)
	}
	// And the legacy SMTP_PASSWORD env should also work.
	cfg2 := config.Default()
	t.Setenv("LLMGATEWAY_SMTP_PASSWORD", "")
	t.Setenv("SMTP_PASSWORD", "from-legacy-env")
	applyEnvOverrides(cfg2)
	if cfg2.Notifications.Email.Password != "from-legacy-env" {
		t.Errorf("expected SMTP password from SMTP_PASSWORD, got %q",
			cfg2.Notifications.Email.Password)
	}
}
