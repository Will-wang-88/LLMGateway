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
// It enforces strict passthrough: each "data: ..." line is forwarded byte-for-byte.
// It still parses chunks opportunistically to extract a usage block for metrics.
func (p *Proxy) streamResponse(w http.ResponseWriter, resp *http.Response, start time.Time, opts ForwardOptions) (int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		WriteError(w, http.StatusInternalServerError, InternalError("Streaming not supported by server", "streaming_unsupported"))
		return http.StatusInternalServerError, errors.New("response writer does not support flush")
	}
	clientCtx := resp.Request.Context()

	// Copy headers so SSE works.
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

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	firstChunkSeen := false
	var capturedUsage *Usage

	// Idle timer: any chunk forwarded resets it. Triggering it cancels the read.
	type readResult struct {
		line []byte
		err  error
	}
	chunkCh := make(chan readResult, 1)
	stopRead := make(chan struct{})
	go func() {
		defer close(chunkCh)
		for {
			select {
			case <-stopRead:
				return
			default:
			}
			line, err := br.ReadBytes('\n')
			if len(line) == 0 && err == nil {
				continue
			}
			chunkCh <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()
	defer close(stopRead)

	idleTimer := time.NewTimer(time.Duration(idleMS) * time.Millisecond)
	defer idleTimer.Stop()

	for {
		select {
		case <-clientCtx.Done():
			return resp.StatusCode, nil
		case <-idleTimer.C:
			// idle timeout - send an [DONE]-like terminator and close. We do not send a fake error
			// inside the SSE stream because that might confuse the client; instead we just stop.
			_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"stream idle timeout\",\"type\":\"backend_timeout\",\"code\":\"stream_idle_timeout\"}}\n\n")
			flusher.Flush()
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
				// Strict passthrough: write the line exactly.
				if _, err := w.Write(res.line); err != nil {
					return resp.StatusCode, err
				}
				flusher.Flush()
				// Reset idle timer.
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(time.Duration(idleMS) * time.Millisecond)

				// Try to extract usage from this chunk without altering it.
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

// helper used by handlers - not strictly streaming, but keeps SSE related helpers together.
func WriteSSEError(w http.ResponseWriter, err APIError) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	payload, _ := json.Marshal(errorEnvelope{Error: err})
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	flusher.Flush()
}
