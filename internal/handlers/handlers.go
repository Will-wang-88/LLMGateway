package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/will-wang-88/llmgateway/internal/auth"
	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/ratelimit"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type Handler struct {
	cfg         *config.Config
	store       *store.Store
	proxy       *proxy.Proxy
	balancer    *balancer.Balancer
	limiter     *ratelimit.Limiter
	concurrency *ratelimit.Concurrency
	logger      *logging.Logger
	metrics     *metrics.Metrics
}

func New(
	cfg *config.Config,
	s *store.Store,
	p *proxy.Proxy,
	b *balancer.Balancer,
	l *ratelimit.Limiter,
	c *ratelimit.Concurrency,
	log *logging.Logger,
	m *metrics.Metrics,
) *Handler {
	return &Handler{
		cfg:         cfg,
		store:       s,
		proxy:       p,
		balancer:    b,
		limiter:     l,
		concurrency: c,
		logger:      log,
		metrics:     m,
	}
}

// ListModels implements GET /v1/models.
// It returns the union of every model declared by an enabled backend, plus any
// aliases. We do not filter by health here so clients can still see configured
// catalog entries; routing-time selection enforces health.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	apiKey, _ := auth.FromContext(r.Context())

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	seen := make(map[string]struct{})
	out := make([]modelEntry, 0)
	now := time.Now().Unix()

	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		if apiKey != nil && !apiKey.ModelAllowed(name) {
			return
		}
		seen[name] = struct{}{}
		out = append(out, modelEntry{
			ID:      name,
			Object:  "model",
			Created: now,
			OwnedBy: "llmgateway",
		})
	}

	for _, b := range h.store.Backends() {
		if !b.Enabled {
			continue
		}
		for _, m := range b.Models {
			add(m)
		}
	}
	for _, m := range h.store.Models() {
		if m.Enabled {
			add(m.Name)
		}
	}
	for _, a := range h.store.ModelAliases() {
		if a.Enabled {
			add(a.Alias)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	resp := map[string]any{
		"object": "list",
		"data":   out,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Forward handles /v1/chat/completions, /v1/completions, /v1/embeddings,
// and any other OpenAI-compatible JSON endpoint that takes a "model" field.
func (h *Handler) Forward(upstreamPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.New().String()
		w.Header().Set("X-Request-ID", requestID)
		started := time.Now()

		apiKey, _ := auth.FromContext(r.Context())
		bodyLimit := int64(h.cfg.Server.RequestBodyLimitMB) * 1024 * 1024
		if bodyLimit <= 0 {
			bodyLimit = 50 * 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				proxy.WriteError(w, http.StatusRequestEntityTooLarge, proxy.APIError{
					Message: "Request body too large",
					Type:    "invalid_request_error",
					Code:    "payload_too_large",
				})
				return
			}
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Failed to read request body: "+err.Error(),
				"invalid_body",
			))
			return
		}
		if len(raw) == 0 {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Empty request body", "invalid_body"))
			return
		}

		// Decode just enough to find "model" and "stream"; preserve every other field.
		var peek struct {
			Model  string `json:"model"`
			Stream *bool  `json:"stream"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Invalid JSON: "+err.Error(),
				"invalid_json",
			))
			return
		}
		if peek.Model == "" {
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Missing required field: model",
				"missing_model",
			))
			return
		}
		isStream := peek.Stream != nil && *peek.Stream

		// Resolve alias -> internal model. Both names are recorded for metrics labels.
		internalModel, forwardName := h.store.ResolveAlias(peek.Model)

		// API key model permission.
		if apiKey != nil && !apiKey.ModelAllowed(peek.Model) {
			proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError(
				fmt.Sprintf("The API key is not allowed to use model: %s", peek.Model),
				"model_not_allowed",
			))
			return
		}

		// Look up backends supporting this model.
		candidates := h.store.BackendsForModel(internalModel)
		if len(candidates) == 0 {
			proxy.WriteError(w, http.StatusNotFound, proxy.NotFound(
				fmt.Sprintf("Unknown model: %s", peek.Model),
				"model_not_found",
			))
			return
		}

		// Rate limit + concurrency for the API key.
		if apiKey != nil {
			rlCode := h.applyAPIKeyLimits(apiKey)
			if rlCode != "" {
				h.metrics.RateLimitHits.WithLabelValues(apiKey.ID, rlCode).Inc()
				proxy.WriteError(w, http.StatusTooManyRequests, proxy.RateLimit(
					"Rate limit exceeded: "+rlCode,
					rlCode,
				))
				return
			}
			concurrencyLimit := 0
			if apiKey.RateLimit != nil {
				concurrencyLimit = apiKey.RateLimit.ConcurrentRequests
			}
			if !h.concurrency.Acquire("key:"+apiKey.ID, concurrencyLimit) {
				h.metrics.RateLimitHits.WithLabelValues(apiKey.ID, "concurrent_limit").Inc()
				proxy.WriteError(w, http.StatusTooManyRequests, proxy.RateLimit(
					"Concurrent request limit exceeded",
					"concurrent_limit",
				))
				return
			}
			defer h.concurrency.Release("key:" + apiKey.ID)
		}

		// API-key delay (QoS).
		if apiKey != nil && apiKey.DelayMS > 0 {
			select {
			case <-time.After(time.Duration(apiKey.DelayMS) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}

		// Filter for healthy + within-capacity, then pick one.
		ready := filterRoutable(candidates)
		if len(ready) == 0 {
			proxy.WriteError(w, http.StatusServiceUnavailable, proxy.BackendUnavailable(
				fmt.Sprintf("No healthy backend available for model: %s", peek.Model),
				"no_healthy_backend",
			))
			return
		}
		policy := balancer.Policy(h.cfg.Routing.DefaultPolicy)
		if m, ok := h.store.Model(internalModel); ok && m.RoutingPolicy != "" {
			policy = balancer.Policy(m.RoutingPolicy)
		}
		picked := h.balancer.Choose(internalModel, policy, ready)
		if picked == nil {
			proxy.WriteError(w, http.StatusServiceUnavailable, proxy.BackendUnavailable(
				fmt.Sprintf("No healthy backend available for model: %s", peek.Model),
				"no_healthy_backend",
			))
			return
		}
		if !picked.AcquireSlot() {
			// Lost the race to another request; pick again from remaining.
			ready = filterRoutable(candidates)
			picked = h.balancer.Choose(internalModel, policy, ready)
			if picked == nil || !picked.AcquireSlot() {
				proxy.WriteError(w, http.StatusServiceUnavailable, proxy.BackendUnavailable(
					"All matching backends at capacity",
					"backend_at_capacity",
				))
				return
			}
		}
		// We will release the slot regardless of success/failure.

		// Rewrite the model field if forwardName differs (alias use_internal).
		forwardBody := raw
		if forwardName != peek.Model {
			forwardBody = rewriteModelInBody(raw, forwardName)
		}

		apiKeyLabel := "anonymous"
		if apiKey != nil {
			apiKeyLabel = apiKey.ID
		}
		h.metrics.ActiveRequests.WithLabelValues(internalModel, picked.ID).Inc()
		defer h.metrics.ActiveRequests.WithLabelValues(internalModel, picked.ID).Dec()

		opts := proxy.ForwardOptions{
			Method:              http.MethodPost,
			Backend:             picked,
			UpstreamPath:        upstreamPath,
			Body:                forwardBody,
			IsStream:            isStream,
			TimeoutMS:           picked.TimeoutMS,
			StreamIdleTimeoutMS: picked.StreamIdleTimeoutMS,
			Model:               internalModel,
			ForwardModel:        forwardName,
			APIKeyLabel:         apiKeyLabel,
			Endpoint:            upstreamPath,
			OnTTFT: func(d time.Duration) {
				h.metrics.TTFT.WithLabelValues(internalModel, picked.ID).Observe(d.Seconds())
			},
			OnUsage: func(u *proxy.Usage) {
				h.recordUsage(u, internalModel, picked.ID, apiKeyLabel, apiKey)
			},
			OnStreamUsage: func(u *proxy.Usage) {
				h.recordUsage(u, internalModel, picked.ID, apiKeyLabel, apiKey)
			},
		}

		statusCode, ferr := h.proxy.Forward(r.Context(), w, r, opts)
		success := ferr == nil && statusCode >= 200 && statusCode < 400
		picked.ReleaseSlot(success)
		if !success {
			h.metrics.BackendErrors.WithLabelValues(picked.ID, statusCodeLabel(statusCode)).Inc()
		}
		h.metrics.Requests.WithLabelValues(
			upstreamPath, internalModel, picked.ID, apiKeyLabel, statusCodeLabel(statusCode), boolLabel(isStream),
		).Inc()
		h.metrics.RequestLatency.WithLabelValues(
			upstreamPath, internalModel, picked.ID, boolLabel(isStream),
		).Observe(time.Since(started).Seconds())

		if h.shouldLog(apiKey, "log_metadata") || ferr != nil {
			fields := logging.F(
				"request_id", requestID,
				"endpoint", upstreamPath,
				"model", peek.Model,
				"internal_model", internalModel,
				"forward_model", forwardName,
				"backend_id", picked.ID,
				"status_code", statusCode,
				"stream", isStream,
				"latency_ms", time.Since(started).Milliseconds(),
				"api_key_id", apiKeyLabel,
			)
			if h.shouldLog(apiKey, "log_raw_request") {
				fields["raw_request"] = json.RawMessage(forwardBody)
			}
			if ferr != nil {
				fields["error"] = ferr.Error()
				h.logger.Warn("request completed with error", fields)
			} else {
				h.logger.Info("request completed", fields)
			}
		}
	}
}

func (h *Handler) recordUsage(u *proxy.Usage, model, backendID, apiKeyLabel string, apiKey *store.APIKey) {
	if u == nil {
		return
	}
	if u.PromptTokens > 0 {
		h.metrics.PromptTokens.WithLabelValues(model, backendID, apiKeyLabel).Add(float64(u.PromptTokens))
	}
	if u.CompletionTokens > 0 {
		h.metrics.CompletionTokens.WithLabelValues(model, backendID, apiKeyLabel).Add(float64(u.CompletionTokens))
	}
	if u.TotalTokens > 0 {
		h.metrics.TotalTokens.WithLabelValues(model, backendID, apiKeyLabel).Add(float64(u.TotalTokens))
	}
	if apiKey != nil {
		apiKey.Touch(u.TotalTokens)
		if apiKey.RateLimit != nil && apiKey.RateLimit.TokensPerMinute > 0 {
			h.limiter.AddTokens("tok:"+apiKey.ID, u.TotalTokens)
		}
	}
}

// applyAPIKeyLimits enforces request-per-minute / token-per-minute limits.
// Returns empty string if allowed, otherwise the rate-limit code.
func (h *Handler) applyAPIKeyLimits(k *store.APIKey) string {
	limit := h.cfg.RateLimit.DefaultRequestsPerMinute
	tokenLimit := 0
	if k.RateLimit != nil && k.RateLimit.Enabled {
		if k.RateLimit.RequestsPerMinute > 0 {
			limit = k.RateLimit.RequestsPerMinute
		}
		tokenLimit = k.RateLimit.TokensPerMinute
	}
	if !h.limiter.AllowRequest("req:"+k.ID, limit) {
		return "rate_limit_exceeded"
	}
	if tokenLimit > 0 && !h.limiter.AllowTokens("tok:"+k.ID, int64(tokenLimit)) {
		return "token_rate_limit_exceeded"
	}
	return ""
}

func (h *Handler) shouldLog(k *store.APIKey, field string) bool {
	var def bool
	switch field {
	case "log_metadata":
		def = h.cfg.Logging.DefaultLogMetadata
	case "log_input":
		def = h.cfg.Logging.DefaultLogInput
	case "log_output":
		def = h.cfg.Logging.DefaultLogOutput
	case "log_raw_request":
		def = h.cfg.Logging.DefaultLogRawRequest
	case "log_raw_response":
		def = h.cfg.Logging.DefaultLogRawResponse
	case "log_stream_chunks":
		def = h.cfg.Logging.DefaultLogStreamChunks
	}
	if k == nil || k.Logging == nil {
		return def
	}
	switch field {
	case "log_metadata":
		return k.Logging.LogMetadata
	case "log_input":
		return k.Logging.LogInput
	case "log_output":
		return k.Logging.LogOutput
	case "log_raw_request":
		return k.Logging.LogRawRequest
	case "log_raw_response":
		return k.Logging.LogRawResponse
	case "log_stream_chunks":
		return k.Logging.LogStreamChunks
	}
	return def
}

func filterRoutable(in []*store.Backend) []*store.Backend {
	out := make([]*store.Backend, 0, len(in))
	for _, b := range in {
		if !b.Enabled {
			continue
		}
		status := b.Status()
		if status != store.StatusHealthy && status != store.StatusUnknown {
			continue
		}
		if b.MaxConcurrentRequests > 0 && b.ActiveRequests() >= int64(b.MaxConcurrentRequests) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// rewriteModelInBody replaces the top-level "model" field with the new name,
// preserving every other field exactly. Used for alias forwarding only.
func rewriteModelInBody(raw []byte, newModel string) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		return raw
	}
	enc, err := json.Marshal(newModel)
	if err != nil {
		return raw
	}
	generic["model"] = enc
	out, err := json.Marshal(generic)
	if err != nil {
		return raw
	}
	return out
}

func statusCodeLabel(code int) string {
	if code <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", code)
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

