package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// newTestOrchestrator wires an orchestrator to a single mock upstream that
// echoes the requested model in the completion content, so tests can assert
// which worker was actually dispatched to.
func newTestOrchestrator(t *testing.T, oc config.OrchestrationConfig) (*Orchestrator, *recordingServer) {
	t.Helper()
	srv := &recordingServer{}
	ts := httptest.NewServer(http.HandlerFunc(srv.handle))
	t.Cleanup(ts.Close)

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{ID: "be-gptoss", BaseURL: ts.URL, Enabled: true, Models: []string{"gpt-oss-120b"}, Weight: 1},
			{ID: "be-gemma", BaseURL: ts.URL, Enabled: true, Models: []string{"gemma-4-26b"}, Weight: 1},
			{ID: "be-qwen", BaseURL: ts.URL, Enabled: true, Models: []string{"qwen3.6-27b"}, Weight: 1},
		},
		Orchestration: oc,
	}
	s := store.New("")
	if err := s.LoadFromConfig(cfg); err != nil {
		t.Fatalf("load store: %v", err)
	}
	o := New(oc, s, balancer.New(), nil)
	return o, srv
}

type recordingServer struct {
	models []string // models seen, in order
}

func (s *recordingServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	s.models = append(s.models, req.Model)
	resp := map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]string{"role": "assistant", "content": "answer from " + req.Model},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func defaultWorkers() []config.OrchestrationWorker {
	return []config.OrchestrationWorker{
		{ID: "qwen", Model: "qwen3.6-27b", Tasks: []string{"code", "zh", "verify"}, Strength: 0.82, Cost: 2},
		{ID: "gptoss", Model: "gpt-oss-120b", Tasks: []string{"reasoning", "verify"}, Strength: 0.90, Cost: 5},
		{ID: "gemma", Model: "gemma-4-26b", Tasks: []string{"general"}, Strength: 0.70, Cost: 1},
	}
}

func baseConfig() config.OrchestrationConfig {
	return config.OrchestrationConfig{
		Enabled:             true,
		RouterModel:         "fugu-auto",
		ConductorModel:      "fugu-ultra",
		ConfidenceThreshold: 0.55,
		MaxSteps:            5,
		RequestTimeoutMS:    5000,
		SecretLevelHeader:   "X-Secret-Level",
		Workers:             defaultWorkers(),
	}
}

func chatBody(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":    "fugu-auto",
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	return b
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"code", "Please write a Go function to reverse a slice and debug this stack trace", TaskCode},
		{"reasoning", "Prove that the sum is optimal and calculate the probability step by step", TaskReasoning},
		{"zh", "請幫我把這段文字翻譯成英文並做摘要整理", TaskZh},
		{"general", "Hi there, how are you today?", TaskGeneral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.text)
			if got.Task != c.want {
				t.Fatalf("classify(%q).Task = %q, want %q (scores=%v)", c.text, got.Task, c.want, got.Scores)
			}
		})
	}
}

func TestClassifyAmbiguousLowConfidence(t *testing.T) {
	// Mixed code + reasoning signals → no single task dominates → low conf.
	got := classify("Write a function to prove this theorem and calculate the derivative, debug the compiler error")
	if got.Confidence >= 0.55 {
		t.Fatalf("expected low confidence for mixed prompt, got %v (scores=%v)", got.Confidence, got.Scores)
	}
}

func TestTierARoutesCodeToQwen(t *testing.T) {
	o, srv := newTestOrchestrator(t, baseConfig())
	res, err := o.Handle(context.Background(), "fugu-auto", chatBody("write a python function and fix the bug in this code"), 0)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Tier != "router" {
		t.Fatalf("tier = %q, want router", res.Tier)
	}
	if len(res.Steps) != 1 || res.Steps[0].WorkerID != "qwen" {
		t.Fatalf("expected single qwen step, got %+v", res.Steps)
	}
	if !strings.Contains(res.Content, "qwen3.6-27b") {
		t.Fatalf("content not from qwen: %q", res.Content)
	}
	if srv.models[0] != "qwen3.6-27b" {
		t.Fatalf("dispatched to %q, want qwen3.6-27b", srv.models[0])
	}
}

