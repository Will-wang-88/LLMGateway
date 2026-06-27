package compress

import "strings"

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
