package compress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReviveStringifiedArray(t *testing.T) {
	cfg := testCfg()
	// A tool that JSON-encoded its array output, then it was embedded as a
	// string: content is a JSON string wrapping the array.
	inner := uniformArray
	encodedString, _ := json.Marshal(inner) // => "\"[{...}]\""
	candidate, revived := maybeReviveToArray(string(encodedString), cfg)
	if !revived {
		t.Fatal("expected revival of stringified array")
	}
	if _, ok := decodeArray(candidate); !ok {
		t.Fatalf("revived candidate is not an array: %q", candidate)
	}
}

func TestReviveEndToEnd(t *testing.T) {
	cfg := testCfg()
	stringified, _ := json.Marshal(uniformArray)
	in := openAIEnvelope(string(stringified))
	out, stats := Compress(in, cfg)
	if !stats.Applied {
		t.Fatalf("expected applied; stats=%+v", stats)
	}
	hasRevive := false
	for _, n := range stats.TransformsApplied {
		if n == "revive" {
			hasRevive = true
		}
	}
	if !hasRevive {
		t.Fatalf("expected revive transform; got %v", stats.TransformsApplied)
	}
	content := toolContentOf(t, out)
	decoded, ok := DecodeCSVSchema(content)
	if !ok {
		t.Fatalf("decode failed:\n%s", content)
	}
	if got, want := canonical(t, decoded), canonical(t, mustDecodeArray(t, uniformArray)); got != want {
		t.Fatalf("revive round-trip mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestRevivePlainArrayNotTouched(t *testing.T) {
	cfg := testCfg()
	_, revived := maybeReviveToArray(uniformArray, cfg)
	if revived {
		t.Fatal("a plain array must not be 'revived'")
	}
}

func TestDedupTextLines(t *testing.T) {
	cfg := testCfg()
	// Many exact-duplicate log lines; dedup should shrink it past the gate.
	var sb strings.Builder
	sb.WriteString("INFO startup complete\n")
	for i := 0; i < 40; i++ {
		sb.WriteString("WARN connection retry attempt to upstream service alpha\n")
	}
	sb.WriteString("INFO shutdown\n")
	out, applied := dedupTextLines(sb.String(), cfg)
	if !applied {
		t.Fatal("expected dedup to apply")
	}
	if strings.Count(out, "WARN connection retry") != 1 {
		t.Fatalf("expected single retained duplicate line, got:\n%s", out)
	}
	if !strings.Contains(out, "INFO startup complete") || !strings.Contains(out, "INFO shutdown") {
		t.Fatal("unique lines must be preserved")
	}
}

func TestDedupNoDuplicatesNoop(t *testing.T) {
	cfg := testCfg()
	in := "line one\nline two\nline three\n"
	out, applied := dedupTextLines(in, cfg)
	if applied || out != in {
		t.Fatal("text with no duplicates must be a no-op")
	}
}
