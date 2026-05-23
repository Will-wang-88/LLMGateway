package handlers

import (
	"context"
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
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/queue"
	"github.com/will-wang-88/llmgateway/internal/quota"
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
	logstore    logstore.Store
	quota       *quota.Manager
	queue       *queue.Manager
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

// WithLogStore attaches a persistent log store for request/audit logs.
func (h *Handler) WithLogStore(ls logstore.Store) *Handler { h.logstore = ls; return h }

// WithQuota attaches a quota manager.
func (h *Handler) WithQuota(q *quota.Manager) *Handler { h.quota = q; return h }

// WithQueue attaches a request queue for backpressure.
func (h *Handler) WithQueue(q *queue.Manager) *Handler { h.queue = q; return h }

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

	// Build the explicit model registry first so we can honor model.enabled
	// for entries that exist there. Models advertised only by a backend
	// (without a registry entry) are included by default.
	registry := make(map[string]*store.Model)
	for _, m := range h.store.Models() {
		registry[m.Name] = m
	}
	for _, b := range h.store.Backends() {
		if !b.Enabled {
			continue
		}
		for _, mname := range b.Models {
			if r, ok := registry[mname]; ok && !r.Enabled {
				continue
			}
			add(mname)
		}
	}
	for _, m := range h.store.Models() {
		if m.Enabled {
			add(m.Name)
		}
	}
	for _, a := range h.store.ModelAliases() {
		if !a.Enabled {
			continue
		}
		if r, ok := registry[a.InternalModel]; ok && !r.Enabled {
			continue
		}
		add(a.Alias)
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

		// Apply rate-limit / quota / concurrency / queue admission.
		release, code, status := h.admit(r.Context(), apiKey, internalModel)
		if code != "" {
			h.recordLog(r.Context(), requestID, apiKey, peek.Model, internalModel, "", upstreamPath, isStream, status, code, nil, time.Since(started).Milliseconds(), 0, raw, nil)
			proxy.WriteError(w, status, proxy.RateLimit("Rejected: "+code, code))
			return
		}
		defer release()

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
		hint := balancer.Hint{APIKeyID: apiKeyID(apiKey)}
		picked := h.balancer.Choose(internalModel, policy, ready, hint)
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
			picked = h.balancer.Choose(internalModel, policy, ready, hint)
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

		var ttftMS int64
		var capturedUsage proxy.Usage
		var rawResp []byte
		captureRawResp := h.shouldLog(apiKey, "log_raw_response")
		captureStreamChunks := h.shouldLog(apiKey, "log_stream_chunks")
		var streamBuf []byte

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
				ttftMS = d.Milliseconds()
				h.metrics.TTFT.WithLabelValues(internalModel, picked.ID).Observe(d.Seconds())
			},
			OnUsage: func(u *proxy.Usage) {
				if u != nil {
					capturedUsage = *u
				}
				h.recordUsage(u, internalModel, picked.ID, apiKeyLabel, apiKey)
			},
			OnStreamUsage: func(u *proxy.Usage) {
				if u != nil {
					capturedUsage = *u
				}
				h.recordUsage(u, internalModel, picked.ID, apiKeyLabel, apiKey)
			},
		}
		if captureRawResp {
			opts.OnRawResponse = func(body []byte) {
				rawResp = append(rawResp[:0], body...)
			}
		}
		if captureStreamChunks {
			opts.OnStreamChunk = func(line []byte) {
				streamBuf = append(streamBuf, line...)
			}
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

		latencyMS := time.Since(started).Milliseconds()
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
				"latency_ms", latencyMS,
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

		// Persistent request log (if a log store is attached).
		if h.logstore != nil {
			var rawReqForLog []byte
			if h.shouldLog(apiKey, "log_raw_request") {
				rawReqForLog = forwardBody
			}
			var rawRespForLog []byte
			if isStream && captureStreamChunks {
				rawRespForLog = streamBuf
			} else if !isStream && captureRawResp {
				rawRespForLog = rawResp
			}
			h.recordLog(r.Context(), requestID, apiKey, peek.Model, internalModel, picked.ID,
				upstreamPath, isStream, statusCode, errorCodeFromForward(ferr, statusCode),
				&capturedUsage, latencyMS, ttftMS, rawReqForLog, rawRespForLog)
		}
	}
}

