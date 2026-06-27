package compress

import (
	"regexp"
	"strings"
)

// classifyOpaque decides whether s is an "opaque" blob worth offloading behind a
// retrieval marker (spec §3d): a large base64 payload, an HTML document, or a
// long unbroken string. It returns a KIND tag and whether s qualifies. It is
// deliberately conservative — natural-language prose (which has spaces and is
// better handled by the §3e text path) is rejected.
func classifyOpaque(s string, cfg Config) (string, bool) {
	if len(s) <= cfg.OpaqueMinBytes {
		return "", false
	}
	// HTML: contains a closing tag or an opening tag-like sequence.
	if strings.Contains(s, "</") || strings.Contains(s, "/>") || htmlTagRe.MatchString(s) {
		return "html", true
	}
	// Character census over ASCII (UTF-8 continuation bytes never collide with
	// the ASCII set we test, so this is rune-safe for the ratios we care about).
	var b64chars, spaces int
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			spaces++
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
			b64chars++
		}
	}
	n := len(s)
	// base64 / base64url: almost entirely alphabet chars, essentially no spaces.
	if float64(b64chars)/float64(n) >= 0.97 && float64(spaces)/float64(n) < 0.01 {
		return "base64", true
	}
	// A long unbroken string with very little whitespace (e.g. a serialized
	// token, hex dump, or data URI) — opaque, not prose.
	if float64(spaces)/float64(n) < 0.02 {
		return "string", true
	}
	return "", false
}

// htmlTagRe matches a simple opening HTML tag like <div or <p>.
var htmlTagRe = regexp.MustCompile(`<[a-zA-Z][a-zA-Z0-9]*[ >/]`)

// replaceOpaqueRows returns a shallow copy of rows with each top-level string
// field that classifies as opaque replaced by a ccr marker, plus the markers to
// persist. Iteration is over sorted keys for determinism. Rows are not mutated.
func replaceOpaqueRows(rows []any, cfg Config) ([]any, []Marker) {
	var markers []Marker
	out := make([]any, len(rows))
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			out[i] = r
			continue
		}
		var nm map[string]any
		for _, k := range sortedKeys(m) {
			v := m[k]
			s, isStr := v.(string)
			if !isStr {
				continue
			}
			kind, opaque := classifyOpaque(s, cfg)
			if !opaque {
				continue
			}
			if nm == nil {
				nm = cloneStringAnyMap(m)
			}
			hash := contentHash([]byte(s))
			nm[k] = opaqueMarker(hash, kind, len(s))
			markers = append(markers, Marker{Hash: hash, Content: []byte(s), Kind: kind})
		}
		if nm != nil {
			out[i] = nm
		} else {
			out[i] = r
		}
	}
	return out, markers
}

func cloneStringAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
