package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// flushingRecorder is an httptest.ResponseRecorder that also implements
// http.Flusher so the proxy's streaming path is exercised.
type flushingRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushingRecorder) Flush() {}

// TestStreamIdleTimeoutDoesNotInjectSSEBytes verifies that when a
// backend stalls and the idle timer fires, the gateway closes the
// stream without writing any extra bytes (e.g. a `: stream-idle-timeout`
// SSE comment). The output to the client must be exactly the bytes
// produced by the backend.
func TestStreamIdleTimeoutDoesNotInjectSSEBytes(t *testing.T) {
	backendChunk := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, backendChunk)
		flusher.Flush()
		// Hold the connection open well past the idle timeout so the
		// gateway triggers the idle branch.
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	logger := logging.New(io.Discard, "error")
	m := metrics.New()
	p := New(logger, m)

	be := &store.Backend{ID: "b1", BaseURL: srv.URL, Enabled: true, Weight: 1, TimeoutMS: 5000, StreamIdleTimeoutMS: 50}
	be.SetStatus(store.StatusHealthy)

	rec := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(context.Background())

	opts := ForwardOptions{
		Method:              "POST",
		Backend:             be,
		UpstreamPath:        "/chat/completions",
		Body:                []byte(`{}`),
		IsStream:            true,
		StreamIdleTimeoutMS: 50,
	}
	_, _ = p.Forward(req.Context(), rec, req, opts)

	got := rec.Body.String()
	if !strings.Contains(got, "delta") {
		t.Fatalf("expected backend chunk in response, got %q", got)
	}
	if strings.Contains(got, "stream-idle-timeout") {
		t.Errorf("gateway must not inject SSE comment on idle timeout, got %q", got)
	}
	// Stripping the backend chunk should leave nothing the gateway
	// itself wrote (no error frame, no comment).
	residue := strings.TrimSpace(strings.Replace(got, backendChunk, "", 1))
	if residue != "" {
		t.Errorf("expected zero gateway-injected bytes, got %q", residue)
	}
}
