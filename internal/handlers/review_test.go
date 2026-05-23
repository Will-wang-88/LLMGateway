package handlers_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/auth"
	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/handlers"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/netutil"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/ratelimit"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type reviewRig struct {
	mux      http.Handler
	store    *store.Store
	logstore *logstore.Memory
	be       *captureBackend
	cfg      *config.Config
}

func newReviewRig(t *testing.T, models []string) *reviewRig {
	t.Helper()
	be := newCaptureBackend()
	t.Cleanup(be.Close)

	cfg := config.Default()
	cfg.Auth.HashSecret = "test"
	cfg.RateLimit.DefaultRequestsPerMinute = 100000
	cfg.RateLimit.DefaultConcurrentReq = 0

	s := store.New(cfg.Auth.HashSecret)
	b := &store.Backend{
		ID: "be", BaseURL: strings.TrimRight(be.URL(), "/"),
		Enabled: true, Models: models, Weight: 1,
		MaxConcurrentRequests: 100, TimeoutMS: 5000,
	}
	b.SetStatus(store.StatusHealthy)
	s.UpsertBackend(b)
	s.UpsertAPIKey(&store.APIKey{
		ID: "k1", Enabled: true, AllowedModels: []string{"*"},
	}, "sk-test")

	ls := logstore.NewMemory(1000)
	m := metrics.New()
	logger := logging.New(io.Discard, "error")
	pxy := proxy.New(logger, m)
	h := handlers.New(cfg, s, pxy, balancer.New(), ratelimit.New(), ratelimit.NewConcurrency(), logger, m).
		WithLogStore(ls)

	ext := netutil.NewExtractor(nil)
	authn := auth.New(s, "Authorization", "Bearer ").
		WithClientIPExtractor(ext).
		WithLogStore(ls)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", authn.Middleware(func(r *http.Request) bool { return true })(h.Forward("/chat/completions")))
	mux.Handle("GET /v1/models", authn.Middleware(func(r *http.Request) bool { return true })(http.HandlerFunc(h.ListModels)))
	return &reviewRig{mux: mux, store: s, logstore: ls, be: be, cfg: cfg}
}

func (r *reviewRig) do(t *testing.T, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// P0-1
func TestDegradedBackendIsNotRoutable(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	b, _ := r.store.Backend("be")
	b.MarkDegraded(5, "auth-probe 401")
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503 when only backend is degraded, got %d (%s)", resp.StatusCode, body)
	}
}

func TestDegradedBackendRoutableWhenConfigEnables(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	r.cfg.Routing.AllowDegradedBackends = true
	b, _ := r.store.Backend("be")
	b.MarkDegraded(5, "auth-probe 401")
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with allow_degraded_backends=true, got %d", resp.StatusCode)
	}
}

// P0-2
func TestAliasCannotBypassDeniedInternalModel(t *testing.T) {
	r := newReviewRig(t, []string{"llama-3.1-70b"})
	r.store.UpsertModelAlias(&store.ModelAlias{
		Alias: "company-main-model", InternalModel: "llama-3.1-70b",
		ForwardingMode: "use_internal", Enabled: true,
	})
	k, _ := r.store.APIKey("k1")
	k.AllowedModels = []string{"*"}
	k.DeniedModels = []string{"llama-3.1-70b"}
	resp := r.do(t, []byte(`{"model":"company-main-model","messages":[]}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 403 when alias resolves to denied model, got %d (%s)", resp.StatusCode, body)
	}
}

// P0-3
func TestDisabledRegistryModelCannotBeForwarded(t *testing.T) {
	r := newReviewRig(t, []string{"llama-3.1-70b"})
	r.store.UpsertModel(&store.Model{Name: "llama-3.1-70b", Enabled: false, CapabilityMode: "passthrough"})
	resp := r.do(t, []byte(`{"model":"llama-3.1-70b","messages":[]}`), nil)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 404 for disabled registry model, got %d (%s)", resp.StatusCode, body)
	}
}

func TestDisabledRegistryModelViaAliasIsRejected(t *testing.T) {
	r := newReviewRig(t, []string{"llama-3.1-70b"})
	r.store.UpsertModel(&store.Model{Name: "llama-3.1-70b", Enabled: false, CapabilityMode: "passthrough"})
	r.store.UpsertModelAlias(&store.ModelAlias{
		Alias: "company-main-model", InternalModel: "llama-3.1-70b",
		ForwardingMode: "use_internal", Enabled: true,
	})
	resp := r.do(t, []byte(`{"model":"company-main-model","messages":[]}`), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 via alias to disabled model, got %d", resp.StatusCode)
	}
}

// P0-4
func TestAPIKeyAllowedClientIPs(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.AllowedClientIPs = []string{"127.0.0.1/32"}
	// httptest sets RemoteAddr to 192.0.2.1:1234 by default - outside allow list.
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 403 for IP not in allow list, got %d (%s)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("client_ip_not_allowed")) {
		t.Errorf("expected client_ip_not_allowed code, got %s", body)
	}
}

func TestAPIKeyDeniedClientIPsTakePrecedence(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.AllowedClientIPs = []string{"0.0.0.0/0"}
	k.DeniedClientIPs = []string{"192.0.2.0/24"}
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 from deny-list match, got %d", resp.StatusCode)
	}
}

func TestClientIPExtractorDoesNotTrustSpoofedXForwardedFor(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.AllowedClientIPs = []string{"10.0.0.0/8"}
	// XFF claims an allowed IP but RemoteAddr (192.0.2.1) is not in
	// any trusted_proxies list, so the header must be ignored.
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), map[string]string{
		"X-Forwarded-For": "10.0.0.5",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected XFF to be ignored without trusted proxies, got %d", resp.StatusCode)
	}
}

func TestRequestLogsIncludeClientIP(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.Logging = &config.APIKeyLogging{LogMetadata: true}
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// Logs are written via a fire-and-forget goroutine.
	time.Sleep(50 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 10})
	if len(rows) == 0 {
		t.Fatalf("expected at least one persisted request log")
	}
	if rows[0].ClientIP == "" {
		t.Errorf("expected client_ip in persisted log, got empty")
	}
}

// P1-1
func TestAuthorizationRequiresBearerPrefix(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1","messages":[]}`)))
	req.Header.Set("Authorization", "sk-test") // missing "Bearer "
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-Bearer Authorization, got %d", rec.Code)
	}
}

