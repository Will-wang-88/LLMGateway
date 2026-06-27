package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

func bigBase64(n int) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alpha[i%len(alpha)])
	}
	return b.String()
}

func TestClassifyOpaque(t *testing.T) {
	cfg := DefaultConfig() // OpaqueMinBytes = 256
	if _, ok := classifyOpaque("short", cfg); ok {
		t.Fatal("short string must not be opaque")
	}
	if k, ok := classifyOpaque(bigBase64(400), cfg); !ok || k != "base64" {
		t.Fatalf("expected base64, got kind=%q ok=%v", k, ok)
	}
	html := "<html><body>" + strings.Repeat("<div>x</div>", 40) + "</body></html>"
	if k, ok := classifyOpaque(html, cfg); !ok || k != "html" {
		t.Fatalf("expected html, got kind=%q ok=%v", k, ok)
	}
	// Natural prose (plenty of spaces) must NOT be classified opaque.
	prose := strings.Repeat("the quick brown fox jumps over the lazy dog ", 20)
	if _, ok := classifyOpaque(prose, cfg); ok {
		t.Fatal("prose with spaces must not be opaque")
	}
}

func TestWholeContentOpaqueMarker(t *testing.T) {
	cfg := testCfg()
	cfg.TokenBudget = 10 // force lossy stage
	blob := bigBase64(2000)
	in := openAIEnvelope(blob)
	out, stats := Compress(in, cfg)
	if !stats.Applied || !stats.Lossy {
		t.Fatalf("expected lossy opaque offload; stats=%+v", stats)
	}
	if len(stats.Markers) != 1 || stats.Markers[0].Kind != "base64" {
		t.Fatalf("expected one base64 marker; got %+v", stats.Markers)
	}
	content := toolContentOf(t, out)
	if !strings.HasPrefix(content, "<<ccr:") {
		t.Fatalf("content should be an opaque marker, got: %.40s", content)
	}
	// Retrievable.
	store := NewMemoryRetrievalStore(0)
	h := store.Put(stats.Markers[0].Content)
	if h != stats.Markers[0].Hash {
		t.Fatalf("store hash mismatch")
	}
	got, ok := store.Get(stats.Markers[0].Hash)
	if !ok || string(got) != blob {
		t.Fatal("opaque blob not retrievable byte-exact")
	}
}

func TestInArrayOpaqueMarker(t *testing.T) {
	cfg := testCfg()
	cfg.TokenBudget = 10 // force lossy stage
	// 6 rows (<= MaxItemsAfterCrush 15, so no row-drop), each with a big base64 cell.
	var rows []any
	for i := 0; i < 6; i++ {
		rows = append(rows, map[string]any{"i": i, "blob": bigBase64(600)})
	}
	arr, _ := json.Marshal(rows)
	in := openAIEnvelope(string(arr))
	out, stats := Compress(in, cfg)
	if !stats.Applied {
		t.Fatalf("expected applied; stats=%+v", stats)
	}
	if len(stats.Markers) == 0 {
		t.Fatal("expected opaque markers for in-array blobs")
	}
	content := toolContentOf(t, out)
	if !strings.Contains(content, "<<ccr:") {
		t.Fatalf("expected opaque markers in content:\n%.200s", content)
	}
	// Every emitted marker must be retrievable.
	store := NewMemoryRetrievalStore(0)
	for _, m := range stats.Markers {
		store.Put(m.Content)
		if _, ok := store.Get(m.Hash); !ok {
			t.Fatalf("marker %s not retrievable", m.Hash)
		}
	}
}

func TestOpaqueDeterministicHash(t *testing.T) {
	cfg := testCfg()
	cfg.TokenBudget = 10
	in := openAIEnvelope(bigBase64(1500))
	_, s1 := Compress(in, cfg)
	_, s2 := Compress(in, cfg)
	if len(s1.Markers) != 1 || len(s2.Markers) != 1 || s1.Markers[0].Hash != s2.Markers[0].Hash {
		t.Fatal("opaque marker hash must be deterministic")
	}
}
