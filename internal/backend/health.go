package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// StatusChangeObserver receives backend status transitions. Used by
// metrics, the dashboard, and the notification subsystem.
type StatusChangeObserver func(b *store.Backend, prev, next store.BackendStatus, latencyMS int64, errMsg string)

type HealthChecker struct {
	store      *store.Store
	defaults   config.HealthCheckConfig
	httpClient *http.Client
	logger     *logging.Logger

	mu        sync.Mutex
	observers []StatusChangeObserver

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewHealthChecker(s *store.Store, defaults config.HealthCheckConfig, logger *logging.Logger, onChange func(*store.Backend, store.BackendStatus)) *HealthChecker {
	hc := &HealthChecker{
		store:      s,
		defaults:   defaults,
		httpClient: &http.Client{Timeout: time.Duration(max(defaults.TimeoutMS, 1)) * time.Millisecond},
		logger:     logger,
		stop:       make(chan struct{}),
	}
	if onChange != nil {
		hc.AddObserver(func(b *store.Backend, _, next store.BackendStatus, _ int64, _ string) {
			onChange(b, next)
		})
	}
	return hc
}

// AddObserver registers a status-change callback. Safe to call before or
// after Start.
func (h *HealthChecker) AddObserver(o StatusChangeObserver) {
	if o == nil {
		return
	}
	h.mu.Lock()
	h.observers = append(h.observers, o)
	h.mu.Unlock()
}

func (h *HealthChecker) emit(b *store.Backend, prev, next store.BackendStatus, latency int64, errMsg string) {
	h.mu.Lock()
	obs := append([]StatusChangeObserver(nil), h.observers...)
	h.mu.Unlock()
	for _, o := range obs {
		o(b, prev, next, latency, errMsg)
	}
}

func (h *HealthChecker) Start() {
	// One goroutine per backend so per-backend interval is honored.
	// Backends added after start are picked up by Rescan().
	for _, b := range h.store.Backends() {
		h.spawn(b)
	}
}

func (h *HealthChecker) spawn(b *store.Backend) {
	h.wg.Add(1)
	go h.loop(b)
}

func (h *HealthChecker) Stop() {
	close(h.stop)
	h.wg.Wait()
}

// Effective returns the merged health check config for a single backend.
func (h *HealthChecker) effective(b *store.Backend) config.HealthCheckConfig {
	hc := h.defaults
	if b.HealthCheck != nil {
		if b.HealthCheck.Enabled != nil {
			hc.Enabled = b.HealthCheck.Enabled
		}
		if b.HealthCheck.Type != "" {
			hc.Type = b.HealthCheck.Type
		}
		if b.HealthCheck.IntervalMS > 0 {
			hc.IntervalMS = b.HealthCheck.IntervalMS
		}
		if b.HealthCheck.TimeoutMS > 0 {
			hc.TimeoutMS = b.HealthCheck.TimeoutMS
		}
		if b.HealthCheck.FailureThreshold > 0 {
			hc.FailureThreshold = b.HealthCheck.FailureThreshold
		}
		if b.HealthCheck.SuccessThreshold > 0 {
			hc.SuccessThreshold = b.HealthCheck.SuccessThreshold
		}
		if b.HealthCheck.Path != "" {
			hc.Path = b.HealthCheck.Path
		}
		if b.HealthCheck.Method != "" {
			hc.Method = b.HealthCheck.Method
		}
		if b.HealthCheck.Body != "" {
			hc.Body = b.HealthCheck.Body
		}
	}
	if hc.Path == "" {
		hc.Path = "/models"
	}
	if hc.Type == "" {
		hc.Type = "http"
	}
	if hc.Method == "" {
		hc.Method = http.MethodGet
	}
	return hc
}

func (h *HealthChecker) loop(b *store.Backend) {
	defer h.wg.Done()
	hc := h.effective(b)
	if hc.Enabled != nil && !*hc.Enabled {
		return
	}
	interval := time.Duration(hc.IntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 5 * time.Second
	}
	h.checkOne(b)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.checkOne(b)
		}
	}
}

func (h *HealthChecker) checkOne(b *store.Backend) {
	if !b.Enabled {
		return
	}
	hc := h.effective(b)
	timeout := time.Duration(hc.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	switch strings.ToLower(hc.Type) {
	case "tcp":
		h.tcpProbe(b, hc, timeout)
	default:
		h.httpProbe(b, hc, timeout)
	}
}

func (h *HealthChecker) httpProbe(b *store.Backend, hc config.HealthCheckConfig, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	url := b.BaseURL + hc.Path
	var body io.Reader
	if hc.Body != "" {
		body = bytes.NewReader([]byte(hc.Body))
	}
	req, err := http.NewRequestWithContext(ctx, hc.Method, url, body)
	if err != nil {
		h.record(b, false, 0, fmt.Sprintf("build request: %v", err))
		return
	}
	if hc.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	start := time.Now()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		h.record(b, false, latency, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		h.record(b, true, latency, "")
		return
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		h.recordDegraded(b, latency, fmt.Sprintf("auth-probe returned %d", resp.StatusCode))
		return
	}
	h.record(b, false, latency, fmt.Sprintf("status %d", resp.StatusCode))
}

func (h *HealthChecker) tcpProbe(b *store.Backend, hc config.HealthCheckConfig, timeout time.Duration) {
	addr := hostPortFromBaseURL(b.BaseURL)
	if addr == "" {
		h.record(b, false, 0, "unable to derive host:port from base_url for tcp probe")
		return
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		h.record(b, false, latency, err.Error())
		return
	}
	_ = conn.Close()
	h.record(b, true, latency, "")
}

func hostPortFromBaseURL(base string) string {
	s := strings.TrimSpace(base)
	for _, pfx := range []string{"https://", "http://"} {
		if strings.HasPrefix(s, pfx) {
			s = s[len(pfx):]
			break
		}
	}
	if idx := strings.IndexAny(s, "/?#"); idx >= 0 {
		s = s[:idx]
	}
	if strings.Contains(s, ":") {
		return s
	}
	if strings.HasPrefix(base, "https://") {
		return s + ":443"
	}
	return s + ":80"
}

func (h *HealthChecker) recordDegraded(b *store.Backend, latencyMS int64, errMsg string) {
	prev := b.Status()
	b.MarkDegraded(latencyMS, errMsg)
	if prev != store.StatusDegraded {
		h.logger.Warn("backend degraded", logging.F(
			"backend_id", b.ID, "latency_ms", latencyMS, "reason", errMsg,
		))
		h.emit(b, prev, store.StatusDegraded, latencyMS, errMsg)
	}
}

func (h *HealthChecker) record(b *store.Backend, ok bool, latencyMS int64, errMsg string) {
	prev := b.Status()
	changed, status := b.RecordHealthCheck(ok, latencyMS, errMsg)
	if changed {
		h.logger.Info("backend status changed", logging.F(
			"backend_id", b.ID,
			"status", string(status),
			"latency_ms", latencyMS,
			"error", errMsg,
		))
		h.emit(b, prev, status, latencyMS, errMsg)
	}
}

// CheckOnce performs an immediate health check on a backend and returns when done.
func (h *HealthChecker) CheckOnce(b *store.Backend) {
	h.checkOne(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
