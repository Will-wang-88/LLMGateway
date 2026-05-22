package backend

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type HealthChecker struct {
	store        *store.Store
	defaults     config.HealthCheckConfig
	httpClient   *http.Client
	logger       *logging.Logger
	statusChange func(b *store.Backend, status store.BackendStatus)

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewHealthChecker(s *store.Store, defaults config.HealthCheckConfig, logger *logging.Logger, onChange func(*store.Backend, store.BackendStatus)) *HealthChecker {
	return &HealthChecker{
		store:        s,
		defaults:     defaults,
		httpClient:   &http.Client{Timeout: time.Duration(defaults.TimeoutMS) * time.Millisecond},
		logger:       logger,
		statusChange: onChange,
		stop:         make(chan struct{}),
	}
}

func (h *HealthChecker) Start() {
	h.wg.Add(1)
	go h.run()
}

func (h *HealthChecker) Stop() {
	close(h.stop)
	h.wg.Wait()
}

func (h *HealthChecker) run() {
	defer h.wg.Done()
	interval := time.Duration(h.defaults.IntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	h.checkAll()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.checkAll()
		}
	}
}

func (h *HealthChecker) checkAll() {
	for _, b := range h.store.Backends() {
		if !b.Enabled {
			continue
		}
		go h.checkOne(b)
	}
}

func (h *HealthChecker) checkOne(b *store.Backend) {
	hc := h.defaults
	if b.HealthCheck != nil {
		if b.HealthCheck.IntervalMS > 0 {
			hc.IntervalMS = b.HealthCheck.IntervalMS
		}
		if b.HealthCheck.TimeoutMS > 0 {
			hc.TimeoutMS = b.HealthCheck.TimeoutMS
		}
		if b.HealthCheck.Path != "" {
			hc.Path = b.HealthCheck.Path
		}
	}
	if hc.Path == "" {
		hc.Path = "/models"
	}
	timeout := time.Duration(hc.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	url := b.BaseURL + hc.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.record(b, false, 0, fmt.Sprintf("build request: %v", err))
		return
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	start := time.Now()
	resp, err := h.httpClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		h.record(b, false, latency, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		// 2xx, 3xx and 4xx all mean the server is responsive enough to route to.
		// 4xx still indicates the backend is alive (e.g. 401 if api_key not provided).
		// Only 5xx and network errors mark it unhealthy.
		if resp.StatusCode >= 400 {
			h.record(b, true, latency, fmt.Sprintf("health probe returned %d (treated as alive)", resp.StatusCode))
			return
		}
		h.record(b, true, latency, "")
		return
	}
	h.record(b, false, latency, fmt.Sprintf("status %d", resp.StatusCode))
}

func (h *HealthChecker) record(b *store.Backend, ok bool, latencyMS int64, errMsg string) {
	changed, status := b.RecordHealthCheck(ok, latencyMS, errMsg)
	if changed && h.statusChange != nil {
		h.statusChange(b, status)
	}
	if changed {
		h.logger.Info("backend status changed", logging.F(
			"backend_id", b.ID,
			"status", string(status),
			"latency_ms", latencyMS,
			"error", errMsg,
		))
	}
}

// CheckOnce performs an immediate health check on a backend and returns the result.
func (h *HealthChecker) CheckOnce(b *store.Backend) {
	h.checkOne(b)
}
