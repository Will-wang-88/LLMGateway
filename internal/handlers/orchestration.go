package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/orchestrator"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// WithOrchestrator attaches the Fugu-style orchestration layer. When set,
// requests whose model resolves to one of the orchestrator's virtual models
// are served by the orchestrator instead of being forwarded to a single
// backend.
func (h *Handler) WithOrchestrator(o *orchestrator.Orchestrator) *Handler {
	h.orchestrator = o
	return h
}

// metricsSink adapts *metrics.Metrics to orchestrator.MetricsSink.
type metricsSink struct{ m *metrics.Metrics }

func (s metricsSink) IncRoute(tier, worker, task, outcome string) {
	s.m.OrchRoutes.WithLabelValues(tier, worker, task, outcome).Inc()
}
func (s metricsSink) IncEscalation(reason string) {
	s.m.OrchEscalations.WithLabelValues(reason).Inc()
}
func (s metricsSink) IncStep(role, worker string) {
	s.m.OrchSteps.WithLabelValues(role, worker).Inc()
}
func (s metricsSink) IncBackendError(backend, code string) {
	s.m.BackendErrors.WithLabelValues(backend, code).Inc()
}
func (s metricsSink) IncTimeout(backend, kind string) {
	s.m.Timeouts.WithLabelValues(backend, kind).Inc()
}

// OrchestratorMetricsSink exposes the handler's metrics as a sink the
// orchestrator can be wired with at construction time in main.
func OrchestratorMetricsSink(m *metrics.Metrics) orchestrator.MetricsSink { return metricsSink{m: m} }

// serveOrchestration runs the orchestration path for a virtual model. It
// applies the same admission controls (rate limit / quota / concurrency /
// queue) as the normal forward path, then hands the request to the
// orchestrator and writes back an OpenAI-compatible response.
func (h *Handler) serveOrchestration(w http.ResponseWriter, r *http.Request, requestID string, started time.Time,
	apiKey *store.APIKey, clientIP, virtualModel string, isStream bool, raw []byte, secretLevel int) {

	ctx := r.Context()
	apiKeyLabel := apiKeyID(apiKey)

	// Admission: virtual models share the same per-key limits and the
	// per-model queue (keyed on the virtual model name).
	release, code, status := h.admit(ctx, apiKey, virtualModel)
	if code != "" {
		if isQuotaCode(code) {
			h.metrics.QuotaHits.WithLabelValues(apiKeyLabel, code).Inc()
		}
		h.recordLog(ctx, requestID, apiKey, clientIP, virtualModel, virtualModel, "orchestrator", "/chat/completions", isStream, status, code, nil, time.Since(started).Milliseconds(), 0, h.rawForLog(apiKey, raw), nil)
		proxy.WriteError(w, status, proxy.RateLimit("Rejected: "+code, code))
		return
	}
	defer release()

	// Track in-flight orchestration requests on the same gauge the direct
	// path uses (backend label "orchestrator").
	h.metrics.ActiveRequests.WithLabelValues(virtualModel, "orchestrator").Inc()
	defer h.metrics.ActiveRequests.WithLabelValues(virtualModel, "orchestrator").Dec()

	result, err := h.orchestrator.Handle(ctx, virtualModel, raw, secretLevel)
	if err != nil {
		latency := time.Since(started).Milliseconds()
		// Even on failure, charge tokens already consumed by completed
		// steps (Tier-B returns partial usage) so quota isn't bypassed.
		var usage *proxy.Usage
		if result != nil {
			h.chargeUsage(result, virtualModel, apiKeyLabel, apiKey, raw, false)
			usage = &result.Usage
			if apiKey != nil {
				apiKey.TouchRequest()
			}
		}
		h.recordLog(ctx, requestID, apiKey, clientIP, virtualModel, virtualModel, "orchestrator", "/chat/completions", isStream, http.StatusBadGateway, "orchestration_failed", usage, latency, 0, h.rawForLog(apiKey, raw), nil)
		h.metrics.Requests.WithLabelValues("/chat/completions", virtualModel, "orchestrator", apiKeyLabel, "502", boolLabel(isStream), "orchestration").Inc()
		h.logger.Warn("orchestration failed", logging.F("request_id", requestID, "model", virtualModel, "error", err.Error()))
		proxy.WriteError(w, http.StatusBadGateway, proxy.BackendUnavailable("Orchestration failed: "+err.Error(), "orchestration_failed"))
		return
	}

	// Charge usage (with the same conservative estimate fallback the direct
	// path uses when the backend omits a usage block). chargeUsage mutates
	// result.Usage so the response/log reflect what was charged.
	h.chargeUsage(result, virtualModel, apiKeyLabel, apiKey, raw, true)
	if apiKey != nil {
		apiKey.TouchRequest()
	}

	created := time.Now().Unix()
	id := "chatcmpl-" + requestID
	var body []byte
	if isStream {
		writeOrchestrationStream(w, id, virtualModel, created, result)
	} else {
		body = orchestrator.BuildChatCompletion(id, virtualModel, created, result)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}

	latency := time.Since(started).Milliseconds()
	h.metrics.Requests.WithLabelValues("/chat/completions", virtualModel, "orchestrator", apiKeyLabel, "200", boolLabel(isStream), "orchestration").Inc()
	h.metrics.RequestLatency.WithLabelValues("/chat/completions", virtualModel, "orchestrator", boolLabel(isStream), "orchestration").Observe(time.Since(started).Seconds())

	if h.shouldLog(apiKey, "log_metadata") {
		h.logger.Info("orchestration completed", logging.F(
			"request_id", requestID,
			"model", virtualModel,
			"tier", result.Tier,
			"task", result.Task,
			"confidence", result.Confidence,
			"escalated", result.Escalated,
			"steps", len(result.Steps),
			"total_tokens", result.Usage.TotalTokens,
			"latency_ms", latency,
			"api_key_id", apiKeyLabel,
		))
	}

	if h.logstore != nil {
		var rawReqForLog []byte
		if h.shouldLog(apiKey, "log_raw_request") {
			rawReqForLog = raw
		}
		var rawRespForLog []byte
		if !isStream && h.shouldLog(apiKey, "log_raw_response") {
			if body == nil {
				body = orchestrator.BuildChatCompletion(id, virtualModel, created, result)
			}
			rawRespForLog = body
		}
		h.recordLog(ctx, requestID, apiKey, clientIP, virtualModel, virtualModel, "orchestrator",
			"/chat/completions", isStream, http.StatusOK, "", &result.Usage, latency, 0, rawReqForLog, rawRespForLog)
	}
}

