package admin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/netutil"
	"github.com/will-wang-88/llmgateway/internal/store"
)

func newAdminMux(t *testing.T) (*http.ServeMux, *store.Store) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Admin.Enabled = true
	cfg.Admin.BindToken = "tk"
	s := store.New("hmac")
	logger := logging.New(io.Discard, "error")
	srv := NewServer(cfg, s, nil, logger)
	mux := http.NewServeMux()
	srv.Register(mux)
	return mux, s
}

func doAdmin(mux *http.ServeMux, method, path, body string) *http.Response {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer tk")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Result()
}

func TestAdminModelsGetPatchRoutes(t *testing.T) {
	mux, s := newAdminMux(t)
	s.UpsertModel(&store.Model{Name: "m1", Type: "chat", Enabled: true, CapabilityMode: "passthrough"})

	resp := doAdmin(mux, "GET", "/admin/models/m1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/models/m1 -> %d", resp.StatusCode)
	}

	resp = doAdmin(mux, "PATCH", "/admin/models/m1", `{"enabled":false,"capability_mode":"declared"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH -> %d", resp.StatusCode)
	}
	m, _ := s.Model("m1")
	if m.Enabled {
		t.Errorf("expected enabled=false")
	}
	if m.CapabilityMode != "declared" {
		t.Errorf("expected capability_mode=declared, got %q", m.CapabilityMode)
	}

	resp = doAdmin(mux, "PATCH", "/admin/models/m1", `{"capability_mode":"banana"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid capability_mode, got %d", resp.StatusCode)
	}
}

func TestAdminAliasPatchRoute(t *testing.T) {
	mux, s := newAdminMux(t)
	s.UpsertModelAlias(&store.ModelAlias{Alias: "a1", InternalModel: "m1", ForwardingMode: "use_internal", Enabled: true})

	resp := doAdmin(mux, "PATCH", "/admin/model-aliases/a1", `{"enabled":false,"internal_model":"m2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH alias -> %d", resp.StatusCode)
	}
	var alias *store.ModelAlias
	for _, a := range s.ModelAliases() {
		if a.Alias == "a1" {
			alias = a
		}
	}
	if alias == nil {
		t.Fatal("alias missing")
	}
	if alias.Enabled || alias.InternalModel != "m2" {
		t.Errorf("alias not patched: %+v", alias)
	}
}

func TestAdminStatsAndMetricsRoutes(t *testing.T) {
	mux, _ := newAdminMux(t)
	for _, path := range []string{
		"/admin/stats/models", "/admin/stats/backends", "/admin/stats/api-keys",
		"/admin/metrics", "/admin/notifications/status",
	} {
		resp := doAdmin(mux, "GET", path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s -> %d", path, resp.StatusCode)
		}
	}
}

// P2-1 (review): POST /admin/models must validate capability_mode just
// like PATCH does. Otherwise UIs could silently configure an unsupported
// mode that capability.Check() treats as passthrough.
func TestAdminModelPostRejectsInvalidCapabilityMode(t *testing.T) {
	mux, _ := newAdminMux(t)
	resp := doAdmin(mux, "POST", "/admin/models",
		`{"name":"m1","capability_mode":"definitely-not-real"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid capability_mode on POST, got %d", resp.StatusCode)
	}
}

// P2-1: POST /admin/model-aliases must validate forwarding_mode.
func TestAdminAliasPostRejectsInvalidForwardingMode(t *testing.T) {
	mux, _ := newAdminMux(t)
	resp := doAdmin(mux, "POST", "/admin/model-aliases",
		`{"alias":"a1","internal_model":"m1","forwarding_mode":"banana"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid forwarding_mode on POST, got %d", resp.StatusCode)
	}
}

func newAdminMuxWithExtractor(t *testing.T, trusted []string) (*http.ServeMux, *store.Store, *logstore.Memory) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Admin.Enabled = true
	cfg.Admin.BindToken = "tk"
	s := store.New("hmac")
	logger := logging.New(io.Discard, "error")
	ls := logstore.NewMemory(100)
	srv := NewServer(cfg, s, nil, logger).
		WithLogStore(ls).
		WithClientIPExtractor(netutil.NewExtractor(trusted))
	mux := http.NewServeMux()
	srv.Register(mux)
	return mux, s, ls
}

// P2-5 (review): admin audit IP must use the same trusted-proxy
// extractor as the data plane, not blindly trust X-Forwarded-For.
func TestAdminAuditIgnoresUntrustedXForwardedFor(t *testing.T) {
	mux, _, ls := newAdminMuxWithExtractor(t, nil) // no trusted proxies
	req := httptest.NewRequest("POST", "/admin/models", bytes.NewBufferString(
		`{"name":"m-audit","capability_mode":"passthrough"}`))
	req.Header.Set("Authorization", "Bearer tk")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.99") // attacker tries to spoof
	req.RemoteAddr = "192.0.2.5:443"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert failed: %d", rec.Code)
	}
	time.Sleep(60 * time.Millisecond)
	events, _ := ls.QueryAudit(context.Background(), logstore.AuditQuery{Limit: 10})
	if len(events) == 0 {
		t.Fatal("no audit event recorded")
	}
	if events[0].IP == "203.0.113.99" {
		t.Errorf("admin audit trusted spoofed XFF from untrusted peer: %s", events[0].IP)
	}
	if events[0].IP != "192.0.2.5" {
		t.Errorf("expected admin audit IP to be the immediate peer 192.0.2.5, got %q", events[0].IP)
	}
}

func TestAdminAuditUsesTrustedProxyClientIP(t *testing.T) {
	mux, _, ls := newAdminMuxWithExtractor(t, []string{"192.0.2.0/24"})
	req := httptest.NewRequest("POST", "/admin/models", bytes.NewBufferString(
		`{"name":"m-audit2","capability_mode":"passthrough"}`))
	req.Header.Set("Authorization", "Bearer tk")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	req.RemoteAddr = "192.0.2.5:443" // trusted proxy
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert failed: %d", rec.Code)
	}
	time.Sleep(60 * time.Millisecond)
	events, _ := ls.QueryAudit(context.Background(), logstore.AuditQuery{Limit: 10})
	if len(events) == 0 {
		t.Fatal("no audit event recorded")
	}
	if events[0].IP != "203.0.113.42" {
		t.Errorf("expected trusted XFF entry 203.0.113.42, got %q", events[0].IP)
	}
}
