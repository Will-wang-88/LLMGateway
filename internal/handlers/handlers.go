package handlers

import (
	"bytes"
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
	"github.com/will-wang-88/llmgateway/internal/capability"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/logstore"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/orchestrator"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/queue"
	"github.com/will-wang-88/llmgateway/internal/quota"
	"github.com/will-wang-88/llmgateway/internal/ratelimit"
	"github.com/will-wang-88/llmgateway/internal/store"
	"github.com/will-wang-88/llmgateway/internal/tracing"
)

type Handler struct {
	cfg          *config.Config
	store        *store.Store
	proxy        *proxy.Proxy
	balancer     *balancer.Balancer
	limiter      ratelimit.Backend
	concurrency  *ratelimit.Concurrency
	logger       *logging.Logger
	metrics      *metrics.Metrics
	logstore     logstore.Store
	quota        *quota.Manager
	queue        *queue.Manager
	tracer       *tracing.Tracer
	orchestrator *orchestrator.Orchestrator
}

func New(
	cfg *config.Config,
	s *store.Store,
	p *proxy.Proxy,
	b *balancer.Balancer,
	l ratelimit.Backend,
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

// WithTracer attaches an OpenTelemetry tracer used to emit a span per
// request.
func (h *Handler) WithTracer(t *tracing.Tracer) *Handler { h.tracer = t; return h }

// rawForLog returns raw only if the effective logging policy permits
// persisting the client-original request body. All error paths must use
// this helper before calling recordLog, otherwise log_raw_request=false
// would still leak request bodies via gateway-side rejection logs.
func (h *Handler) rawForLog(k *store.APIKey, raw []byte) []byte {
	if !h.shouldLog(k, "log_raw_request") {
		return nil
	}
	return raw
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

	// addResolved adds an entry checking permission against BOTH the
	// displayed name and (for aliases) its internal model. Without the
	// internal-model check, /v1/models could advertise an alias whose
	// internal model is in DeniedModels — and the user would then get a
	// 403 only at request time.
	addResolved := func(name, internal string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		if apiKey != nil && !apiKey.ModelAllowedResolved(name, internal) {
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
	add := func(name string) { addResolved(name, name) }

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
		// Resolve the alias to its terminal internal model so a key
		// that denies the internal model can't see the alias either.
		internal, _ := h.store.ResolveAlias(a.Alias)
		addResolved(a.Alias, internal)
	}
	// Orchestration virtual models (Tier-A router / Tier-B conductor).
	if h.orchestrator != nil {
		for _, vm := range h.orchestrator.VirtualModels() {
			add(vm)
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

		var span *tracing.Span
		ctx := r.Context()
		if h.tracer != nil {
			ctx, span = h.tracer.Start(ctx, "gateway.forward")
			span.Attributes["request_id"] = requestID
			span.Attributes["endpoint"] = upstreamPath
			r = r.WithContext(ctx)
			defer h.tracer.End(span)
		}

		apiKey, _ := auth.FromContext(r.Context())
		clientIP := auth.ClientIPFromContext(r.Context())
		bodyLimit := int64(h.cfg.Server.RequestBodyLimitMB) * 1024 * 1024
		if bodyLimit <= 0 {
			bodyLimit = 50 * 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			code := "invalid_body"
			status := http.StatusBadRequest
			if errors.As(err, &maxErr) {
				code = "payload_too_large"
				status = http.StatusRequestEntityTooLarge
			}
			// raw may be partial / truncated here; even if logging policy
			// allowed raw_request, persisting a partial body is more
			// confusing than useful, so we drop it.
			h.recordLog(r.Context(), requestID, apiKey, clientIP, "", "", "", upstreamPath, false, status, code, nil, time.Since(started).Milliseconds(), 0, nil, nil)
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
			h.recordLog(r.Context(), requestID, apiKey, clientIP, "", "", "", upstreamPath, false, http.StatusBadRequest, "invalid_body", nil, time.Since(started).Milliseconds(), 0, nil, nil)
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest("Empty request body", "invalid_body"))
			return
		}

		// Decode just enough to find "model", "stream" and the optional
		// orchestration "secret_level"; preserve every other field.
		var peek struct {
			Model       string `json:"model"`
			Stream      *bool  `json:"stream"`
			SecretLevel *int   `json:"secret_level"`
		}
		if err := json.Unmarshal(raw, &peek); err != nil {
			h.recordLog(r.Context(), requestID, apiKey, clientIP, "", "", "", upstreamPath, false, http.StatusBadRequest, "invalid_json", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Invalid JSON: "+err.Error(),
				"invalid_json",
			))
			return
		}
		if peek.Model == "" {
			h.recordLog(r.Context(), requestID, apiKey, clientIP, "", "", "", upstreamPath, false, http.StatusBadRequest, "missing_model", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
			proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
				"Missing required field: model",
				"missing_model",
			))
			return
		}
		isStream := peek.Stream != nil && *peek.Stream

		// Resolve alias -> internal model. Both names are recorded for metrics labels.
		internalModel, forwardName := h.store.ResolveAlias(peek.Model)

		// API key model permission. Both the requested (alias) and the
		// resolved internal model are checked so an alias cannot bypass
		// DeniedModels.
		if apiKey != nil && !apiKey.ModelAllowedResolved(peek.Model, internalModel) {
			h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusForbidden, "model_not_allowed", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
			proxy.WriteError(w, http.StatusForbidden, proxy.PermissionError(
				fmt.Sprintf("The API key is not allowed to use model: %s", peek.Model),
				"model_not_allowed",
			))
			return
		}

		// Orchestration: if the resolved model is one of the Fugu-style
		// virtual models, hand the whole request to the orchestrator,
		// which fans out to the worker pool. This must run before the
		// registry / capability / backend checks below, because a virtual
		// model has no backends of its own.
		if h.orchestrator != nil && h.orchestrator.Handles(internalModel) {
			// Honor a capability descriptor declared on the virtual model
			// (e.g. vision=false) before fanning out — the conductor
			// flattens content to text, so capabilities the pool can't
			// satisfy should be rejected up front rather than silently
			// dropped.
			if m, ok := h.store.Model(internalModel); ok {
				if reason := capability.Check(m.CapabilityMode, m.Capabilities, raw); reason != "" {
					h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusBadRequest, reason, nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
					proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
						fmt.Sprintf("Model %s does not support this capability: %s", peek.Model, reason),
						reason,
					))
					return
				}
			}
			if span != nil {
				span.Attributes["model"] = peek.Model
				span.Attributes["internal_model"] = internalModel
				span.Attributes["orchestration"] = true
			}
			secretLevel := h.secretLevel(r, peek.SecretLevel)
			h.serveOrchestration(w, r, requestID, started, apiKey, clientIP, internalModel, isStream, raw, secretLevel)
			return
		}

		// Explicit model registry: if the resolved internal model has a
		// registry entry with enabled=false, reject before backend
		// selection. This makes Admin disable a real kill-switch instead
		// of only hiding the model from /v1/models.
		if m, ok := h.store.Model(internalModel); ok {
			if !m.Enabled {
				h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusNotFound, "model_not_found", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
				proxy.WriteError(w, http.StatusNotFound, proxy.NotFound(
					fmt.Sprintf("Model is disabled: %s", peek.Model),
					"model_not_found",
				))
				return
			}
			if reason := capability.Check(m.CapabilityMode, m.Capabilities, raw); reason != "" {
				h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusBadRequest, reason, nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
				proxy.WriteError(w, http.StatusBadRequest, proxy.InvalidRequest(
					fmt.Sprintf("Model %s does not support this capability: %s", peek.Model, reason),
					reason,
				))
				return
			}
		}

		// Look up backends supporting this model.
		candidates := h.store.BackendsForModel(internalModel)
		if len(candidates) == 0 {
			h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusNotFound, "model_not_found", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
			proxy.WriteError(w, http.StatusNotFound, proxy.NotFound(
				fmt.Sprintf("Unknown model: %s", peek.Model),
				"model_not_found",
			))
			return
		}

		// Apply rate-limit / quota / concurrency / queue admission.
		release, code, status := h.admit(r.Context(), apiKey, internalModel)
		if code != "" {
			if isQuotaCode(code) {
				h.metrics.QuotaHits.WithLabelValues(apiKeyID(apiKey), code).Inc()
			}
			h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, status, code, nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
			proxy.WriteError(w, status, proxy.RateLimit("Rejected: "+code, code))
			return
		}
		defer release()

		// Filter for healthy + within-capacity, then pick one.
		ready := filterRoutable(candidates, h.cfg.Routing.AllowDegradedBackends)
		if len(ready) == 0 {
			h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, "", upstreamPath, isStream, http.StatusServiceUnavailable, "no_healthy_backend", nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
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
		policyLabel := string(policy)
		if policyLabel == "" {
			policyLabel = "unknown"
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
			ready = filterRoutable(candidates, h.cfg.Routing.AllowDegradedBackends)
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
		usageSeen := false
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
					usageSeen = true
				}
				h.recordUsage(u, internalModel, picked.ID, apiKeyLabel, apiKey)
			},
			OnStreamUsage: func(u *proxy.Usage) {
				if u != nil {
					capturedUsage = *u
					usageSeen = true
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

		if span != nil {
			span.Attributes["model"] = peek.Model
			span.Attributes["internal_model"] = internalModel
			span.Attributes["backend_id"] = picked.ID
			span.Attributes["stream"] = isStream
			span.Attributes["routing_policy"] = policyLabel
		}
		statusCode, ferr := h.proxy.Forward(r.Context(), w, r, opts)
		success := ferr == nil && statusCode >= 200 && statusCode < 400
		picked.ReleaseSlot(success)
		if span != nil {
			span.Attributes["status_code"] = statusCode
			span.Attributes["latency_ms"] = time.Since(started).Milliseconds()
			if ttftMS > 0 {
				span.Attributes["ttft_ms"] = ttftMS
			}
			if success {
				span.Status = "ok"
			} else {
				span.Status = "error"
				if ferr != nil {
					span.Attributes["error"] = ferr.Error()
				}
			}
		}
		if !success {
			h.metrics.BackendErrors.WithLabelValues(picked.ID, statusCodeLabel(statusCode)).Inc()
		}
		if statusCode == http.StatusGatewayTimeout {
			h.metrics.Timeouts.WithLabelValues(picked.ID, "backend").Inc()
		} else if ferr != nil && ferr.Error() == "stream idle timeout" {
			h.metrics.Timeouts.WithLabelValues(picked.ID, "stream_idle").Inc()
		}
		// API-key request stats must be updated regardless of whether the
		// backend returned a usage block (or any body at all). Token totals
		// are still attributed via recordUsage.
		if apiKey != nil {
			apiKey.TouchRequest()
		}
		// If the backend didn't return a usage block on a successful
		// response, fall back to a conservative estimate so token-based
		// quotas/limits don't permanently silently bypass.
		if success && !usageSeen && apiKey != nil {
			est := estimateTokens(raw, extractMaxTokens(raw))
			estUsage := proxy.Usage{TotalTokens: est}
			capturedUsage = estUsage
			h.recordUsage(&estUsage, internalModel, picked.ID, apiKeyLabel, apiKey)
		}
		h.metrics.Requests.WithLabelValues(
			upstreamPath, internalModel, picked.ID, apiKeyLabel, statusCodeLabel(statusCode), boolLabel(isStream), policyLabel,
		).Inc()
		h.metrics.RequestLatency.WithLabelValues(
			upstreamPath, internalModel, picked.ID, boolLabel(isStream), policyLabel,
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
				// Persist the client-original body so alias rewrites don't
				// obscure what the caller actually sent.
				fields["raw_request"] = json.RawMessage(raw)
				if !bytes.Equal(raw, forwardBody) {
					fields["forwarded_request"] = json.RawMessage(forwardBody)
				}
			}
			if clientIP != "" {
				fields["client_ip"] = clientIP
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
				// Persist what the client actually sent, not the
				// alias-rewritten body the backend received.
				rawReqForLog = raw
			}
			var rawRespForLog []byte
			if isStream && captureStreamChunks {
				rawRespForLog = streamBuf
			} else if !isStream && captureRawResp {
				rawRespForLog = rawResp
			}
			h.recordLog(r.Context(), requestID, apiKey, clientIP, peek.Model, internalModel, picked.ID,
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
	clientIP, model, internalModel, backendID, endpoint string, stream bool, statusCode int,
	errorCode string, usage *proxy.Usage, latencyMS, ttftMS int64,
	rawReq, rawResp []byte) {
	if h.logstore == nil {
		return
	}
	rec := &logstore.RequestLog{
		ID:            uuid.New().String(),
		RequestID:     requestID,
		ClientIP:      clientIP,
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

// estimateTokens produces a conservative token estimate from the
// inbound request body. It is used as a fallback when the backend does
// not return a usage block so that token quotas and per-minute token
// limits still receive a signal. The estimate is intentionally simple
// (1 token per ~4 bytes of body) — overestimating is the safe failure
// mode for a quota system.
func estimateTokens(rawReq []byte, maxTokensFromBody int64) int64 {
	approx := int64(len(rawReq)) / 4
	if approx < 1 {
		approx = 1
	}
	if maxTokensFromBody > 0 {
		approx += maxTokensFromBody
	} else {
		approx += 256 // assume a small completion
	}
	return approx
}

func extractMaxTokens(raw []byte) int64 {
	var peek struct {
		MaxTokens           *int64 `json:"max_tokens"`
		MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return 0
	}
	if peek.MaxCompletionTokens != nil && *peek.MaxCompletionTokens > 0 {
		return *peek.MaxCompletionTokens
	}
	if peek.MaxTokens != nil && *peek.MaxTokens > 0 {
		return *peek.MaxTokens
	}
	return 0
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
		// Request counter is incremented unconditionally elsewhere
		// (see Forward); attribute the token total only.
		apiKey.AddTokens(u.TotalTokens)
		// Use the same bucket key as CheckAndReserve so token-per-minute
		// and daily-token limits actually see this accumulation.
		h.limiter.AddTokens("key:"+apiKey.ID, u.TotalTokens)
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
			if isQuotaCode(reject) {
				h.metrics.QuotaHits.WithLabelValues(k.ID, reject).Inc()
			} else {
				h.metrics.RateLimitHits.WithLabelValues(k.ID, reject).Inc()
			}
			doRelease()
			return noop, reject, http.StatusTooManyRequests
		}

		// 2. Monthly quota (separate manager since rolling daily counters
		// can't be reused for monthly accumulation).
		if h.quota != nil && k.Quota != nil {
			if reject := h.quota.Check(k.ID, 0, 0, k.Quota.MonthlyRequests, k.Quota.MonthlyTokens); reject != "" {
				h.metrics.QuotaHits.WithLabelValues(k.ID, reject).Inc()
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

// filterRoutable returns backends admissible for routing. It delegates to
// store.FilterRoutable so the main path and the orchestrator share one
// definition of "routable" (see store.FilterRoutable for the rules).
func filterRoutable(in []*store.Backend, allowDegraded bool) []*store.Backend {
	return store.FilterRoutable(in, allowDegraded)
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

// isQuotaCode reports whether a rejection code came from the daily /
// monthly quota path (vs. per-minute rate limit or concurrency cap).
// Used to feed the separate quota counter for accurate dashboarding.
func isQuotaCode(code string) bool {
	switch code {
	case "daily_request_limit_exceeded", "daily_token_limit_exceeded",
		"daily_request_quota_exceeded", "daily_token_quota_exceeded",
		"monthly_request_quota_exceeded", "monthly_token_quota_exceeded":
		return true
	}
	return false
}
