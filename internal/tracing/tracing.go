package tracing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
)

// Span is a minimal trace span.
type Span struct {
	TraceID    string
	SpanID     string
	ParentID   string
	Name       string
	Start      time.Time
	End        time.Time
	Attributes map[string]any
	Status     string
}

// Tracer collects spans and optionally exports them to an OTLP HTTP endpoint.
type Tracer struct {
	cfg     config.TracingConfig
	logger  *logging.Logger
	enabled bool

	mu      sync.Mutex
	queue   []*Span
	maxQueue int
	flushTk *time.Ticker
	stop    chan struct{}
	wg      sync.WaitGroup
	client  *http.Client
}

// New returns a no-op tracer when cfg.Enabled is false.
func New(cfg config.TracingConfig, logger *logging.Logger) *Tracer {
	t := &Tracer{cfg: cfg, logger: logger, enabled: cfg.Enabled, maxQueue: 1024, stop: make(chan struct{})}
	if t.enabled {
		t.client = &http.Client{Timeout: 5 * time.Second}
		t.flushTk = time.NewTicker(5 * time.Second)
		t.wg.Add(1)
		go t.run()
	}
	return t
}

func (t *Tracer) Stop() {
	if !t.enabled {
		return
	}
	close(t.stop)
	t.wg.Wait()
	t.flush()
}

func (t *Tracer) run() {
	defer t.wg.Done()
	for {
		select {
		case <-t.stop:
			return
		case <-t.flushTk.C:
			t.flush()
		}
	}
}

func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start begins a new span. The returned span is "open" until End is called.
func (t *Tracer) Start(ctx context.Context, name string) (context.Context, *Span) {
	span := &Span{
		Name:       name,
		Start:      time.Now(),
		Attributes: map[string]any{},
	}
	parent, _ := ctx.Value(ctxKey{}).(*Span)
	if parent != nil {
		span.TraceID = parent.TraceID
		span.ParentID = parent.SpanID
	} else {
		span.TraceID = newID(16)
	}
	span.SpanID = newID(8)
	return context.WithValue(ctx, ctxKey{}, span), span
}

type ctxKey struct{}

// End marks a span as complete and enqueues it for export.
func (t *Tracer) End(span *Span) {
	if span == nil {
		return
	}
	span.End = time.Now()
	if !t.enabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) >= t.maxQueue {
		t.queue = t.queue[len(t.queue)/2:]
	}
	t.queue = append(t.queue, span)
}

func (t *Tracer) flush() {
	if !t.enabled || t.cfg.Endpoint == "" {
		t.mu.Lock()
		t.queue = nil
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	batch := t.queue
	t.queue = nil
	t.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	serviceName := t.cfg.Service
	if serviceName == "" {
		serviceName = "llmgateway"
	}
	// Construct an OTLP/HTTP JSON ResourceSpans payload (subset).
	spans := make([]map[string]any, 0, len(batch))
	for _, s := range batch {
		spans = append(spans, map[string]any{
			"traceId":           s.TraceID,
			"spanId":            s.SpanID,
			"parentSpanId":      s.ParentID,
			"name":              s.Name,
			"kind":              2, // SPAN_KIND_SERVER
			"startTimeUnixNano": fmt.Sprintf("%d", s.Start.UnixNano()),
			"endTimeUnixNano":   fmt.Sprintf("%d", s.End.UnixNano()),
			"attributes":        attrsToOTLP(s.Attributes),
			"status":            map[string]any{"code": statusCode(s.Status)},
		})
	}
	payload := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": attrsToOTLP(map[string]any{"service.name": serviceName}),
				},
				"scopeSpans": []any{
					map[string]any{
						"scope": map[string]any{"name": "llmgateway"},
						"spans": spans,
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, t.cfg.Endpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		t.logger.Warn("trace export failed", logging.F("error", err.Error()))
		return
	}
	_ = resp.Body.Close()
}

func attrsToOTLP(m map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]any{"key": k, "value": valueForOTLP(v)})
	}
	return out
}

func valueForOTLP(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"stringValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	case int:
		return map[string]any{"intValue": fmt.Sprintf("%d", x)}
	case int64:
		return map[string]any{"intValue": fmt.Sprintf("%d", x)}
	case float64:
		return map[string]any{"doubleValue": x}
	default:
		return map[string]any{"stringValue": fmt.Sprintf("%v", v)}
	}
}

func statusCode(s string) int {
	if s == "error" {
		return 2
	}
	if s == "ok" {
		return 1
	}
	return 0
}

// SpanFromContext returns the current span if any.
func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(ctxKey{}).(*Span)
	return s
}

// SetAttr sets an attribute on the current span (no-op if none).
func SetAttr(ctx context.Context, k string, v any) {
	if s := SpanFromContext(ctx); s != nil {
		s.Attributes[k] = v
	}
}

// SetStatus sets the span status.
func SetStatus(ctx context.Context, status string) {
	if s := SpanFromContext(ctx); s != nil {
		s.Status = status
	}
}
