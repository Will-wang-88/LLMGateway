// Package orchestrator implements a Fugu-style, behaviour-level model
// orchestration layer on top of the gateway's existing routing machinery.
//
// It does NOT merge model weights (the pool is architecturally
// heterogeneous — MoE + two dense models with different tokenizers, so
// weight merges are impossible). Instead it treats each model as a black
// box and decides, at inference time, which worker(s) should answer a
// request:
//
//   - Tier-A "router" (low latency): a rule-based classifier inspects the
//     request and dispatches it to the single best-suited worker. This is
//     the default, cheap path. When the classifier is not confident the
//     request escalates to Tier-B (or to the strongest worker).
//
//   - Tier-B "conductor" (high quality): a bounded (≤5 step) DAG that
//     decomposes the task across workers in Thinker → Worker → Verifier →
//     Synthesizer roles, enforcing that the verifier is a *different*
//     model from the producer (heterogeneous cross-check), and gating
//     cross-step visibility through an access list.
//
// Both tiers are exposed as ordinary OpenAI-compatible virtual models, so
// clients keep calling a single endpoint.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// MetricsSink receives orchestration telemetry. A nil sink is tolerated
// (see the nil-guards in the call sites).
type MetricsSink interface {
	IncRoute(tier, worker, task, outcome string)
	IncEscalation(reason string)
	IncStep(role, worker string)
	// IncBackendError / IncTimeout mirror the direct path's per-backend
	// error and timeout counters so worker-pool failures are visible on the
	// same dashboards/alerts as normal backend traffic.
	IncBackendError(backend, code string)
	IncTimeout(backend, kind string)
}

// Orchestrator coordinates the worker pool. It reuses the shared store and
// balancer so worker selection honors the same health/capacity rules as the
// main request path.
type Orchestrator struct {
	cfg           config.OrchestrationConfig
	store         *store.Store
	balancer      *balancer.Balancer
	logger        *logging.Logger
	metrics       MetricsSink
	client        *http.Client
	allowDegraded bool
}

// New constructs an Orchestrator. cfg is assumed already validated by
// config.Validate.
func New(cfg config.OrchestrationConfig, s *store.Store, b *balancer.Balancer, log *logging.Logger) *Orchestrator {
	return &Orchestrator{
		cfg:      cfg,
		store:    s,
		balancer: b,
		logger:   log,
		client:   &http.Client{Transport: proxy.DefaultTransport()},
	}
}

// WithMetrics attaches a metrics sink.
func (o *Orchestrator) WithMetrics(m MetricsSink) *Orchestrator { o.metrics = m; return o }

// WithRouting copies the relevant gateway routing defaults so the
// orchestrator's worker dispatch agrees with the direct path (notably
// whether degraded backends are routable).
func (o *Orchestrator) WithRouting(allowDegraded bool) *Orchestrator {
	o.allowDegraded = allowDegraded
	return o
}

// Handles reports whether the given model name is one of the virtual models
// this orchestrator owns.
func (o *Orchestrator) Handles(model string) bool {
	if o == nil || !o.cfg.Enabled {
		return false
	}
	return model == o.cfg.RouterModel || (o.cfg.ConductorModel != "" && model == o.cfg.ConductorModel)
}

// VirtualModels returns the configured virtual model names (for /v1/models).
func (o *Orchestrator) VirtualModels() []string {
	if o == nil || !o.cfg.Enabled {
		return nil
	}
	var out []string
	if o.cfg.RouterModel != "" {
		out = append(out, o.cfg.RouterModel)
	}
	if o.cfg.ConductorModel != "" {
		out = append(out, o.cfg.ConductorModel)
	}
	return out
}