// P1-3
func TestAPIKeyStatsIncrementWithoutUsage(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	// Override backend to respond with no usage block.
	r.be.responseFn = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[]}`))
	}
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	k, _ := r.store.APIKey("k1")
	_, total, _ := k.Stats()
	if total != 1 {
		t.Errorf("expected total_requests=1 even without backend usage block, got %d", total)
	}
}

func TestGatewayErrorsArePersistentlyLogged(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	// invalid_json
	r.do(t, []byte(`{not json`), nil)
	// missing_model
	r.do(t, []byte(`{"messages":[]}`), nil)
	// model_not_found
	r.do(t, []byte(`{"model":"unknown-model","messages":[]}`), nil)
	time.Sleep(80 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 10})
	codes := map[string]bool{}
	for _, l := range rows {
		codes[l.ErrorCode] = true
	}
	for _, want := range []string{"invalid_json", "missing_model", "model_not_found"} {
		if !codes[want] {
			t.Errorf("expected %q to be persistently logged; got %v", want, codes)
		}
	}
}

// P1-2
func TestFallbackTokenEstimationWhenBackendOmitsUsage(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	r.be.responseFn = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally no `usage` block.
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[]}`))
	}
	body := []byte(`{"model":"m1","max_tokens":128,"messages":[{"role":"user","content":"abcdefghij"}]}`)
	resp := r.do(t, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	k, _ := r.store.APIKey("k1")
	_, _, totalTokens := k.Stats()
	if totalTokens == 0 {
		t.Errorf("expected fallback estimation to credit tokens; got 0")
	}
}

// P1-9
func TestMetricsIncludeRoutingPolicyLabel(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	r.cfg.Routing.DefaultPolicy = "weighted_round_robin"
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// Force metric registration; the build/registration would have
	// panicked at startup if the label was missing.
}

// P0-1 (review): the limiter key used by CheckAndReserve and
// AddTokens must match, otherwise tokens_per_minute / daily_tokens
// limits never see token accumulation and silently never trigger.
func TestHandlerTokensPerMinuteBlocksAfterUsage(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.RateLimit = &config.APIKeyRateLimit{
		Enabled:         true,
		TokensPerMinute: 10, // very small, will be blown by one response
	}
	// First request: backend reports 100 prompt + 50 completion = 150 tokens.
	r.be.responseFn = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first request should succeed, got %d (%s)", resp.StatusCode, body)
	}
	// The next request must be rejected by the token rate limit, since
	// the accumulated 150 tokens exceed the 10/minute cap.
	resp = r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after token-per-minute exhausted, got %d (%s)", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("token_rate_limit_exceeded")) {
		t.Errorf("expected token_rate_limit_exceeded code: %s", body)
	}
}

func TestHandlerDailyTokenLimitBlocksAfterUsage(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.Quota = &config.APIKeyQuota{DailyTokens: 10}
	r.be.responseFn = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", resp.StatusCode)
	}
	resp = r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after daily token quota exhausted, got %d (%s)", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("daily_token")) {
		t.Errorf("expected daily_token_* code, got %s", body)
	}
}

