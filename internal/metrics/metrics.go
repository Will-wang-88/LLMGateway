package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	registry *prometheus.Registry

	Requests         *prometheus.CounterVec
	RequestLatency   *prometheus.HistogramVec
	TTFT             *prometheus.HistogramVec
	ActiveRequests   *prometheus.GaugeVec
	PromptTokens     *prometheus.CounterVec
	CompletionTokens *prometheus.CounterVec
	TotalTokens      *prometheus.CounterVec
	ReasoningTokens  *prometheus.CounterVec
	BackendErrors    *prometheus.CounterVec
	BackendStatus    *prometheus.GaugeVec
	RateLimitHits    *prometheus.CounterVec
	QuotaHits        *prometheus.CounterVec
	Timeouts         *prometheus.CounterVec
	QueueDepth       *prometheus.GaugeVec

	// Orchestration (Fugu-style model routing) metrics.
	OrchRoutes      *prometheus.CounterVec
	OrchEscalations *prometheus.CounterVec
	OrchSteps       *prometheus.CounterVec
}

// New constructs a Metrics with its own private registry so multiple instances
// can co-exist in the same process (useful for tests).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)
	return &Metrics{
		registry: reg,
		Requests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_requests_total",
			Help: "Total requests handled by the gateway",
		}, []string{"endpoint", "model", "backend", "api_key", "status", "stream", "routing_policy"}),
		RequestLatency: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmgw_request_latency_seconds",
			Help:    "Request latency in seconds",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"endpoint", "model", "backend", "stream", "routing_policy"}),
		TTFT: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmgw_ttft_seconds",
			Help:    "Time to first token (streaming) in seconds",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"model", "backend"}),
		ActiveRequests: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llmgw_active_requests",
			Help: "Active requests being proxied",
		}, []string{"model", "backend"}),
		PromptTokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_prompt_tokens_total",
			Help: "Total prompt tokens reported by backends",
		}, []string{"model", "backend", "api_key"}),
		CompletionTokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_completion_tokens_total",
			Help: "Total completion tokens reported by backends",
		}, []string{"model", "backend", "api_key"}),
		TotalTokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_total_tokens_total",
			Help: "Total tokens reported by backends",
		}, []string{"model", "backend", "api_key"}),
		ReasoningTokens: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_reasoning_tokens_total",
			Help: "Reasoning tokens reported by backends",
		}, []string{"model", "backend", "api_key"}),
		BackendErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_backend_errors_total",
			Help: "Backend errors",
		}, []string{"backend", "error_code"}),
		BackendStatus: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llmgw_backend_status",
			Help: "Backend status (1 healthy, 0 unhealthy)",
		}, []string{"backend"}),
		RateLimitHits: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_rate_limit_total",
			Help: "Rate-limit events (per-minute / concurrent / queue). Quota events are tracked separately in llmgw_quota_total.",
		}, []string{"api_key", "code"}),
		QuotaHits: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_quota_total",
			Help: "Daily / monthly quota rejections (request and token)",
		}, []string{"api_key", "code"}),
		Timeouts: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_timeouts_total",
			Help: "Backend / stream timeouts split by kind",
		}, []string{"backend", "kind"}),
		QueueDepth: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "llmgw_queue_depth",
			Help: "Pending requests queue depth",
		}, []string{"scope"}),
		OrchRoutes: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_orchestration_routes_total",
			Help: "Orchestrator routing decisions by tier, chosen worker, task class and outcome",
		}, []string{"tier", "worker", "task", "outcome"}),
		OrchEscalations: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_orchestration_escalations_total",
			Help: "Tier-A -> Tier-B escalations by reason",
		}, []string{"reason"}),
		OrchSteps: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgw_orchestration_steps_total",
			Help: "Conductor (Tier-B) worker steps executed by role and worker",
		}, []string{"role", "worker"}),
	}
}

// Registry returns the underlying prometheus registry for this Metrics instance.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