// StepInfo records one node in the route trace, surfaced to clients in the
// response's x_orchestration extension.
type StepInfo struct {
	Step      int    `json:"step"`
	Role      string `json:"role"`
	WorkerID  string `json:"worker"`
	Model     string `json:"model"`
	BackendID string `json:"backend,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// Result is the orchestrated outcome.
type Result struct {
	Content    string
	Usage      proxy.Usage
	Tier       string
	Task       string
	Confidence float64
	Escalated  bool
	Steps      []StepInfo
}

// Handle runs the orchestration for one request. virtualModel selects the
// tier; rawBody is the client's OpenAI chat-completions body; secretLevel is
// the request data-sensitivity used for access-list filtering.
func (o *Orchestrator) Handle(ctx context.Context, virtualModel string, rawBody []byte, secretLevel int) (*Result, error) {
	msgs, params, err := parseRequest(rawBody)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("orchestration requires at least one message")
	}

	eligible := o.eligibleWorkers(secretLevel)
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no worker is permitted to process a secret level %d request", secretLevel)
	}

	conductorRequested := o.cfg.ConductorModel != "" && virtualModel == o.cfg.ConductorModel
	if conductorRequested {
		return o.runConductor(ctx, msgs, params, eligible, classify(lastUserText(msgs)), false)
	}

	// Tier-A router path.
	cls := classify(lastUserText(msgs))
	if cls.Confidence < o.cfg.ConfidenceThreshold {
		// Low confidence: escalate. Prefer Tier-B if available, else fall
		// back to the strongest eligible worker (section 8).
		o.incEscalation("low_confidence")
		if o.cfg.ConductorModel != "" {
			return o.runConductor(ctx, msgs, params, eligible, cls, true)
		}
		w := strongestWorker(eligible)
		return o.single(ctx, w, msgs, params, cls, "router", true)
	}

	w := o.selectWorker(cls.Task, eligible)
	if w == nil {
		w = strongestWorker(eligible)
	}
	return o.single(ctx, w, msgs, params, cls, "router", false)
}

// single dispatches to one worker and wraps the result (Tier-A).
func (o *Orchestrator) single(ctx context.Context, w *config.OrchestrationWorker, msgs []chatMessage, params genParams, cls classification, tier string, escalated bool) (*Result, error) {
	c, err := o.callWorker(ctx, w, msgs, params)
	if err != nil {
		o.incRoute(tier, w.ID, cls.Task, "error")
		return nil, err
	}
	o.incRoute(tier, w.ID, cls.Task, "ok")
	return &Result{
		Content:    c.Text,
		Usage:      c.Usage,
		Tier:       tier,
		Task:       cls.Task,
		Confidence: cls.Confidence,
		Escalated:  escalated,
		Steps: []StepInfo{{
			Step: 1, Role: "route", WorkerID: w.ID, Model: w.Model,
			BackendID: c.BackendID, LatencyMS: c.LatencyMS,
		}},
	}, nil
}

// eligibleWorkers filters the pool by the access list: a worker may process
// the request only if its SecretMaxLevel admits the request's secret level.
// SecretMaxLevel 0 means "unlimited" (an on-prem worker).
func (o *Orchestrator) eligibleWorkers(secretLevel int) []*config.OrchestrationWorker {
	// A negative (or junk-parsed) level must never widen eligibility: clamp
	// to 0 so an out-of-range header can't defeat the access list.
	if secretLevel < 0 {
		secretLevel = 0
	}
	out := make([]*config.OrchestrationWorker, 0, len(o.cfg.Workers))
	for i := range o.cfg.Workers {
		w := &o.cfg.Workers[i]
		if w.SecretMaxLevel > 0 && secretLevel > w.SecretMaxLevel {
			continue
		}
		out = append(out, w)
	}
	return out
}

// selectWorker chooses the best worker for a task: workers that declare the
// task get a large preference bonus; ties break on strength minus cost
// penalty.
func (o *Orchestrator) selectWorker(task string, eligible []*config.OrchestrationWorker) *config.OrchestrationWorker {
	var best *config.OrchestrationWorker
	bestScore := -1e18
	for _, w := range eligible {
		score := workerStrength(w) - o.cfg.CostPenalty*w.Cost
		if containsFold(w.Tasks, task) {
			score += 1.0
		}
		if score > bestScore {
			bestScore = score
			best = w
		}
	}
	return best
}

func strongestWorker(eligible []*config.OrchestrationWorker) *config.OrchestrationWorker {
	var best *config.OrchestrationWorker
	bestScore := -1e18
	for _, w := range eligible {
		if s := workerStrength(w); s > bestScore {
			bestScore = s
			best = w
		}
	}
	return best
}

func workerStrength(w *config.OrchestrationWorker) float64 {
	if w.Strength == 0 {
		return 0.5
	}
	return w.Strength
}

func (o *Orchestrator) incRoute(tier, worker, task, outcome string) {
	if o.metrics != nil {
		o.metrics.IncRoute(tier, worker, task, outcome)
	}
}

func (o *Orchestrator) incEscalation(reason string) {
	if o.metrics != nil {
		o.metrics.IncEscalation(reason)
	}
}

func (o *Orchestrator) incStep(role, worker string) {
	if o.metrics != nil {
		o.metrics.IncStep(role, worker)
	}
}

func (o *Orchestrator) incBackendError(backend, code string) {
	if o.metrics != nil {
		o.metrics.IncBackendError(backend, code)
	}
}

func (o *Orchestrator) incTimeout(backend, kind string) {
	if o.metrics != nil {
		o.metrics.IncTimeout(backend, kind)
	}
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// lastUserText returns the content of the last user message, or the last
// message if no user message is present.
func lastUserText(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") {
			return msgs[i].Content
		}
	}
	return msgs[len(msgs)-1].Content
}

// parseRequest extracts messages and generation params from an OpenAI
// chat-completions body, flattening multimodal content parts to text.
func parseRequest(raw []byte) ([]chatMessage, genParams, error) {
	var in struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		MaxTokens   *int64   `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, genParams{}, fmt.Errorf("invalid orchestration request: %w", err)
	}
	msgs := make([]chatMessage, 0, len(in.Messages))
	for _, m := range in.Messages {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: flattenContent(m.Content)})
	}
	return msgs, genParams{Temperature: in.Temperature, TopP: in.TopP, MaxTokens: in.MaxTokens}, nil
}

// flattenContent reduces OpenAI message content (a string or an array of
// typed parts) to plain text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// BuildChatCompletion renders a Result as an OpenAI chat.completion object,
// attaching an x_orchestration extension with the route trace.
func BuildChatCompletion(id, model string, created int64, r *Result) []byte {
	type choice struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []choice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: r.Content},
			FinishReason: "stop",
		}},
		"usage": r.Usage,
		"x_orchestration": map[string]any{
			"tier":       r.Tier,
			"task":       r.Task,
			"confidence": r.Confidence,
			"escalated":  r.Escalated,
			"route":      r.Steps,
		},
	}
	out, _ := json.Marshal(resp)
	return out
}
