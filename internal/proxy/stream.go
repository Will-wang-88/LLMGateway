package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamResponse forwards an upstream SSE stream to the client.
//
// Strict passthrough semantics:
//   - bytes from the backend are written to the client unmodified.
//   - lines larger than the read buffer are reassembled before being parsed
//     for usage (still forwarded byte-for-byte to the client).
//   - on idle timeout, the stream is closed silently. Nothing is written
//     to the client beyond what the backend already produced; the spec
//     requires zero gateway-injected bytes in the stream.
//
// Goroutine lifecycle: the read goroutine sends to a buffered channel and
// also selects on stopRead. When streamResponse returns, defer closes
// stopRead and resp.Body (via the caller's defer), unblocking the producer.
func (p *Proxy) streamResponse(w http.ResponseWriter, resp *http.Response, start time.Time, opts ForwardOptions) (int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, InternalError("Streaming not supported by server", "streaming_unsupported"))
		return http.StatusInternalServerError, errors.New("response writer does not support flush")
	}
	clientCtx := resp.Request.Context()

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	idleMS := opts.StreamIdleTimeoutMS
	if idleMS <= 0 && opts.Backend != nil {
		idleMS = opts.Backend.StreamIdleTimeoutMS
	}
	if idleMS <= 0 {
		idleMS = 30000
	}

	br := bufio.NewReaderSize(resp.Body, 256*1024)
	firstChunkSeen := false
	var capturedUsage *Usage

	type readResult struct {
		line []byte
		err  error
	}
	chunkCh := make(chan readResult, 4)
	stopRead := make(chan struct{})

	// Producer goroutine. Uses readLine to reassemble lines larger than the
	// bufio buffer. Selects on stopRead when sending so a dead consumer doesn't
	// wedge it. Also closes resp.Body if the consumer signals stop, so an
	// in-flight ReadBytes returns and the goroutine can exit.
	go func() {
		defer close(chunkCh)
		for {
			line, err := readLine(br)
			if len(line) == 0 && err == nil {
				continue
			}
			select {
			case chunkCh <- readResult{line: line, err: err}:
			case <-stopRead:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(stopRead)
		// Drain any pending chunks so the producer doesn't park on send.
		go func() {
			for range chunkCh {
			}
		}()
	}()

	idleTimer := time.NewTimer(time.Duration(idleMS) * time.Millisecond)
	defer idleTimer.Stop()

	for {
		select {
		case <-clientCtx.Done():
			return resp.StatusCode, nil
		case <-idleTimer.C:
			// Per spec the gateway must not inject content into the
			// stream. Close upstream and return; the client sees EOF.
			p.logger.Warn("stream idle timeout", map[string]any{
				"backend": backendIDOf(opts),
			})
			return resp.StatusCode, errors.New("stream idle timeout")
		case res, ok := <-chunkCh:
			if !ok {
				return resp.StatusCode, nil
			}
			if len(res.line) > 0 {
				if !firstChunkSeen && hasContent(res.line) {
					firstChunkSeen = true
					if opts.OnTTFT != nil {
						opts.OnTTFT(time.Since(start))
					}
				}
				if _, err := w.Write(res.line); err != nil {
					return resp.StatusCode, err
				}
				flusher.Flush()
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(time.Duration(idleMS) * time.Millisecond)

				if opts.OnStreamUsage != nil && capturedUsage == nil {
					if u := parseChunkUsage(res.line); u != nil {
						capturedUsage = u
						opts.OnStreamUsage(u)
					}
				}
				if opts.OnStreamChunk != nil {
					opts.OnStreamChunk(res.line)
				}
			}
			if res.err != nil {
				if !errors.Is(res.err, io.EOF) {
					backendID := ""
					if opts.Backend != nil {
						backendID = opts.Backend.ID
					}
					p.logger.Warn("upstream stream read error", map[string]any{
						"error":   res.err.Error(),
						"backend": backendID,
					})
				}
				return resp.StatusCode, nil
			}
		}
	}
}

// readLine reads until '\n' or EOF, reassembling buffer-full continuations so
// long SSE lines are returned intact. The returned slice ends in '\n' for a
// complete line, otherwise it ends at the partial read.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			// chunk is owned by bufio; copy if we need to combine multiple.
			if out == nil && (err == nil || err == bufio.ErrBufferFull) {
				// First read - we may still need to grow. Copy to detach from buffer.
				out = append([]byte(nil), chunk...)
			} else if out != nil {
				out = append(out, chunk...)
			} else {
				// Single complete line (no further reads needed) and we're returning.
				out = append([]byte(nil), chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return out, err
	}
}

func hasContent(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	// Heartbeat or comment lines (starting with ":") should not count as TTFT.
	if bytes.HasPrefix(trimmed, []byte(":")) {
		return false
	}
	return true
}

func parseChunkUsage(line []byte) *Usage {
	s := string(bytes.TrimSpace(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var envelope struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return nil
	}
	return envelope.Usage
}

func backendIDOf(opts ForwardOptions) string {
	if opts.Backend != nil {
		return opts.Backend.ID
	}
	return ""
}

// WriteSSEError emits an OpenAI-style error frame as a single SSE event.
// Used by handlers that need to surface an error AFTER the SSE headers were
// already flushed (e.g. routing failure detected post-header). Not used for
// idle-timeout (spec: no injection into the stream).
func WriteSSEError(w http.ResponseWriter, err APIError) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	payload, _ := json.Marshal(errorEnvelope{Error: err})
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	flusher.Flush()
}