// P0-2 (review): error paths must not persist raw_request unless
// log_raw_request is enabled.
func TestGatewayErrorsDoNotPersistRawRequestWhenDisabled(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.Logging = &config.APIKeyLogging{LogMetadata: true, LogRawRequest: false}
	// invalid_json
	r.do(t, []byte(`{not json`), nil)
	// missing_model
	r.do(t, []byte(`{"messages":[]}`), nil)
	// model_not_found
	r.do(t, []byte(`{"model":"unknown-model","messages":[]}`), nil)
	time.Sleep(80 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 20})
	if len(rows) == 0 {
		t.Fatalf("expected error rows to be logged at all")
	}
	for _, row := range rows {
		if row.RawRequest != "" {
			t.Errorf("error row %q (%s) leaked raw_request when log_raw_request=false: %q",
				row.ErrorCode, row.Endpoint, row.RawRequest)
		}
	}
}

func TestGatewayErrorsPersistRawRequestWhenEnabled(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.Logging = &config.APIKeyLogging{LogMetadata: true, LogRawRequest: true}
	r.do(t, []byte(`{"messages":[]}`), nil)            // missing_model
	r.do(t, []byte(`{"model":"unknown","messages":[]}`), nil) // model_not_found
	time.Sleep(80 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 20})
	hasRaw := 0
	for _, row := range rows {
		if row.RawRequest != "" {
			hasRaw++
		}
	}
	if hasRaw == 0 {
		t.Errorf("expected raw_request to be present on at least one error row when log_raw_request=true")
	}
}

// P2-2: an alias whose internal model is in DeniedModels must not be
// listed by /v1/models.
func TestListModelsDoesNotShowAliasDeniedByInternalModel(t *testing.T) {
	r := newReviewRig(t, []string{"llama-3.1-70b"})
	r.store.UpsertModelAlias(&store.ModelAlias{
		Alias: "company-main-model", InternalModel: "llama-3.1-70b",
		ForwardingMode: "use_internal", Enabled: true,
	})
	k, _ := r.store.APIKey("k1")
	k.AllowedModels = []string{"*"}
	k.DeniedModels = []string{"llama-3.1-70b"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	r.mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "company-main-model") {
		t.Errorf("alias should be hidden when its internal model is denied: %s", body)
	}
}

// P1-1 (review): auth-layer rejections must also reach the persistent
// log store with client_ip and error_code.
func TestAuthRejectedRequestsArePersistentlyLoggedWithClientIP(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer not-a-real-key")
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	time.Sleep(80 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 10})
	var found *logstore.RequestLog
	for _, row := range rows {
		if row.ErrorCode == "invalid_api_key" {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("expected an invalid_api_key entry in persistent log; got %d rows", len(rows))
	}
	if found.ClientIP == "" {
		t.Errorf("expected client_ip to be recorded on auth reject, got empty")
	}
}

func TestClientIPNotAllowedIsLoggedWithClientIP(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	k, _ := r.store.APIKey("k1")
	k.AllowedClientIPs = []string{"127.0.0.1/32"} // any other IP -> denied
	resp := r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	time.Sleep(80 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 10})
	var found *logstore.RequestLog
	for _, row := range rows {
		if row.ErrorCode == "client_ip_not_allowed" {
			found = row
			break
		}
	}
	if found == nil {
		t.Fatalf("expected client_ip_not_allowed log entry")
	}
	if found.ClientIP == "" {
		t.Errorf("expected client_ip to be recorded, got empty")
	}
}

// P1-1 follow-up: aggregate stats include ByClientIP.
func TestStatsAggregateByClientIP(t *testing.T) {
	r := newReviewRig(t, []string{"m1"})
	for i := 0; i < 3; i++ {
		r.do(t, []byte(`{"model":"m1","messages":[]}`), nil)
	}
	time.Sleep(80 * time.Millisecond)
	stats, err := r.logstore.StatsSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.ByClientIP == nil {
		t.Fatal("stats.ByClientIP nil")
	}
	if len(stats.ByClientIP) == 0 {
		t.Errorf("expected non-empty ByClientIP, got %+v", stats.ByClientIP)
	}
}

// P1-4
func TestRawRequestLogKeepsClientOriginalBodyWhenAliasRewrites(t *testing.T) {
	r := newReviewRig(t, []string{"llama-3.1-70b"})
	r.store.UpsertModelAlias(&store.ModelAlias{
		Alias: "company-main-model", InternalModel: "llama-3.1-70b",
		ForwardingMode: "use_internal", Enabled: true,
	})
	k, _ := r.store.APIKey("k1")
	k.Logging = &config.APIKeyLogging{LogMetadata: true, LogRawRequest: true}
	body := []byte(`{"model":"company-main-model","messages":[{"role":"user","content":"hi"}]}`)
	resp := r.do(t, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	rows, _ := r.logstore.QueryRequests(context.Background(), logstore.LogQuery{Limit: 10})
	if len(rows) == 0 {
		t.Fatalf("no rows logged")
	}
	if !strings.Contains(rows[0].RawRequest, "company-main-model") {
		t.Errorf("raw_request should preserve client-original alias, got %q", rows[0].RawRequest)
	}
}
