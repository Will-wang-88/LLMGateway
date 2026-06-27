package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const anchorUUIDValue = "550e8400-e29b-41d4-a716-446655440000"

// bigArrayEnvelope builds a request whose tool result is a large uniform array.
// Row errorAt carries an error keyword; row anchorAt carries the query anchor.
func bigArrayEnvelope(n, errorAt, anchorAt int, query string) []byte {
	rows := make([]any, n)
	for i := 0; i < n; i++ {
		msg := "status nominal operating normally"
		switch i {
		case errorAt:
			msg = "connection timeout error talking to upstream"
		case anchorAt:
			msg = "request id " + anchorUUIDValue + " processed"
		}
		rows[i] = map[string]any{"i": i, "host": "web-1", "msg": msg}
	}
	arr, _ := json.Marshal(rows)
	body := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": query},
			map[string]any{"role": "assistant", "content": "calling tool"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": string(arr)},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func lossyCfg() Config {
	c := testCfg()
	c.TokenBudget = 50 // force the lossy stage
	c.MaxItemsAfterCrush = 15
	return c
}

func keptRows(t *testing.T, body []byte) []any {
	t.Helper()
	content := toolContentOf(t, body)
	rows, ok := DecodeCSVSchema(content)
	if !ok {
		t.Fatalf("kept content is not CSV-schema:\n%s", content)
	}
	return rows
}

func rowHasMsgSubstr(rows []any, sub string) bool {
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m["msg"].(string); strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func rowHasI(rows []any, want int) bool {
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := m["i"].(json.Number); ok {
			if iv, err := n.Int64(); err == nil && int(iv) == want {
				return true
			}
		}
	}
	return false
}

func TestLossyRowDropFires(t *testing.T) {
	cfg := lossyCfg()
	in := bigArrayEnvelope(60, 30, 45, "what happened with "+anchorUUIDValue)
	out, stats := Compress(in, cfg)
	if !stats.Applied || !stats.Lossy {
		t.Fatalf("expected lossy applied; stats=%+v", stats)
	}
	if stats.RowsOffloaded == 0 || len(stats.Markers) == 0 {
		t.Fatalf("expected offloaded rows + markers; stats=%+v", stats)
	}
	rows := keptRows(t, out)
	if len(rows) > cfg.MaxItemsAfterCrush {
		t.Fatalf("kept %d rows, expected <= %d", len(rows), cfg.MaxItemsAfterCrush)
	}
}

func TestErrorRowsAlwaysSurvive(t *testing.T) {
	cfg := lossyCfg()
	// Place an error row in the middle so position anchors don't trivially keep it.
	in := bigArrayEnvelope(80, 40, 41, "lookup "+anchorUUIDValue)
	out, _ := Compress(in, cfg)
	rows := keptRows(t, out)
	if !rowHasMsgSubstr(rows, "error") {
		t.Fatalf("error row was dropped; kept=%v", rows)
	}
}

func TestQueryAnchorRowSurvives(t *testing.T) {
	cfg := lossyCfg()
	in := bigArrayEnvelope(80, 10, 55, "please investigate "+anchorUUIDValue)
	out, _ := Compress(in, cfg)
	rows := keptRows(t, out)
	if !rowHasMsgSubstr(rows, anchorUUIDValue) {
		t.Fatalf("query-anchor row was dropped; kept=%v", rows)
	}
}

func TestFirstAndLastKept(t *testing.T) {
	cfg := lossyCfg()
	in := bigArrayEnvelope(60, 30, 45, "q")
	out, _ := Compress(in, cfg)
	rows := keptRows(t, out)
	if !rowHasI(rows, 0) || !rowHasI(rows, 59) {
		t.Fatalf("first/last rows not kept; kept=%v", rows)
	}
}

func TestMarkerRetrievable(t *testing.T) {
	cfg := lossyCfg()
	in := bigArrayEnvelope(60, 30, 45, "q "+anchorUUIDValue)
	out, stats := Compress(in, cfg)
	store := NewMemoryRetrievalStore(0)
	for _, m := range stats.Markers {
		if h := store.Put(m.Content); h != m.Hash {
			t.Fatalf("store hash %s != marker hash %s", h, m.Hash)
		}
	}
	// The forwarded content must reference the marker hash.
	content := toolContentOf(t, out)
	for _, m := range stats.Markers {
		if !strings.Contains(content, m.Hash) {
			t.Fatalf("marker hash %s not present in forwarded content", m.Hash)
		}
		got, ok := store.Get(m.Hash)
		if !ok {
			t.Fatalf("hash %s not retrievable", m.Hash)
		}
		// Retrieved bytes must decode to the dropped rows.
		var dropped []any
		if err := json.Unmarshal(got, &dropped); err != nil {
			t.Fatalf("retrieved bytes not valid JSON: %v", err)
		}
		if len(dropped) != stats.RowsOffloaded {
			t.Fatalf("retrieved %d dropped rows, expected %d", len(dropped), stats.RowsOffloaded)
		}
	}
}

func TestLossyDeterministicAndStoreIndependent(t *testing.T) {
	cfg := lossyCfg()
	in := bigArrayEnvelope(70, 33, 50, "trace "+anchorUUIDValue)
	out1, s1 := Compress(in, cfg)
	out2, s2 := Compress(in, cfg)
	if string(out1) != string(out2) {
		t.Fatal("lossy output is non-deterministic")
	}
	if fmt.Sprint(markerHashes(s1)) != fmt.Sprint(markerHashes(s2)) {
		t.Fatal("marker hashes differ across runs")
	}
}

func markerHashes(s Stats) []string {
	out := make([]string, 0, len(s.Markers))
	for _, m := range s.Markers {
		out = append(out, m.Hash)
	}
	return out
}

func TestLosslessOnlyNoMarkers(t *testing.T) {
	cfg := lossyCfg()
	cfg.LosslessOnly = true
	in := bigArrayEnvelope(60, 30, 45, "q "+anchorUUIDValue)
	_, stats := Compress(in, cfg)
	if stats.Lossy || len(stats.Markers) > 0 || stats.RowsOffloaded > 0 {
		t.Fatalf("lossless_only must not drop rows or emit markers; stats=%+v", stats)
	}
}
