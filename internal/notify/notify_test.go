package notify

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/store"
)

type fakeSender struct {
	mu       sync.Mutex
	calls    []call
	failNext atomic.Bool
}

type call struct{ Subject, Body string }

func (f *fakeSender) Send(subject, body string, _ []string) error {
	if f.failNext.Swap(false) {
		return errors.New("smtp failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Subject: subject, Body: body})
	return nil
}

func (f *fakeSender) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func makeNotifier(t *testing.T, cooldownMS int) (*Notifier, *fakeSender) {
	t.Helper()
	cfg := config.NotificationsConfig{Email: config.EmailNotifierConfig{
		Enabled: true, To: []string{"ops@example.com"}, CooldownMS: cooldownMS,
	}}
	n := New(cfg, logging.New(io.Discard, "error"))
	f := &fakeSender{}
	n.SetSender(f)
	n.Start()
	t.Cleanup(n.Stop)
	return n, f
}

func TestBackendUnhealthySendsEmailNotification(t *testing.T) {
	n, f := makeNotifier(t, 0)
	n.Notify(Event{
		BackendID: "b1", BackendName: "vllm-1", Prev: store.StatusHealthy, Next: store.StatusUnhealthy,
		Error: "connection refused", At: time.Now(),
	})
	waitFor(t, func() bool { return len(f.snapshot()) == 1 })
	if !strings.Contains(f.snapshot()[0].Subject, "backend_unhealthy") {
		t.Errorf("subject missing kind: %q", f.snapshot()[0].Subject)
	}
	if !strings.Contains(f.snapshot()[0].Body, "connection refused") {
		t.Errorf("body missing reason: %q", f.snapshot()[0].Body)
	}
}

func TestBackendNotificationCooldown(t *testing.T) {
	n, f := makeNotifier(t, 5_000)
	evt := Event{
		BackendID: "b1", Prev: store.StatusHealthy, Next: store.StatusUnhealthy,
		At: time.Now(),
	}
	n.Notify(evt)
	n.Notify(evt)
	n.Notify(evt)
	waitFor(t, func() bool { return len(f.snapshot()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := len(f.snapshot()); got != 1 {
		t.Errorf("cooldown should suppress dupes, got %d sends", got)
	}
}

func TestBackendRecoverySendsEmailNotification(t *testing.T) {
	n, f := makeNotifier(t, 0)
	n.Notify(Event{
		BackendID: "b1", Prev: store.StatusUnhealthy, Next: store.StatusHealthy, At: time.Now(),
	})
	waitFor(t, func() bool { return len(f.snapshot()) == 1 })
	if !strings.Contains(f.snapshot()[0].Subject, "backend_recovered") {
		t.Errorf("expected recovered kind, got %q", f.snapshot()[0].Subject)
	}
}

func TestNotifyFilterByNotifyOn(t *testing.T) {
	cfg := config.NotificationsConfig{Email: config.EmailNotifierConfig{
		Enabled: true, To: []string{"x@y"}, NotifyOn: []string{"backend_unhealthy"},
	}}
	n := New(cfg, logging.New(io.Discard, "error"))
	f := &fakeSender{}
	n.SetSender(f)
	n.Start()
	defer n.Stop()
	n.Notify(Event{BackendID: "b1", Prev: store.StatusHealthy, Next: store.StatusDegraded})
	time.Sleep(50 * time.Millisecond)
	if got := len(f.snapshot()); got != 0 {
		t.Errorf("backend_degraded should be filtered out, got %d", got)
	}
	n.Notify(Event{BackendID: "b1", Prev: store.StatusHealthy, Next: store.StatusUnhealthy})
	waitFor(t, func() bool { return len(f.snapshot()) == 1 })
}

func TestNotifyFailureDoesNotPanicAndIsLogged(t *testing.T) {
	n, f := makeNotifier(t, 0)
	f.failNext.Store(true)
	n.Notify(Event{BackendID: "b1", Prev: store.StatusHealthy, Next: store.StatusUnhealthy})
	time.Sleep(50 * time.Millisecond)
	_, lastErr := n.LastResult()
	if lastErr == "" {
		t.Errorf("expected last error to be recorded")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition")
}
