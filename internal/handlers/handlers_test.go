package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/auth"
	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/handlers"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/ratelimit"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// captureBackend records every request body it receives so tests can assert
// transparent passthrough.
type captureBackend struct {
	server      *httptest.Server
	lastBody    []byte
	lastModel   string
	lastHeader  http.Header
	requestCnt  int64
	responseFn  func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newCaptureBackend() *captureBackend {
	cb := &captureBackend{}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cb.requestCnt, 1)
		body, _ := io.ReadAll(r.Body)
		cb.lastBody = body
		cb.lastHeader = r.Header.Clone()
		// Extract model from body
		var peek struct{ Model string `json:"model"` }
		_ = json.Unmarshal(body, &peek)
		cb.lastModel = peek.Model
		if cb.responseFn != nil {
			cb.responseFn(w, r, body)
			return
		}
		// Default echo response - includes extension fields to verify passthrough.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"model": "%s",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "The answer is 42.",
					"reasoning_content": "First we analyse the problem..."
				},
				"finish_reason": "stop"
			}],
			"usage": {
				"prompt_tokens": 10,
				"completion_tokens": 20,
				"total_tokens": 30,
				"reasoning_tokens": 8
			},
			"custom_vendor_field": {"foo":"bar"}
		}`, peek.Model)
	})
	cb.server = httptest.NewServer(mux)
	return cb
}

func (c *captureBackend) URL() string { return c.server.URL }
func (c *captureBackend) Close()      { c.server.Close() }

// buildGateway wires up a minimal in-process gateway suitable for tests.
func buildGateway(t *testing.T, backendURL string, modelList []string) (http.Handler, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.HashSecret = "test-secret"
	cfg.RateLimit.DefaultRequestsPerMinute = 100000
	cfg.RateLimit.DefaultConcurrentReq = 0

	s := store.New(cfg.Auth.HashSecret)
	b := &store.Backend{
		ID:                    "be",
		Name:                  "test backend",
		BaseURL:               strings.TrimRight(backendURL, "/"),
		Enabled:               true,
		Models:                modelList,
		Weight:                1,
		MaxConcurrentRequests: 100,
		TimeoutMS:             5000,
		StreamIdleTimeoutMS:   5000,
	}
	b.SetStatus(store.StatusHealthy)
	s.UpsertBackend(b)

	// Provision an API key allowed to use these models.
	s.UpsertAPIKey(&store.APIKey{
		ID:            "k1",
		Name:          "test",
		Enabled:       true,
		AllowedModels: []string{"*"},
	}, "sk-test")

	m := metrics.New()
	logger := logging.New(io.Discard, "error")
	pxy := proxy.New(logger, m)
	bal := balancer.New()
	rl := ratelimit.New()
	cc := ratelimit.NewConcurrency()
	h := handlers.New(cfg, s, pxy, bal, rl, cc, logger, m)

	authn := auth.New(s, "Authorization", "Bearer ")
	mux := http.NewServeMux()
	mux.Handle("GET /v1/models", authn.Middleware(func(r *http.Request) bool { return true })(http.HandlerFunc(h.ListModels)))
	mux.Handle("POST /v1/chat/completions", authn.Middleware(func(r *http.Request) bool { return true })(h.Forward("/chat/completions")))
	return mux, s
}

func doRequest(t *testing.T, h http.Handler, body []byte) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestRequestPassthroughKeepsUnknownFields(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	reqBody := []byte(`{
		"model":"llama-3.1-70b",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high",
		"thinking_budget":4096,
		"enable_thinking":true,
		"top_k":40,
		"min_p":0.05,
		"repetition_penalty":1.1,
		"extra_body":{"chat_template_kwargs":{"enable_thinking":true}},
		"custom_vendor_param":{"foo":"bar"}
	}`)
	resp := doRequest(t, h, reqBody)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	// Backend should have received every field exactly.
	var got map[string]any
	if err := json.Unmarshal(be.lastBody, &got); err != nil {
		t.Fatalf("backend body not valid JSON: %v", err)
	}
	required := []string{"reasoning_effort", "thinking_budget", "enable_thinking",
		"top_k", "min_p", "repetition_penalty", "extra_body", "custom_vendor_param"}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("backend did not receive field %q (gateway stripped it)", k)
		}
	}
}

func TestResponsePassthroughKeepsReasoningContent(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"deepseek-r1"})

	resp := doRequest(t, h, []byte(`{"model":"deepseek-r1","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"reasoning_content"`)) {
		t.Errorf("response missing reasoning_content: %s", body)
	}
	if !bytes.Contains(body, []byte(`"reasoning_tokens"`)) {
		t.Errorf("response missing reasoning_tokens: %s", body)
	}
	if !bytes.Contains(body, []byte(`"custom_vendor_field"`)) {
		t.Errorf("response missing custom_vendor_field: %s", body)
	}
}

func TestNoModelFallback(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	// Mark backend unhealthy by configuring it that way.
	resp := doRequest(t, h, []byte(`{"model":"qwen-72b","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"model_not_found"`)) {
		t.Errorf("expected model_not_found code: %s", body)
	}
}

func TestNoHealthyBackend(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	// Disable the backend.
	b, _ := s.Backend("be")
	b.Enabled = false
	s.UpsertBackend(b)

	resp := doRequest(t, h, []byte(`{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"no_healthy_backend"`)) {
		t.Errorf("expected no_healthy_backend code: %s", body)
	}
}