func TestTierARoutesReasoningToGptoss(t *testing.T) {
	o, _ := newTestOrchestrator(t, baseConfig())
	res, err := o.Handle(context.Background(), "fugu-auto", chatBody("prove this theorem and calculate the integral step by step"), 0)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Steps[0].WorkerID != "gptoss" {
		t.Fatalf("reasoning should route to gptoss, got %q", res.Steps[0].WorkerID)
	}
}

func TestTierALowConfidenceEscalatesToConductor(t *testing.T) {
	o, _ := newTestOrchestrator(t, baseConfig())
	res, err := o.Handle(context.Background(), "fugu-auto",
		chatBody("write a function to prove this theorem and calculate the derivative, debug the compiler error"), 0)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !res.Escalated {
		t.Fatalf("expected escalation on low confidence")
	}
	if res.Tier != "conductor" {
		t.Fatalf("expected escalation to conductor tier, got %q", res.Tier)
	}
}

func TestTierBConductorRunsTrinity(t *testing.T) {
	o, _ := newTestOrchestrator(t, baseConfig())
	res, err := o.Handle(context.Background(), "fugu-ultra", chatBody("write a python function to fix this bug"), 0)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Tier != "conductor" {
		t.Fatalf("tier = %q, want conductor", res.Tier)
	}
	if len(res.Steps) < 3 {
		t.Fatalf("expected at least 3 conductor steps, got %d: %+v", len(res.Steps), res.Steps)
	}
	if len(res.Steps) > o.cfg.MaxSteps {
		t.Fatalf("steps %d exceed max_steps %d", len(res.Steps), o.cfg.MaxSteps)
	}
	// Verifier must be a different model than the worker (heterogeneous check).
	var worker, verifier string
	for _, s := range res.Steps {
		switch s.Role {
		case roleWorker:
			worker = s.WorkerID
		case roleVerifier:
			verifier = s.WorkerID
		}
	}
	if verifier == "" {
		t.Fatalf("no verifier step ran: %+v", res.Steps)
	}
	if verifier == worker {
		t.Fatalf("verifier (%s) must differ from worker (%s)", verifier, worker)
	}
	// Usage is aggregated across all steps.
	if res.Usage.TotalTokens != int64(15*len(res.Steps)) {
		t.Fatalf("usage not aggregated: got %d for %d steps", res.Usage.TotalTokens, len(res.Steps))
	}
}

func TestAccessListExcludesBySecretLevel(t *testing.T) {
	oc := baseConfig()
	// Make gemma a "cloud" worker that may only see secret level <= 1.
	for i := range oc.Workers {
		if oc.Workers[i].ID == "gemma" {
			oc.Workers[i].SecretMaxLevel = 1
		}
	}
	o, _ := newTestOrchestrator(t, oc)

	eligLow := o.eligibleWorkers(0)
	if len(eligLow) != 3 {
		t.Fatalf("secret 0 should admit all workers, got %d", len(eligLow))
	}
	eligHigh := o.eligibleWorkers(3)
	if len(eligHigh) != 2 {
		t.Fatalf("secret 3 should exclude gemma, got %d", len(eligHigh))
	}
	for _, w := range eligHigh {
		if w.ID == "gemma" {
			t.Fatalf("gemma must be excluded at secret level 3")
		}
	}
}

func TestMaxStepsBudgetTrims(t *testing.T) {
	oc := baseConfig()
	oc.MaxSteps = 2 // only solver + verifier; final = draft
	o, _ := newTestOrchestrator(t, oc)
	res, err := o.Handle(context.Background(), "fugu-ultra", chatBody("write a function"), 0)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(res.Steps) > 2 {
		t.Fatalf("max_steps=2 should cap steps, got %d", len(res.Steps))
	}
	if res.Content == "" {
		t.Fatalf("expected a final answer even with trimmed budget")
	}
}

func TestHandlesAndVirtualModels(t *testing.T) {
	o, _ := newTestOrchestrator(t, baseConfig())
	if !o.Handles("fugu-auto") || !o.Handles("fugu-ultra") {
		t.Fatalf("Handles should recognize both virtual models")
	}
	if o.Handles("gpt-oss-120b") {
		t.Fatalf("Handles must not claim a real worker model")
	}
	vm := o.VirtualModels()
	if len(vm) != 2 {
		t.Fatalf("expected 2 virtual models, got %v", vm)
	}
}
