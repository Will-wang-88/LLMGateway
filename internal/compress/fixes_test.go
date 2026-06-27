package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

// Item E: a single-user-turn agent loop with many tool results must compress
// the older ones, protecting only the most recent N.
func TestSingleTurnProtectsRecentNToolResults(t *testing.T) {
	cfg := DefaultConfig() // ProtectRecentTurns = 2
	cfg.MinTokensToCrush = 0
	big := func(tag string) string {
		var rows []any
		for i := 0; i < 8; i++ {
			rows = append(rows, map[string]any{"i": i, "tag": tag})
		}
		b, _ := json.Marshal(rows)
		return string(b)
	}
	body, _ := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "one question, many tools"},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": big("a")},
			map[string]any{"role": "tool", "tool_call_id": "b", "content": big("b")},
			map[string]any{"role": "tool", "tool_call_id": "c", "content": big("c")},
			map[string]any{"role": "tool", "tool_call_id": "d", "content": big("d")},
		},
	})
	env, ok := parseEnvelope(body, cfg)
	if !ok {
		t.Fatal("parse failed")
	}
	// 4 tool results, protect last 2 -> exactly the first 2 are movable.
	if len(env.movables) != 2 {
		t.Fatalf("expected 2 movable (older) tool results, got %d", len(env.movables))
	}
	// And end-to-end it actually compresses.
	out, stats := Compress(body, cfg)
	if !stats.Applied || len(out) >= len(body) {
		t.Fatalf("expected compression of older tool results; applied=%v", stats.Applied)
	}
}

// Bug A: a json-typed (mixed) column must preserve string vs number/bool/null.
func TestJSONColumnPreservesStringType(t *testing.T) {
	cfg := testCfg()
	// Column "v" is mixed: object, numeric-looking string, bool-looking string,
	// "null" string, and a real number -> inferred type json.
	arr := `[{"v":{"a":1},"i":1},
 {"v":"42","i":2},
 {"v":"true","i":3},
 {"v":"null","i":4},
 {"v":123,"i":5}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	cols := buildColumns(rows, cfg)
	var vcol *column
	for i := range cols {
		if cols[i].name == "v" {
			vcol = &cols[i]
		}
	}
	if vcol == nil || vcol.typ != typeJSON {
		t.Fatalf("column v should be json-typed, got %+v", vcol)
	}
	encoded, ok := encodeTable(rows, cols, 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatalf("decode failed:\n%s", encoded)
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("json-column round-trip mismatch:\n got=%s\nwant=%s\nencoded:\n%s", got, want, encoded)
	}
}

// Bug B: an array whose object keys contain a literal '.' must pass through
// uncompacted (byte-identical), since dotted column names are ambiguous.
func TestDottedKeysPassThrough(t *testing.T) {
	cfg := testCfg()
	arr := `[{"user.id":1,"user.name":"a"},{"user.id":2,"user.name":"b"},{"user.id":3,"user.name":"c"},{"user.id":4,"user.name":"d"},{"user.id":5,"user.name":"e"}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	if cols := buildColumns(rows, cfg); len(cols) != 0 {
		t.Fatalf("expected buildColumns to bail on dotted keys, got %d cols", len(cols))
	}
	// End-to-end: the tool content must be forwarded byte-identical.
	in := openAIEnvelope(arr)
	out, stats := Compress(in, cfg)
	if stats.Applied || string(out) != string(in) {
		t.Fatalf("dotted-key array must pass through unchanged; applied=%v", stats.Applied)
	}
}

// Bug C: trailing data after a JSON array must not be silently dropped.
func TestTrailingDataNoop(t *testing.T) {
	cfg := testCfg()
	arr := `[{"a":1},{"a":2},{"a":3},{"a":4},{"a":5}] trailing-garbage`
	if _, ok := decodeArray(arr); ok {
		t.Fatal("decodeArray must reject trailing data")
	}
	in := openAIEnvelope(arr)
	out, stats := Compress(in, cfg)
	if stats.Applied || string(out) != string(in) {
		t.Fatal("content with trailing data must pass through unchanged")
	}
}

// A flattened nested column whose inner key contains '.' must not be flattened
// (it renders as a json cell) and must still round-trip.
func TestInnerDottedKeyNotFlattened(t *testing.T) {
	cfg := testCfg()
	arr := `[{"meta":{"a.b":1},"i":1},{"meta":{"a.b":2},"i":2},{"meta":{"a.b":3},"i":3},{"meta":{"a.b":4},"i":4},{"meta":{"a.b":5},"i":5}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	cols := buildColumns(rows, cfg)
	for _, c := range cols {
		if strings.Contains(c.name, "meta.") {
			t.Fatalf("meta should not be flattened (inner dotted key): col %q", c.name)
		}
	}
	encoded, ok := encodeTable(rows, cols, 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatal("decode failed")
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("inner-dotted round-trip mismatch:\n got=%s\nwant=%s", got, want)
	}
}
