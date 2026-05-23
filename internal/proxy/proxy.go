package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/will-wang-88/llmgateway/internal/logging"
	"github.com/will-wang-88/llmgateway/internal/metrics"
	"github.com/will-wang-88/llmgateway/internal/store"
)

// Proxy forwards arbitrary OpenAI-compatible payloads to a backend.
// Strict passthrough semantics:
//   - request body is forwarded byte-for-byte (only model may be rewritten
//     by the caller before invocation; this proxy never inspects unknown fields).
//   - response body is forwarded byte-for-byte for non-streaming.
//   - streaming responses are flushed chunk-by-chunk without parsing.
type Proxy struct {
	client            *http.Client
	streamingClient   *http.Client
	logger            *logging.Logger
	metrics           *metrics.Metrics
}

func New(logger *logging.Logger, m *metrics.Metrics) *Proxy {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// DisableCompression is important - gzip would buffer the stream.
		DisableCompression:    true,
		// Detect upstream that accepts the TCP connection but never returns
		// response headers. Without this a hung backend wedges the request
		// until the client disconnects.
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &Proxy{
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // request-level timeout is enforced via context
		},
		streamingClient: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
		logger:  logger,
		metrics: m,
	}
}

// ForwardOptions describes how to forward a single request.
type ForwardOptions struct {
	Method        string
	Backend       *store.Backend
	UpstreamPath  string // e.g. "/chat/completions"
	Body          []byte // raw body to forward (already rewritten if needed)
	IsStream      bool
	StreamIdleTimeoutMS int
	TimeoutMS     int

	// Labels for metrics/logging.
	Model        string
	ForwardModel string // model name actually sent to backend
	APIKeyLabel  string
	Endpoint     string

	// ContentType override (defaults to application/json). When set the
	// upstream Content-Type and Accept handling is bypassed so multipart
	// (audio/transcriptions) requests can be relayed unchanged.
	ContentType string

	// Optional callbacks.
	OnTTFT        func(d time.Duration)
	OnUsage       func(usage *Usage)       // called for non-streaming once response is parsed
	OnStreamUsage func(usage *Usage)       // called for streaming if usage block found in final chunk
	OnRawResponse func(body []byte)        // called for non-streaming, raw bytes
	OnStreamChunk func(line []byte)        // called once per streamed chunk
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens,omitempty"`
}

// Forward proxies the request. It writes the upstream response (status, headers, body)
// to w. Returns a tuple of (statusCode, error).
func (p *Proxy) Forward(ctx context.Context, w http.ResponseWriter, r *http.Request, opts ForwardOptions) (int, error) {
	upstreamURL := opts.Backend.BaseURL + opts.UpstreamPath
	timeoutMS := opts.TimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = opts.Backend.TimeoutMS
	}
	if timeoutMS <= 0 {
		timeoutMS = 120000
	}

	// Non-streaming requests can use a hard ctx timeout because the full response is bounded.
	// Streaming requests should not use a hard deadline; we rely on idle-timeout detection instead.
	var reqCtx context.Context
	var cancel context.CancelFunc
	if opts.IsStream {
		reqCtx, cancel = context.WithCancel(ctx)
	} else {
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, opts.Method, upstreamURL, bytes.NewReader(opts.Body))
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("build upstream request: %w", err)
	}
	// Copy a sane subset of request headers. We deliberately drop Authorization and
	// substitute the backend's own credentials so client API keys never leak upstream.
	copyForwardHeaders(req.Header, r.Header)
	if opts.Backend.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Backend.APIKey)
	} else {
		req.Header.Del("Authorization")
	}
	if opts.ContentType != "" {
		req.Header.Set("Content-Type", opts.ContentType)
	} else if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept-Encoding", "identity")
	if opts.IsStream {
		req.Header.Set("Accept", "text/event-stream")
	} else if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	client := p.client
	if opts.IsStream {
		client = p.streamingClient
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			WriteError(w, http.StatusGatewayTimeout, APIError{
				Message: "Backend request timed out",
				Type:    "backend_timeout",
				Code:    "backend_timeout",
			})
			return http.StatusGatewayTimeout, err
		}
		if errors.Is(err, context.Canceled) {
			return 499, err
		}
		WriteError(w, http.StatusBadGateway, APIError{
			Message: "Failed to reach backend: " + err.Error(),
			Type:    "backend_error",
			Code:    "backend_unreachable",
		})
		return http.StatusBadGateway, err
	}
	defer resp.Body.Close()

	if opts.IsStream && isEventStream(resp) {
		return p.streamResponse(w, resp, start, opts)
	}
	return p.passResponse(w, resp, start, opts)
}

func isEventStream(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream")
}

func (p *Proxy) passResponse(w http.ResponseWriter, resp *http.Response, start time.Time, opts ForwardOptions) (int, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		WriteError(w, http.StatusBadGateway, APIError{
			Message: "Failed to read backend response: " + err.Error(),
			Type:    "backend_error",
			Code:    "backend_read_failed",
		})
		return http.StatusBadGateway, err
	}

	// Copy response headers (except hop-by-hop). Backend response body is forwarded as-is.
	copyResponseHeaders(w.Header(), resp.Header)
	// Force a fresh content-length in case upstream sent chunked.
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	// Try to parse usage block for metrics, without mutating body.
	if opts.OnUsage != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if usage, ok := extractUsage(body); ok {
			opts.OnUsage(usage)
		}
	}
	if opts.OnRawResponse != nil {
		opts.OnRawResponse(body)
	}
	_ = time.Since(start)
	return resp.StatusCode, nil
}

func extractUsage(body []byte) (*Usage, bool) {
	var envelope struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false
	}
	if envelope.Usage == nil {
		return nil, false
	}
	return envelope.Usage, true
}

// copyForwardHeaders copies headers from inbound client request to upstream backend request.
// It drops hop-by-hop headers and the client's Authorization header.
var hopByHop = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"host":                {},
	"content-length":      {},
}

var dropForward = map[string]struct{}{
	"authorization": {},
	"x-api-key":     {},
	"cookie":        {},
}

func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if _, hop := hopByHop[lk]; hop {
			continue
		}
		if _, drop := dropForward[lk]; drop {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if _, hop := hopByHop[lk]; hop {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
