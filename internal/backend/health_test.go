package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

func newProbeServer(counter *int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(counter, 1)
		w.WriteHeader(http.StatusOK)
	}))
}

// P1-2: backends created via Admin must be picked up by the periodic
// health-check loop, not just receive a single CheckOnce.
func TestAdminCreatedBackendGetsPeriodicHealthChecks(t *testing.T) {
	var probes int64
	be := newProbeServer(&probes)
	defer be.Close()

	s := store.New("hmac")
	hc := NewHealthChecker(s, config.HealthCheckConfig{
		IntervalMS: 30, TimeoutMS: 200, FailureThreshold: 3, SuccessThreshold: 1,
		Path: "/healthz",
	}, logging.New(io.Discard, "error"), nil)
	hc.Start()
	t.Cleanup(hc.Stop)

	// Backend added at runtime (not present at hc.Start()).
	b := &store.Backend{
		ID: "b1", BaseURL: be.URL, Enabled: true, Models: []string{"m1"}, Weight: 1,
	}
	s.UpsertBackend(b)
	hc.AddBackend(b)

	// Wait long enough for at least two probes (interval=30ms).
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt64(&probes); got < 2 {
		t.Errorf("expected >=2 periodic probes for runtime-added backend, got %d", got)
	}
}

// P1-2: spawning the same backend twice must not result in duplicate
// goroutines hammering the probe endpoint.
func TestHealthCheckerDoesNotSpawnDuplicateLoopsForSameBackend(t *testing.T) {
	var probes int64
	be := newProbeServer(&probes)
	defer be.Close()

	s := store.New("hmac")
	hc := NewHealthChecker(s, config.HealthCheckConfig{
		IntervalMS: 25, TimeoutMS: 200, FailureThreshold: 3, SuccessThreshold: 1,
		Path: "/healthz",
	}, logging.New(io.Discard, "error"), nil)
	hc.Start()
	t.Cleanup(hc.Stop)

	b := &store.Backend{ID: "b1", BaseURL: be.URL, Enabled: true, Models: []string{"m1"}, Weight: 1}
	s.UpsertBackend(b)
	// Multiple Add calls (e.g. AdminAPI create then Rescan) must be idempotent.
	hc.AddBackend(b)
	hc.AddBackend(b)
	hc.AddBackend(b)

	time.Sleep(120 * time.Millisecond)
	// With a single goroutine at 25ms we expect <=6 probes in 120ms;
	// with 3 goroutines we'd see ~15. Use a generous-but-discriminating bound.
	got := atomic.LoadInt64(&probes)
	if got > 9 {
		t.Errorf("expected probes from a single goroutine (<=9 in 120ms), got %d — duplicate loops?", got)
	}
}

// P1-2: deleting a backend must stop its goroutine so probes do not
// leak after admin removal.
func TestDeletedBackendStopsHealthLoop(t *testing.T) {
	var probes int64
	be := newProbeServer(&probes)
	defer be.Close()

	s := store.New("hmac")
	hc := NewHealthChecker(s, config.HealthCheckConfig{
		IntervalMS: 25, TimeoutMS: 200, FailureThreshold: 3, SuccessThreshold: 1,
		Path: "/healthz",
	}, logging.New(io.Discard, "error"), nil)
	hc.Start()
	t.Cleanup(hc.Stop)

	b := &store.Backend{ID: "b1", BaseURL: be.URL, Enabled: true, Models: []string{"m1"}, Weight: 1}
	s.UpsertBackend(b)
	hc.AddBackend(b)

	time.Sleep(75 * time.Millisecond)
	hc.RemoveBackend(b.ID)
	stopAt := atomic.LoadInt64(&probes)
	time.Sleep(150 * time.Millisecond)
	after := atomic.LoadInt64(&probes)
	// Allow at most one in-flight probe to land after RemoveBackend.
	if after-stopAt > 1 {
		t.Errorf("probes continued after RemoveBackend: before=%d after=%d", stopAt, after)
	}
}
