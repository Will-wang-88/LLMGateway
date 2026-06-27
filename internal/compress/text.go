package compress

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// dedupTextLines removes exact duplicate non-empty lines, keeping the first
// occurrence of each (spec §3e, lossless class). Blank lines are preserved as
// structure. It returns (out, applied) where applied is true only if lines were
// removed AND the §3f adopt gate is satisfied.
func dedupTextLines(content string, cfg Config) (string, bool) {
	if !strings.Contains(content, "\n") {
		return content, false
	}
	lines := strings.Split(content, "\n")
	seen := make(map[string]bool, len(lines))
	out := make([]string, 0, len(lines))
	removed := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			out = append(out, ln)
			continue
		}
		if seen[ln] {
			removed = true
			continue
		}
		seen[ln] = true
		out = append(out, ln)
	}
	if !removed {
		return content, false
	}
	joined := strings.Join(out, "\n")
	if !adopt(len(content), len(joined), cfg) {
		return content, false
	}
	return joined, true
}

// proseLossy applies the budget-gated §3e prose path: near-duplicate line
// removal (word-shingle Jaccard >= DedupThreshold) followed by BM25-extractive
// line selection down to roughly half, always keeping the first/last lines,
// error lines, and query-relevant lines. The original text is offloaded behind a
// trailing <<ccr:HASH N_lines_offloaded>> marker (returned for the caller to
// persist). It refuses to touch valid JSON so it never corrupts structured data.
//
// Returns (out, marker, applied). applied is false (and out empty) when the
// content is JSON, too small, or the result wouldn't clear the adopt gate.
func proseLossy(content, query string, cfg Config) (string, Marker, bool) {
	if json.Valid([]byte(content)) {
		return "", Marker{}, false // structured data — never line-edit it
	}
	if !strings.Contains(content, "\n") {
		return "", Marker{}, false
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 6 {
		return "", Marker{}, false
	}

	deduped := nearDedupLines(lines, cfg.DedupThreshold)
	target := len(deduped) / 2
	if target < 5 {
		target = 5
	}
	selected := extractiveSelect(deduped, query, target, cfg)
	if len(selected) >= len(lines) {
		return "", Marker{}, false // nothing removed
	}

	hash := contentHash([]byte(content))
	out := strings.Join(selected, "\n") + "\n<<ccr:" + hash + " " +
		strconv.Itoa(len(lines)-len(selected)) + "_lines_offloaded>>"
	if !adopt(len(content), len(out), cfg) {
		return "", Marker{}, false
	}
	return out, Marker{Hash: hash, Content: []byte(content), Kind: "text"}, true
}

// nearDedupLines removes near-duplicate non-blank lines (word-shingle Jaccard >=
// threshold), keeping the first occurrence. Blank lines are preserved as
// structure. Exact duplicates are a special case (Jaccard 1.0).
func nearDedupLines(lines []string, threshold float64) []string {
	var out []string
	var keptShingles []map[string]bool
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			out = append(out, ln)
			continue
		}
		sh := wordShingles(t, 3)
		dup := false
		for _, ks := range keptShingles {
			if jaccard(sh, ks) >= threshold {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, ln)
		keptShingles = append(keptShingles, sh)
	}
	return out
}

// extractiveSelect keeps query-relevant and structurally-important lines up to
// target, preserving original order. Always keeps the first 2 and last 3 lines,
// error-keyword lines, and query-anchor lines; fills the rest by BM25 score.
func extractiveSelect(lines []string, query string, target int, cfg Config) []string {
	n := len(lines)
	if n <= target {
		return lines
	}
	keep := map[int]bool{}
	anchors := extractAnchors(query)
	for i, ln := range lines {
		low := strings.ToLower(ln)
		if containsErrorKeyword(low) || matchesAnchor(low, anchors) {
			keep[i] = true
		}
	}
	for _, i := range firstK(n, 2) {
		keep[i] = true
	}
	for _, i := range lastK(n, 3) {
		keep[i] = true
	}
	for _, idx := range bm25Rank(lines, query) {
		if len(keep) >= target {
			break
		}
		keep[idx] = true
	}
	idxs := make([]int, 0, len(keep))
	for i := range keep {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]string, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, lines[i])
	}
	return out
}

// wordShingles returns the set of k-word shingles of s (lowercased). If s has
// fewer than k words it falls back to the set of single words.
func wordShingles(s string, k int) map[string]bool {
	words := strings.Fields(strings.ToLower(s))
	set := map[string]bool{}
	if len(words) < k {
		for _, w := range words {
			set[w] = true
		}
		return set
	}
	for i := 0; i+k <= len(words); i++ {
		set[strings.Join(words[i:i+k], " ")] = true
	}
	return set
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
