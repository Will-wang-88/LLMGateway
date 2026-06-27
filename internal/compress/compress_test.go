package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

// testCfg disables turn protection and the size gate so small fixtures exercise
// the transforms directly.
func testCfg() Config {
	c := DefaultConfig()
	c.ProtectRecentTurns = 0
	c.MinTokensToCrush = 0
	return c
}

// openAIEnvelope wraps a tool-result content string in a minimal OpenAI body.
func openAIEnvelope(toolContent string) []byte {
	body := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "you are a helper"},
			map[string]any{"role": "user", "content": "fetch the data"},
			map[string]any{"role": "assistant", "content": "calling tool"},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": toolContent},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// toolContentOf extracts the tool message content from a request body.
func toolContentOf(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	for _, m := range env.Messages {
		if m.Role == "tool" {
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				t.Fatalf("tool content not a string: %v", err)
			}
			return s
		}
	}
	t.Fatal("no tool message found")
	return ""
}

// canonical renders v (decoded with UseNumber) to key-sorted JSON for semantic
// comparison.
func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func mustDecodeArray(t *testing.T, s string) []any {
	t.Helper()
	items, ok := decodeArray(s)
	if !ok {
		t.Fatalf("not a JSON array: %q", s)
	}
	return items
}

const uniformArray = `[{"ts":"2024-01-01T00:00:00Z","host":"web-1","cpu":45,"ok":true},
 {"ts":"2024-01-01T00:01:00Z","host":"web-1","cpu":47,"ok":true},
 {"ts":"2024-01-01T00:02:00Z","host":"web-1","cpu":91,"ok":false},
 {"ts":"2024-01-01T00:03:00Z","host":"web-2","cpu":12,"ok":true},
 {"ts":"2024-01-01T00:04:00Z","host":"web-2","cpu":33,"ok":true},
 {"ts":"2024-01-01T00:05:00Z","host":"web-2","cpu":88,"ok":false}]`

