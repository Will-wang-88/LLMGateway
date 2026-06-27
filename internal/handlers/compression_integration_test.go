package handlers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/will-wang-88/llmgateway/internal/compress"
	"github.com/will-wang-88/llmgateway/internal/config"
	"github.com/will-wang-88/llmgateway/internal/store"
)

func ptrBool(b bool) *bool { return &b }
func ptrInt(i int) *int    { return &i }

// TestInputCompressionEndToEnd verifies that, with per-model compression
// enabled, a large uniform tool-result array is rewritten to CSV-schema before
// the backend receives it, while the rest of the request and the response are
// unaffected.
func TestInputCompressionEndToEnd(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})

	// Register the model with compression enabled at a low threshold.
	s.UpsertModel(&store.Model{
		Name:          "llama-3.1-70b",
		Type:          "chat",
		Enabled:       true,
		ContextLength: 131072,
		Compression: &config.CompressionConfig{
			Enabled:        ptrBool(true),
			MinInputTokens: ptrInt(10),
		},
	})

	// Build a large uniform tool-result array.
	var rows []any
	for i := 0; i < 30; i++ {
		rows = append(rows, map[string]any{
			"ts": "2024-01-01T00:00:00Z", "host": "web-1", "cpu": i, "ok": true,
		})
	}
	arr, _ := json.Marshal(rows)

	// The big tool result is the OLDEST of three; the protected window is the
	// most recent 2 tool results, so this one is eligible for compression.
	body := map[string]any{
		"model": "llama-3.1-70b",
		"messages": []any{
			map[string]any{"role": "user", "content": "first question"},
			map[string]any{"role": "assistant", "content": "calling tool"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": string(arr)},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": "ok recent A"},
			map[string]any{"role": "tool", "tool_call_id": "c3", "content": "ok recent B"},
			map[string]any{"role": "user", "content": "another"},
		},
	}
	reqBody, _ := json.Marshal(body)

	resp := doRequest(t, h, reqBody)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	// The backend must have received a SMALLER body than the client sent.
	if len(be.lastBody) >= len(reqBody) {
		t.Fatalf("backend body not compressed: client=%d backend=%d", len(reqBody), len(be.lastBody))
	}

	// The tool message content must now be CSV-schema and decode back to the
	// original rows.
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(be.lastBody, &got); err != nil {
		t.Fatalf("backend body not valid JSON: %v", err)
	}
	if got.Model != "llama-3.1-70b" {
		t.Fatalf("model field altered: %q", got.Model)
	}
	var toolContents []string
	for _, m := range got.Messages {
		if m.Role == "tool" {
			var s string
			_ = json.Unmarshal(m.Content, &s)
			toolContents = append(toolContents, s)
		}
	}
	// The oldest (big) tool result is compacted; the recent two are untouched.
	var compacted string
	for _, s := range toolContents {
		if strings.HasPrefix(s, "[30]{") {
			compacted = s
		}
	}
	if compacted == "" {
		t.Fatalf("no CSV-schema compacted tool content found:\n%v", toolContents)
	}
	decoded, ok := compress.DecodeCSVSchema(compacted)
	if !ok || len(decoded) != 30 {
		t.Fatalf("compacted tool content does not decode to 30 rows (ok=%v len=%d)", ok, len(decoded))
	}
	recentUntouched := false
	for _, s := range toolContents {
		if s == "ok recent B" {
			recentUntouched = true
		}
	}
	if !recentUntouched {
		t.Fatalf("most recent tool result should be untouched; got %v", toolContents)
	}
	// Recent user prose must be untouched.
	lastUser := ""
	for _, m := range got.Messages {
		if m.Role == "user" {
			_ = json.Unmarshal(m.Content, &lastUser)
		}
	}
	if lastUser != "another" {
		t.Fatalf("recent user content altered: %q", lastUser)
	}
}

// TestInputCompressionDisabledByDefault verifies a model without a compression
// block forwards the body unchanged.
func TestInputCompressionDisabledByDefault(t *testing.T) {
	be := newCaptureBackend()
	defer be.Close()
	h, s := buildGateway(t, be.URL(), []string{"llama-3.1-70b"})
	s.UpsertModel(&store.Model{Name: "llama-3.1-70b", Type: "chat", Enabled: true})

	var rows []any
	for i := 0; i < 30; i++ {
		rows = append(rows, map[string]any{"a": i, "b": "x"})
	}
	arr, _ := json.Marshal(rows)
	body := map[string]any{
		"model": "llama-3.1-70b",
		"messages": []any{
			map[string]any{"role": "user", "content": "q1"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": string(arr)},
			map[string]any{"role": "user", "content": "q2"},
			map[string]any{"role": "user", "content": "q3"},
		},
	}
	reqBody, _ := json.Marshal(body)
	resp := doRequest(t, h, reqBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(be.lastBody) != len(reqBody) {
		t.Fatalf("compression must be off by default: client=%d backend=%d", len(reqBody), len(be.lastBody))
	}
}