func errorCodeFromForward(err error, statusCode int) string {
	if err != nil && statusCode == 0 {
		return "forward_error"
	}
	if statusCode >= 400 {
		return statusCodeLabel(statusCode)
	}
	return ""
}

// recordLog writes a single request-log entry to the persistent log store.
func (h *Handler) recordLog(ctx context.Context, requestID string, k *store.APIKey,
	model, internalModel, backendID, endpoint string, stream bool, statusCode int,
	errorCode string, usage *proxy.Usage, latencyMS, ttftMS int64,
	rawReq, rawResp []byte) {
	if h.logstore == nil {
		return
	}
	rec := &logstore.RequestLog{
		ID:            uuid.New().String(),
		RequestID:     requestID,
		Model:         model,
		InternalModel: internalModel,
		BackendID:     backendID,
		Endpoint:      endpoint,
		Stream:        stream,
		StatusCode:    statusCode,
		ErrorCode:     errorCode,
		LatencyMS:     latencyMS,
		TTFTMS:        ttftMS,
		CreatedAt:     time.Now().UTC(),
	}
	if k != nil {
		rec.APIKeyID = k.ID
		rec.APIKeyName = k.Name
	}
	if usage != nil {
		rec.PromptTokens = usage.PromptTokens
		rec.CompletionTokens = usage.CompletionTokens
		rec.TotalTokens = usage.TotalTokens
		rec.ReasoningTokens = usage.ReasoningTokens
	}
	if len(rawReq) > 0 {
		rec.RawRequest = string(rawReq)
	}
	if len(rawResp) > 0 {
		rec.RawResponse = string(rawResp)
	}
	// Fire-and-forget so a slow log store can't block request completion.
	go func(r *logstore.RequestLog) {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := h.logstore.AppendRequest(c, r); err != nil {
			h.logger.Warn("logstore append failed", logging.F("error", err.Error()))
		}
	}(rec)
}

func apiKeyID(k *store.APIKey) string {
	if k == nil {
		return "anonymous"
	}
	return k.ID
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
	if u.ReasoningTokens > 0 {
		h.metrics.ReasoningTokens.WithLabelValues(model, backendID, apiKeyLabel).Add(float64(u.ReasoningTokens))
	}
	if apiKey != nil {
		apiKey.Touch(u.TotalTokens)
		if apiKey.RateLimit != nil && apiKey.RateLimit.TokensPerMinute > 0 {
			h.limiter.AddTokens("tok:"+apiKey.ID, u.TotalTokens)
		}
	}
	if h.quota != nil && u.TotalTokens > 0 {
		h.quota.AddTokens(apiKeyLabel, u.TotalTokens)
	}
}