func TestModelPermissionDenied(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b", "gpt-4"})

	// Restrict the test key to llama-* only.
	k, _ := s.APIKey("k1")
	k.AllowedModels = []string{"llama-*"}

	resp := doRequest(t, h, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"model_not_allowed"`)) {
		t.Errorf("expected model_not_allowed: %s", body)
	}
}

func TestInvalidAPIKey(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"llama-3.1-70b","messages":[]}`)))
	req.Header.Set("Authorization", "Bearer wrong-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthorizationNotForwardedToBackend(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	// Set a backend-specific api key.
	b, _ := s.Backend("be")
	b.APIKey = "backend-secret"
	s.UpsertBackend(b)

	doRequest(t, h, []byte(`{"model":"llama-3.1-70b","messages":[]}`))
	upstreamAuth := be.lastHeader.Get("Authorization")
	if upstreamAuth != "Bearer backend-secret" {
		t.Errorf("expected backend api key in upstream Authorization, got %q", upstreamAuth)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()

	chunks := []string{
		`data: {"choices":[{"delta":{"reasoning_content":"first thinking..."}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"more thinking..."}}]}`,
		`data: {"choices":[{"delta":{"content":"The answer is 42."}}]}`,
		`data: {"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
		`data: [DONE]`,
	}
	be.responseFn = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = io.WriteString(w, c+"\n\n")
			flusher.Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}
	h, _ := buildGateway(t, be.URL(), []string{"deepseek-r1"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(
		`{"model":"deepseek-r1","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected SSE content type, got %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, c := range chunks {
		if !strings.Contains(body, c) {
			t.Errorf("streamed body missing chunk %q", c)
		}
	}
	// reasoning_content must survive intact.
	if !strings.Contains(body, "reasoning_content") {
		t.Errorf("streamed body should preserve reasoning_content: %s", body)
	}
}

func TestAliasForwardingUsesInternalModel(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})
	s.UpsertModelAlias(&store.ModelAlias{
		Alias:          "company-main-model",
		InternalModel:  "llama-3.1-70b",
		ForwardingMode: "use_internal",
		Enabled:        true,
	})
	resp := doRequest(t, h, []byte(`{"model":"company-main-model","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if be.lastModel != "llama-3.1-70b" {
		t.Errorf("expected internal model name on backend, got %q", be.lastModel)
	}
}

func TestAliasForwardingKeepsExternal(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})
	s.UpsertModelAlias(&store.ModelAlias{
		Alias:          "company-main-model",
		InternalModel:  "llama-3.1-70b",
		ForwardingMode: "keep_external",
		Enabled:        true,
	})
	resp := doRequest(t, h, []byte(`{"model":"company-main-model","messages":[{"role":"user","content":"hi"}]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if be.lastModel != "company-main-model" {
		t.Errorf("expected external alias to be preserved on backend, got %q", be.lastModel)
	}
}

func TestInvalidJSON(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})
	resp := doRequest(t, h, []byte(`{not json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMissingModel(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, _ := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})
	resp := doRequest(t, h, []byte(`{"messages":[]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing model, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`"missing_model"`)) {
		t.Errorf("expected missing_model code: %s", body)
	}
}

func TestListModelsFiltersByAPIKey(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b", "qwen-72b"})
	k, _ := s.APIKey("k1")
	k.AllowedModels = []string{"llama-*"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "llama-3.1-70b") {
		t.Errorf("expected llama-3.1-70b in /v1/models: %s", body)
	}
	if strings.Contains(body, "qwen-72b") {
		t.Errorf("expected qwen-72b to be filtered out: %s", body)
	}
}

func TestLoadBalancingDistributesAcrossBackends(t *testing.T) {
	be1 := newCaptureBackend()
	defer be1.Close()
	be2 := newCaptureBackend()
	defer be2.Close()

	cfg := config.Default()
	cfg.Auth.HashSecret = "test"
	cfg.RateLimit.DefaultRequestsPerMinute = 100000
	s := store.New(cfg.Auth.HashSecret)
	for i, u := range []string{be1.URL(), be2.URL()} {
		b := &store.Backend{
			ID: fmt.Sprintf("b%d", i), BaseURL: strings.TrimRight(u, "/"),
			Enabled: true, Models: []string{"shared"}, Weight: 1,
			MaxConcurrentRequests: 100, TimeoutMS: 5000,
		}
		b.SetStatus(store.StatusHealthy)
		s.UpsertBackend(b)
	}
	s.UpsertAPIKey(&store.APIKey{Enabled: true, AllowedModels: []string{"*"}}, "sk-lb")
	m := metrics.New()
	logger := logging.New(io.Discard, "error")
	pxy := proxy.New(logger, m)
	h := handlers.New(cfg, s, pxy, balancer.New(), ratelimit.New(), ratelimit.NewConcurrency(), logger, m)
	authn := auth.New(s, "Authorization", "Bearer ")
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", authn.Middleware(func(r *http.Request) bool { return true })(h.Forward("/chat/completions")))

	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"shared","messages":[]}`)))
		req.Header.Set("Authorization", "Bearer sk-lb")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d failed: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	c1 := atomic.LoadInt64(&be1.requestCnt)
	c2 := atomic.LoadInt64(&be2.requestCnt)
	if c1 == 0 || c2 == 0 {
		t.Errorf("expected both backends to receive requests: be1=%d be2=%d", c1, c2)
	}
}
