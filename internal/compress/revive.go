package compress

import (
	"encoding/json"
	"strings"
)

// maybeReviveToArray handles spec §3d stringified-JSON revival for the common,
// clearly-beneficial case: a tool result whose content is a JSON-encoded string
// (one or more layers) that wraps a JSON array. Unwrapping removes a layer of
// escaping and exposes the array for tabularization. It is lossless: no
// information is lost, only redundant escaping.
//
// It returns (candidate, revived). If content is already a JSON array, or no
// unwrapping exposes an array, it returns (content, false).
func maybeReviveToArray(content string, cfg Config) (string, bool) {
	if _, ok := decodeArray(content); ok {
		return content, false // already an array; nothing to revive
	}
	unwrapped, did := reviveStringified(content, cfg.MaxDepth)
	if did {
		if _, ok := decodeArray(unwrapped); ok {
			return unwrapped, true
		}
	}
	return content, false
}

// reviveStringified peels JSON string-encoding layers off s while each decoded
// layer still looks like JSON (starts with '[' or '{'), up to maxDepth layers.
func reviveStringified(s string, maxDepth int) (string, bool) {
	if maxDepth <= 0 {
		maxDepth = 1
	}
	cur := s
	revived := false
	for depth := 0; depth < maxDepth; depth++ {
		trimmed := strings.TrimSpace(cur)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			break // not a JSON-encoded string
		}
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
			break
		}
		if !looksJSON(inner) {
			break
		}
		cur = inner
		revived = true
	}
	return cur, revived
}

// looksJSON reports whether s, trimmed, begins like a JSON array or object.
func looksJSON(s string) bool {
	t := strings.TrimSpace(s)
	return len(t) > 0 && (t[0] == '[' || t[0] == '{')
}