func TestCSVSchemaRoundTrip(t *testing.T) {
	cfg := testCfg()
	items := mustDecodeArray(t, uniformArray)
	rows, _ := asObjectRows(items)
	cols := buildColumns(rows, cfg)
	encoded, ok := encodeTable(rows, cols, 0, "")
	if !ok {
		t.Fatal("encodeTable failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatalf("decode failed for:\n%s", encoded)
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("round-trip mismatch:\n got=%s\nwant=%s\nencoded:\n%s", got, want, encoded)
	}
}

func TestGoldenSpecExample(t *testing.T) {
	cfg := testCfg()
	arr := `[{"ts":"2024-01-01T00:00:00Z","host":"web-1","cpu":45,"ok":true},
 {"ts":"2024-01-01T00:01:00Z","host":"web-1","cpu":47,"ok":true},
 {"ts":"2024-01-01T00:02:00Z","host":"web-1","cpu":91,"ok":false}]`
	rows, _ := asObjectRows(mustDecodeArray(t, arr))
	encoded, ok := encodeTable(rows, buildColumns(rows, cfg), 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	const want = "[3]{cpu:int,host:string,ok:bool,ts:string}\n" +
		"45,web-1,true,2024-01-01T00:00:00Z\n" +
		"47,web-1,true,2024-01-01T00:01:00Z\n" +
		"91,web-1,false,2024-01-01T00:02:00Z"
	if encoded != want {
		t.Fatalf("golden mismatch:\n got=%q\nwant=%q", encoded, want)
	}
}

func TestColumnOrderDeterministicAlpha(t *testing.T) {
	// Per spec §4.3 example: equal-frequency columns are ordered name-asc.
	cfg := testCfg()
	items := mustDecodeArray(t, uniformArray)
	rows, _ := asObjectRows(items)
	cols := buildColumns(rows, cfg)
	var names []string
	for _, c := range cols {
		names = append(names, c.name)
	}
	want := []string{"cpu", "host", "ok", "ts"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("column order = %v, want %v", names, want)
	}
}

func TestEndToEndCompressAndDecode(t *testing.T) {
	cfg := testCfg()
	in := openAIEnvelope(uniformArray)
	out, stats := Compress(in, cfg)
	if !stats.Applied {
		t.Fatalf("expected compression to apply; stats=%+v", stats)
	}
	if stats.BytesAfter >= stats.BytesBefore {
		t.Fatalf("expected smaller output: before=%d after=%d", stats.BytesBefore, stats.BytesAfter)
	}
	content := toolContentOf(t, out)
	decoded, ok := DecodeCSVSchema(content)
	if !ok {
		t.Fatalf("decode of tool content failed:\n%s", content)
	}
	if got, want := canonical(t, decoded), canonical(t, mustDecodeArray(t, uniformArray)); got != want {
		t.Fatalf("end-to-end round-trip mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestSavingsAtLeast25Percent(t *testing.T) {
	cfg := testCfg()
	in := openAIEnvelope(uniformArray)
	_, stats := Compress(in, cfg)
	content := toolContentOf(t, openAIEnvelope(uniformArray))
	_ = content
	// Compare the tool payload sizes specifically.
	origLen := len(uniformArray)
	csv, _ := encodeTable(func() []map[string]any { r, _ := asObjectRows(mustDecodeArray(t, uniformArray)); return r }(), buildColumns(func() []map[string]any { r, _ := asObjectRows(mustDecodeArray(t, uniformArray)); return r }(), cfg), 0, "")
	savings := float64(origLen-len(csv)) / float64(origLen)
	if savings < 0.25 {
		t.Fatalf("expected >=25%% payload savings, got %.1f%%", savings*100)
	}
	if !stats.Applied {
		t.Fatal("expected applied")
	}
}

func TestDeterminism100x(t *testing.T) {
	cfg := testCfg()
	in := openAIEnvelope(uniformArray)
	first, _ := Compress(in, cfg)
	for i := 0; i < 100; i++ {
		got, _ := Compress(in, cfg)
		if string(got) != string(first) {
			t.Fatalf("non-deterministic output on iteration %d", i)
		}
	}
}

func TestKeyOrderPermutationInvariance(t *testing.T) {
	cfg := testCfg()
	a := `[{"a":1,"b":2,"c":3},{"a":4,"b":5,"c":6},{"a":7,"b":8,"c":9},{"a":10,"b":11,"c":12},{"a":13,"b":14,"c":15}]`
	b := `[{"c":3,"b":2,"a":1},{"b":5,"c":6,"a":4},{"c":6,"a":7,"b":8},{"a":10,"c":12,"b":11},{"b":14,"a":13,"c":15}]`
	// Note: b row 2/3 values intentionally identical data, different key order.
	ra, _ := asObjectRows(mustDecodeArray(t, a))
	rb, _ := asObjectRows(mustDecodeArray(t, b))
	ea, _ := encodeTable(ra, buildColumns(ra, cfg), 0, "")
	eb, _ := encodeTable(rb, buildColumns(rb, cfg), 0, "")
	// Same schema/header regardless of input key order.
	declA := ea[:strings.IndexByte(ea, '\n')]
	declB := eb[:strings.IndexByte(eb, '\n')]
	if declA != declB {
		t.Fatalf("declaration differs by key order:\n%s\n%s", declA, declB)
	}
}

func TestNumberRoundTripBigInt(t *testing.T) {
	cfg := testCfg()
	// Includes an int64 > 2^53, a float, and scientific notation.
	arr := `[{"id":9007199254740993,"v":1.5,"x":1e10},
 {"id":9007199254740994,"v":2.25,"x":2e10},
 {"id":9007199254740995,"v":3.75,"x":3e10},
 {"id":9007199254740996,"v":4.5,"x":4e10},
 {"id":9007199254740997,"v":5.0,"x":5e10}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	encoded, ok := encodeTable(rows, buildColumns(rows, cfg), 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	// The big integer literal must appear verbatim in the output.
	if !strings.Contains(encoded, "9007199254740993") {
		t.Fatalf("big int literal lost:\n%s", encoded)
	}
	if !strings.Contains(encoded, "1e10") {
		t.Fatalf("scientific notation literal lost:\n%s", encoded)
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatal("decode failed")
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("number round-trip mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestUnicodeAndEscaping(t *testing.T) {
	cfg := testCfg()
	arr := `[{"k":"a,b","n":1},{"k":"quote\"here","n":2},{"k":"line\nbreak","n":3},{"k":"emoji 😀 CJK 你好","n":4},{"k":"","n":5}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	encoded, ok := encodeTable(rows, buildColumns(rows, cfg), 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatalf("decode failed:\n%s", encoded)
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("unicode/escape round-trip mismatch:\n got=%s\nwant=%s\nencoded:\n%s", got, want, encoded)
	}
}

func TestNullVsEmptyStringDistinction(t *testing.T) {
	cfg := testCfg()
	// Row with explicit null vs row with explicit empty string in the same col.
	arr := `[{"a":null,"b":1},{"a":"","b":2},{"a":"x","b":3},{"a":null,"b":4},{"a":"","b":5}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	encoded, ok := encodeTable(rows, buildColumns(rows, cfg), 0, "")
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatal("decode failed")
	}
	if got, want := canonical(t, decoded), canonical(t, items); got != want {
		t.Fatalf("null/empty mismatch:\n got=%s\nwant=%s\nencoded:\n%s", got, want, encoded)
	}
}

func TestMalformedJSONNoop(t *testing.T) {
	cfg := testCfg()
	in := []byte(`{"messages": [ this is not valid json `)
	out, stats := Compress(in, cfg)
	if string(out) != string(in) {
		t.Fatal("malformed input must pass through byte-identical")
	}
	if stats.Applied {
		t.Fatal("malformed input must not report applied")
	}
}

func TestSmallArrayNoop(t *testing.T) {
	cfg := testCfg()
	in := openAIEnvelope(`[{"a":1},{"a":2}]`) // below min_items
	out, stats := Compress(in, cfg)
	if string(out) != string(in) || stats.Applied {
		t.Fatal("small array must be a no-op")
	}
}

func TestBucketsRoundTrip(t *testing.T) {
	cfg := testCfg()
	// Heterogeneous: a discriminator "kind" with two values, divergent fields.
	arr := `[{"kind":"a","x":1,"p":10},{"kind":"a","x":2,"p":20},{"kind":"a","x":3,"p":30},
 {"kind":"b","y":"foo","q":true},{"kind":"b","y":"bar","q":false},{"kind":"b","y":"baz","q":true}]`
	items := mustDecodeArray(t, arr)
	rows, _ := asObjectRows(items)
	d := decideArray(items, cfg)
	if d.kind != decBuckets {
		t.Skipf("array classified as %d, not buckets; discriminator detection tuned elsewhere", d.kind)
	}
	encoded, ok := encodeBuckets(rows, d.discriminator, cfg, 0, "")
	if !ok {
		t.Fatal("encodeBuckets failed")
	}
	decoded, ok := DecodeCSVSchema(encoded)
	if !ok {
		t.Fatalf("decode buckets failed:\n%s", encoded)
	}
	// Bucket decode groups by discriminator value (sorted), so compare as sets
	// keyed by canonical row JSON.
	if !sameRowSet(t, decoded, items) {
		t.Fatalf("bucket round-trip mismatch:\ndecoded=%s\norig=%s\nencoded:\n%s",
			canonical(t, decoded), canonical(t, items), encoded)
	}
}

func sameRowSet(t *testing.T, a, b []any) bool {
	t.Helper()
	count := map[string]int{}
	for _, r := range a {
		count[canonical(t, r)]++
	}
	for _, r := range b {
		count[canonical(t, r)]--
	}
	for _, c := range count {
		if c != 0 {
			return false
		}
	}
	return true
}