// chargeUsage records token usage for an orchestrated request. On a
// successful request that returned no token counts (worker omitted the
// usage block), it falls back to the same conservative estimate the direct
// path uses so token-based limits/quotas still see a signal. It mutates
// result.Usage in place so the response and logs reflect the charged total.
func (h *Handler) chargeUsage(result *orchestrator.Result, model, apiKeyLabel string, apiKey *store.APIKey, raw []byte, success bool) {
	if success && result.Usage.TotalTokens == 0 && apiKey != nil {
		result.Usage.TotalTokens = estimateTokens(raw, extractMaxTokens(raw))
	}
	h.recordUsage(&result.Usage, model, "orchestrator", apiKeyLabel, apiKey)
}

// secretLevel resolves the request data-sensitivity level for the
// orchestrator access list: the configured header takes precedence, then a
// top-level "secret_level" field already parsed from the body. Negative
// values are clamped to 0 by the orchestrator's access-list check.
func (h *Handler) secretLevel(r *http.Request, bodySecretLevel *int) int {
	hdr := h.cfg.Orchestration.SecretLevelHeader
	if hdr == "" {
		hdr = "X-Secret-Level"
	}
	if v := r.Header.Get(hdr); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if bodySecretLevel != nil {
		return *bodySecretLevel
	}
	return 0
}

// writeOrchestrationStream emits the orchestrated answer as a minimal SSE
// stream so OpenAI streaming clients work unchanged. The orchestrated answer
// is produced as a whole, so it is delivered as a single content chunk
// followed by a terminating chunk that carries usage and the x_orchestration
// route trace (so streaming and non-streaming expose the same metadata),
// then [DONE].
func writeOrchestrationStream(w http.ResponseWriter, id, model string, created int64, result *orchestrator.Result) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	type delta struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	write := func(obj map[string]any) {
		b, _ := json.Marshal(obj)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	base := func(choices []choice) map[string]any {
		return map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": choices,
		}
	}

	write(base([]choice{{Index: 0, Delta: delta{Role: "assistant", Content: result.Content}}}))
	stop := "stop"
	final := base([]choice{{Index: 0, Delta: delta{}, FinishReason: &stop}})
	final["usage"] = result.Usage
	final["x_orchestration"] = map[string]any{
		"tier":       result.Tier,
		"task":       result.Task,
		"confidence": result.Confidence,
		"escalated":  result.Escalated,
		"route":      result.Steps,
	}
	write(final)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