// admit enforces all per-API-key admission policies atomically. Returns a
// release func that the caller must defer; an error code if the request is
// rejected; and the HTTP status to return.
func (h *Handler) admit(ctx context.Context, k *store.APIKey, internalModel string) (release func(), code string, status int) {
	releases := make([]func(), 0, 3)
	doRelease := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	noop := func() {}

	if k != nil {
		// 1. Combined per-minute + per-day check (single atomic call).
		rpm := h.cfg.RateLimit.DefaultRequestsPerMinute
		var tpm int64
		var dayReq, dayTok int64
		if k.RateLimit != nil && k.RateLimit.Enabled {
			if k.RateLimit.RequestsPerMinute > 0 {
				rpm = k.RateLimit.RequestsPerMinute
			}
			tpm = int64(k.RateLimit.TokensPerMinute)
			if k.RateLimit.RequestsPerDay > 0 {
				dayReq = k.RateLimit.RequestsPerDay
			}
			if k.RateLimit.TokensPerDay > 0 {
				dayTok = k.RateLimit.TokensPerDay
			}
		}
		if k.Quota != nil {
			// Quota fields take precedence if both are set (quota is the
			// "hard cap"; rate_limit.requests_per_day is the "soft rate").
			if k.Quota.DailyRequests > 0 {
				dayReq = k.Quota.DailyRequests
			}
			if k.Quota.DailyTokens > 0 {
				dayTok = k.Quota.DailyTokens
			}
		}
		if reject := h.limiter.CheckAndReserve("key:"+k.ID, rpm, tpm, dayReq, dayTok); reject != "" {
			h.metrics.RateLimitHits.WithLabelValues(k.ID, reject).Inc()
			doRelease()
			return noop, reject, http.StatusTooManyRequests
		}

		// 2. Monthly quota (separate manager since rolling daily counters
		// can't be reused for monthly accumulation).
		if h.quota != nil && k.Quota != nil {
			if reject := h.quota.Check(k.ID, 0, 0, k.Quota.MonthlyRequests, k.Quota.MonthlyTokens); reject != "" {
				h.metrics.RateLimitHits.WithLabelValues(k.ID, reject).Inc()
				doRelease()
				return noop, reject, http.StatusTooManyRequests
			}
		}

		// 3. Concurrency.
		concLimit := 0
		if k.RateLimit != nil {
			concLimit = k.RateLimit.ConcurrentRequests
		}
		if !h.concurrency.Acquire("key:"+k.ID, concLimit) {
			h.metrics.RateLimitHits.WithLabelValues(k.ID, "concurrent_limit").Inc()
			doRelease()
			return noop, "concurrent_limit", http.StatusTooManyRequests
		}
		releases = append(releases, func() { h.concurrency.Release("key:"+k.ID, concLimit) })

		// 4. Optional delay (QoS).
		if k.DelayMS > 0 {
			select {
			case <-time.After(time.Duration(k.DelayMS) * time.Millisecond):
			case <-ctx.Done():
				doRelease()
				return noop, "client_canceled", 499
			}
		}
	}

	// 5. Per-model request queue (backpressure).
	if h.queue != nil && h.cfg.Queue.Enabled {
		rel, err := h.queue.Acquire(ctx, "model:"+internalModel)
		if err != nil {
			c := "queue_timeout"
			if errors.Is(err, queue.ErrFull) {
				c = "queue_full"
			}
			h.metrics.RateLimitHits.WithLabelValues(apiKeyID(k), c).Inc()
			doRelease()
			return noop, c, http.StatusTooManyRequests
		}
		releases = append(releases, rel)
		h.metrics.QueueDepth.WithLabelValues("model:" + internalModel).Set(float64(h.queue.Depth("model:" + internalModel)))
	}

	// 6. Account the request for monthly quota now that admission succeeded.
	if h.quota != nil && k != nil {
		h.quota.AddRequest(k.ID)
	}
	return doRelease, "", 0
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

// filterRoutable returns backends that are admissible for routing.
//
// Routable states: healthy, unknown (just-started), degraded (responsive but
// reporting some failure - still worth trying, with a warning logged).
// Excluded states: disabled, unhealthy, maintenance.
// Excluded: backends at max concurrency.
// Excluded: backends with weight 0 (drain mode).
func filterRoutable(in []*store.Backend) []*store.Backend {
	out := make([]*store.Backend, 0, len(in))
	for _, b := range in {
		if !b.Enabled {
			continue
		}
		switch b.Status() {
		case store.StatusHealthy, store.StatusUnknown, store.StatusDegraded:
			// admissible
		default:
			continue
		}
		if b.Weight == 0 {
			// 0 weight is interpreted as "drain": registered but no new traffic.
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

