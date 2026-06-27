package compress

import (
	"regexp"
	"strings"
)

// errorKeywords are the 12 pinned, ASCII, lowercase keywords that force a row
// to be retained during lossy row-drop (spec §5.2(2)). Fixed, not configurable.
var errorKeywords = []string{
	"error", "exception", "failed", "failure", "critical", "fatal",
	"crash", "panic", "abort", "timeout", "denied", "rejected",
}

// containsErrorKeyword reports whether the already-lowercased serialization s
// contains any pinned error keyword. Uses strings.Contains (not regex) on the
// hot path.
func containsErrorKeyword(lower string) bool {
	for _, kw := range errorKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// Precompiled anchor extractors (spec §5.2(4)). Compiled once at package init.
var (
	anchorUUID   = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	anchorDigits = regexp.MustCompile(`[0-9]{4,}`)
	anchorEmail  = regexp.MustCompile(`[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	anchorHost   = regexp.MustCompile(`[a-z0-9][a-z0-9\-]*(?:\.[a-z0-9\-]+)+`)
	anchorQuoted = regexp.MustCompile(`"([^"\n]{1,80})"|'([^'\n]{1,80})'`)
)

// extractAnchors pulls deterministic, exact-match anchor substrings out of the
// query (UUID / 4+digit id / hostname / quoted string / email), lowercased.
// Returns a sorted, de-duplicated list for stable iteration.
func extractAnchors(query string) []string {
	q := strings.ToLower(query)
	set := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) >= 2 {
			set[s] = true
		}
	}
	for _, m := range anchorUUID.FindAllString(q, -1) {
		add(m)
	}
	for _, m := range anchorEmail.FindAllString(q, -1) {
		add(m)
	}
	for _, m := range anchorHost.FindAllString(q, -1) {
		add(m)
	}
	for _, m := range anchorDigits.FindAllString(q, -1) {
		add(m)
	}
	for _, m := range anchorQuoted.FindAllStringSubmatch(q, -1) {
		if m[1] != "" {
			add(m[1])
		}
		if m[2] != "" {
			add(m[2])
		}
	}
	return sortedKeys(set)
}

// matchesAnchor reports whether the already-lowercased row serialization
// contains any of the anchors as a substring.
func matchesAnchor(lower string, anchors []string) bool {
	for _, a := range anchors {
		if strings.Contains(lower, a) {
			return true
		}
	}
	return false
}
