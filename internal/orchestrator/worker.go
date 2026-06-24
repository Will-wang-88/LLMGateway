package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/will-wang-88/llmgateway/internal/balancer"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/proxy"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// chatMessage is the minimal OpenAI chat message shape the orchestrator
// emits to workers and returns to clients.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// genParams carries the optional generation knobs the orchestrator passes
// through to workers. They are copied from the inbound request so callers
// keep control over sampling.
type genParams struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int64   `json:"max_tokens,omitempty"`
}

// completion is the result of a single worker call.
type completion struct {
	Text      string
	Usage     proxy.Usage
	WorkerID  string
	Model     string
	BackendID string
	LatencyMS int64
}

// chatRequest is what we POST to a worker backend.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   *int64        `json:"max_tokens,omitempty"`
}

// chatResponse is the subset of the worker response we read.
type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *proxy.Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// callWorker dispatches a single non-streaming chat completion to the named
// worker, picking a healthy backend via the shared balancer, and returns the
// assistant text plus usage. It enforces the worker's concurrency slot and
// the configured per-call timeout.
func (o *Orchestrator) callWorker(ctx context.Context, w *config.OrchestrationWorker, msgs []chatMessage, p genParams) (*completion, error) {
	be, err := o.pickBackend(w.Model)
	if err != nil {
		return nil, err
	}
	if !be.AcquireSlot() {
		return nil, fmt.Errorf("worker %s: backend %s at capacity", w.ID, be.ID)
	}

	body, _ := json.Marshal(chatRequest{
		Model:       w.Model,
		Messages:    msgs,
		Stream:      false,
		Temperature: p.Temperature,
		TopP:        p.TopP,
		MaxTokens:   p.MaxTokens,
	})

	timeout := time.Duration(o.cfg.RequestTimeoutMS) * time.Millisecond
	if be.TimeoutMS > 0 && time.Duration(be.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(be.TimeoutMS) * time.Millisecond
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	url := be.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		be.ReleaseSlot(false)
		return nil, fmt.Errorf("worker %s: build request: %w", w.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if be.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+be.APIKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		be.ReleaseSlot(false)
		return nil, fmt.Errorf("worker %s: backend %s unreachable: %w", w.ID, be.ID, err)
	}
	defer resp.Body.Close()

	var parsed chatResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&parsed); err != nil {
		be.ReleaseSlot(false)
		return nil, fmt.Errorf("worker %s: decode backend response: %w", w.ID, err)
	}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	be.ReleaseSlot(success)

	if !success {
		msg := fmt.Sprintf("status %d", resp.StatusCode)
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return nil, fmt.Errorf("worker %s: backend %s error: %s", w.ID, be.ID, msg)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("worker %s: backend %s returned no choices", w.ID, be.ID)
	}

	c := &completion{
		Text:      parsed.Choices[0].Message.Content,
		WorkerID:  w.ID,
		Model:     w.Model,
		BackendID: be.ID,
		LatencyMS: time.Since(start).Milliseconds(),
	}
	if parsed.Usage != nil {
		c.Usage = *parsed.Usage
	}
	return c, nil
}

// pickBackend selects one routable backend for an internal model name using
// the same health/capacity rules as the main request path.
func (o *Orchestrator) pickBackend(model string) (*store.Backend, error) {
	candidates := o.store.BackendsForModel(model)
	ready := make([]*store.Backend, 0, len(candidates))
	for _, b := range candidates {
		if !b.Enabled {
			continue
		}
		switch b.Status() {
		case store.StatusHealthy, store.StatusUnknown:
		default:
			continue
		}
		if b.Weight == 0 {
			continue
		}
		if b.MaxConcurrentRequests > 0 && b.ActiveRequests() >= int64(b.MaxConcurrentRequests) {
			continue
		}
		ready = append(ready, b)
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("no healthy backend for worker model %q", model)
	}
	picked := o.balancer.Choose(model, balancer.PolicyWeightedRoundRobin, ready, balancer.Hint{})
	if picked == nil {
		return nil, fmt.Errorf("no healthy backend for worker model %q", model)
	}
	return picked, nil
}
