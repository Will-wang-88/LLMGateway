package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// buildProse makes a multi-line log: many near-duplicate noise lines, one error
// line, one line containing the query anchor, plus distinct head/tail lines.
func buildProse(anchor string) string {
	var b strings.Builder
	b.WriteString("=== service log start ===\n")
	for i := 0; i < 40; i++ {
		// near-duplicates: same shingle, tiny variation
		fmt.Fprintf(&b, "heartbeat ok worker pool stable iteration %d nominal\n", i%3)
	}
	b.WriteString("connection timeout error contacting upstream payment service\n")
	fmt.Fprintf(&b, "request trace id %s completed successfully\n", anchor)
	for i := 0; i < 20; i++ {
		b.WriteString("cache warm steady state no anomalies detected here\n")
	}
	b.WriteString("=== service log end ===\n")
	return b.String()
}

func proseCfg() Config {
	c := testCfg()
	c.TokenBudget = 10 // force lossy
	return c
}

func TestProseLossyKeepsErrorAndAnchor(t *testing.T) {
	cfg := proseCfg()
	anchor := "abc12345"
	body := openAIEnvelopeWithQuery(buildProse(anchor), "what happened to request "+anchor)
	out, stats := Compress(body, cfg)
	if !stats.Applied || !stats.Lossy {
		t.Fatalf("expected lossy prose compression; stats=%+v", stats)
	}
	content := toolContentOf(t, out)
	if !strings.Contains(content, "error contacting upstream") {
		t.Fatalf("error line must be kept:\n%s", content)
	}
	if !strings.Contains(content, anchor) {
		t.Fatalf("query-anchor line must be kept:\n%s", content)
	}
	if !strings.Contains(content, "<<ccr:") {
		t.Fatalf("expected a retrieval marker:\n%s", content)
	}
	if len(stats.Markers) == 0 || stats.Markers[0].Kind != "text" {
		t.Fatalf("expected a text marker; got %+v", stats.Markers)
	}
}

func TestProseLossyMarkerRetrievable(t *testing.T) {
	cfg := proseCfg()
	original := buildProse("xyz98765")
	body := openAIEnvelopeWithQuery(original, "status")
	_, stats := Compress(body, cfg)
	if len(stats.Markers) == 0 {
		t.Fatal("expected a marker")
	}
	store := NewMemoryRetrievalStore(0)
	h := store.Put(stats.Markers[0].Content)
	if h != stats.Markers[0].Hash {
		t.Fatal("hash mismatch")
	}
	got, ok := store.Get(stats.Markers[0].Hash)
	if !ok || string(got) != original {
		t.Fatal("original prose not retrievable byte-exact")
	}
}

func TestProseLossyDeterministic(t *testing.T) {
	cfg := proseCfg()
	body := openAIEnvelopeWithQuery(buildProse("id7777"), "trace id7777")
	out1, _ := Compress(body, cfg)
	out2, _ := Compress(body, cfg)
	if string(out1) != string(out2) {
		t.Fatal("prose lossy output must be deterministic")
	}
}

func TestProseSkipsJSON(t *testing.T) {
	cfg := proseCfg()
	// A pretty-printed JSON object (valid JSON) must never be line-edited.
	obj := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3,\n  \"d\": 4,\n  \"e\": 5,\n  \"f\": 6\n}"
	if _, _, ok := proseLossy(obj, "", cfg); ok {
		t.Fatal("proseLossy must refuse valid JSON")
	}
}

func TestProseUnderBudgetUntouched(t *testing.T) {
	// When the request is within budget, the lossy prose path must not run; only
	// exact-duplicate removal (lossless) may apply.
	cfg := testCfg()
	cfg.TokenBudget = 0 // lossy disabled
	body := openAIEnvelopeWithQuery(buildProse("id"), "q")
	_, stats := Compress(body, cfg)
	if stats.Lossy {
		t.Fatal("lossy prose must not run when budget disables the lossy stage")
	}
}

// openAIEnvelopeWithQuery wraps tool content plus a user query, with extra tool
// results so the target tool result is outside the protected recent-N window.
func openAIEnvelopeWithQuery(toolContent, query string) []byte {
	body := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": query},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": toolContent},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": "recent A"},
			map[string]any{"role": "tool", "tool_call_id": "c3", "content": "recent B"},
		},
	}
	b, _ := json.Marshal(body)
	return b
}
