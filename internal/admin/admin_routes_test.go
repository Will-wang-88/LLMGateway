package admin

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
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
