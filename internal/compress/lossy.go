package compress

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// lossyPass is the budget-gated conservative-lossy stage (spec §4 Stage 4 /
// §5). It runs only when the request, after all lossless transforms, is still
// over the token budget. For each oversized array tool-result it drops
// low-value rows (keeping first/last/error/query-relevant) down toward
// MaxItemsAfterCrush, replacing them with a CSV (or JSON) payload carrying a
// retrieval marker. The dropped bytes are returned in stats.Markers for the
// caller to persist; nothing is written to a store here.
//
// Returns true if any lossy transform fired.
func lossyPass(env *envelope, cfg Config, stats *Stats, transforms map[string]bool) bool {
	if cfg.LosslessOnly || !cfg.CCRMarkerEnabled || cfg.TokenBudget <= 0 {
		return false
	}
	cur, err := env.render()
	if err != nil {
		return false
	}
	if EstimateTokens(cur) <= cfg.TokenBudget {
		return false // still within budget after lossless
	}
	query := env.queryText()
	target := cfg.MaxItemsAfterCrush
	if target < 1 {
		target = 1
	}

	applied := false
	for _, m := range env.movables {
		items, ok := decodeArray(m.content)
		if !ok {
			cand, _ := maybeReviveToArray(m.content, cfg)
			if items, ok = decodeArray(cand); !ok {
				// Not an array: a whole-content opaque blob (§3d) can be offloaded
				// behind a marker; otherwise try the §3e prose path.
				if kind, opaque := classifyOpaque(m.content, cfg); opaque {
					hash := contentHash([]byte(m.content))
					env.setContent(m, opaqueMarker(hash, kind, len(m.content)))
					stats.Markers = append(stats.Markers, Marker{Hash: hash, Content: []byte(m.content), Kind: kind})
					transforms["opaque"] = true
					applied = true
				} else if out, marker, ok := proseLossy(m.content, query, cfg); ok {
					env.setContent(m, out)
					stats.Markers = append(stats.Markers, marker)
					transforms["prose"] = true
					applied = true
				}
				continue
			}
		}
		rows, ok := asObjectRows(items)
		if !ok {
			continue
		}

		if len(rows) > target {
			// Row-drop path. Hash the dropped rows BEFORE any opaque rewrite so a
			// row-drop retrieval returns the original full rows.
			keep, dropped := selectKeep(rows, query, target, cfg)
			if len(dropped) == 0 {
				continue
			}
			keptItems := pickItems(items, keep)
			droppedItems := pickItems(items, dropped)
			droppedBytes := canonicalJSON(droppedItems)
			hash := contentHash(droppedBytes)
			// Offload opaque blobs inside the kept rows too (§3d).
			keptItems, opaqueMarkers := replaceOpaqueRows(keptItems, cfg)
			content, ok := renderLossy(keptItems, droppedItems, hash, cfg)
			if !ok {
				continue
			}
			env.setContent(m, content)
			stats.Markers = append(stats.Markers, Marker{Hash: hash, Content: droppedBytes, Kind: "rows"})
			stats.Markers = append(stats.Markers, opaqueMarkers...)
			stats.RowsOffloaded += len(dropped)
			transforms["row-drop"] = true
			if len(opaqueMarkers) > 0 {
				transforms["opaque"] = true
			}
			applied = true
			continue
		}

		// Not oversized, but we're over budget: still offload any opaque blobs
		// in the array's cells and re-encode if that helped.
		newItems, opaqueMarkers := replaceOpaqueRows(items, cfg)
		if len(opaqueMarkers) == 0 {
			continue
		}
		content, ok := compactArray(newItems, cfg, 0, "")
		if !ok {
			b, err := json.Marshal(newItems)
			if err != nil {
				continue
			}
			content = string(b)
		}
		env.setContent(m, content)
		stats.Markers = append(stats.Markers, opaqueMarkers...)
		transforms["opaque"] = true
		applied = true
	}
	return applied
}

// renderLossy renders the kept rows plus a retrieval marker. It prefers the
// CSV-schema form (with ` __dropped:N <<ccr:...>>`); if the kept rows don't
// qualify for CSV it falls back to a JSON array with a trailing sentinel object.
func renderLossy(keptItems, droppedItems []any, hash string, cfg Config) (string, bool) {
	marker := arrayMarker(hash, len(droppedItems))
	if csv, ok := compactArray(keptItems, cfg, len(droppedItems), marker); ok {
		return csv, true
	}
	arr := append(append([]any{}, keptItems...), map[string]any{"_ccr_dropped": marker})
	b, err := json.Marshal(arr)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// selectKeep implements the deterministic keep-set selection of spec §5.2/§5.3.
// It returns the kept and dropped row indices, both ascending.
func selectKeep(rows []map[string]any, query string, target int, cfg Config) (keep, dropped []int) {
	n := len(rows)
	if target < 1 {
		target = 1
	}
	if n <= target {
		return seqInts(n), nil
	}

	ser := make([]string, n)
	lower := make([]string, n)
	for i, r := range rows {
		ser[i] = string(canonicalJSON(r))
		lower[i] = strings.ToLower(ser[i])
	}

	keepSet := map[int]bool{}
	critical := map[int]bool{} // never removed by dedup/trim

	// (2) error rows + (4) query anchors: hard keeps.
	anchors := extractAnchors(query)
	for i := 0; i < n; i++ {
		if containsErrorKeyword(lower[i]) || matchesAnchor(lower[i], anchors) {
			keepSet[i] = true
			critical[i] = true
		}
	}
	// Mandatory position anchors: first 3, last 2 (never trimmed).
	for _, i := range firstK(n, 3) {
		keepSet[i] = true
		critical[i] = true
	}
	for _, i := range lastK(n, 2) {
		keepSet[i] = true
		critical[i] = true
	}

	// (1) soft position anchors: first FirstFraction, last LastFraction.
	soft := map[int]bool{}
	firstN := int(math.Ceil(float64(n) * cfg.FirstFraction))
	lastN := int(math.Ceil(float64(n) * cfg.LastFraction))
	for i := 0; i < firstN && i < n; i++ {
		soft[i] = true
	}
	for i := n - lastN; i < n; i++ {
		if i >= 0 {
			soft[i] = true
		}
	}

	// Fill order: soft anchors (ascending) then BM25-ranked rows.
	fillOrder := append(sortedIntSet(soft), bm25Rank(ser, query)...)
	for _, idx := range fillOrder {
		if len(keepSet) >= target {
			break
		}
		keepSet[idx] = true
	}

	// (5.3.1) content dedup: identical rows converge to the min index. Critical
	// rows are exempt so error/anchor rows always survive.
	seen := map[string]bool{}
	for _, idx := range sortedIntSet(keepSet) {
		h := dedupKey(rows[idx])
		if seen[h] && !critical[idx] {
			delete(keepSet, idx)
			continue
		}
		seen[h] = true
	}

	// (5.3.2) gap fill: if dedup dropped us below target, add diverse unique
	// rows by a deterministic equidistant stride.
	if len(keepSet) < target {
		gapFill(rows, keepSet, seen, target)
	}

	keep = sortedIntSet(keepSet)
	dropped = complementInts(n, keepSet)
	return keep, dropped
}

// gapFill adds content-unique, not-yet-kept rows using a deterministic stride
// so the retained sample stays diverse.
func gapFill(rows []map[string]any, keepSet map[int]bool, seen map[string]bool, target int) {
	n := len(rows)
	var cand []int
	for i := 0; i < n; i++ {
		if keepSet[i] {
			continue
		}
		if seen[dedupKey(rows[i])] {
			continue
		}
		cand = append(cand, i)
	}
	if len(cand) == 0 {
		return
	}
	remaining := target - len(keepSet)
	if remaining <= 0 {
		return
	}
	step := len(cand) / (remaining + 1)
	if step < 1 {
		step = 1
	}
	for k := 0; k < len(cand) && len(keepSet) < target; k += step {
		idx := cand[k]
		h := dedupKey(rows[idx])
		if seen[h] {
			continue
		}
		keepSet[idx] = true
		seen[h] = true
	}
	// Top up sequentially if the stride left us short.
	for k := 0; k < len(cand) && len(keepSet) < target; k++ {
		idx := cand[k]
		h := dedupKey(rows[idx])
		if seen[h] {
			continue
		}
		keepSet[idx] = true
		seen[h] = true
	}
}

// --- small deterministic int-set helpers ---

func seqInts(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func sortedIntSet(s map[int]bool) []int {
	out := make([]int, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func complementInts(n int, s map[int]bool) []int {
	var out []int
	for i := 0; i < n; i++ {
		if !s[i] {
			out = append(out, i)
		}
	}
	return out
}

func firstK(n, k int) []int {
	if k > n {
		k = n
	}
	return seqInts(k)
}

func lastK(n, k int) []int {
	if k > n {
		k = n
	}
	out := make([]int, 0, k)
	for i := n - k; i < n; i++ {
		out = append(out, i)
	}
	return out
}

func pickItems(items []any, idx []int) []any {
	out := make([]any, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(items) {
			out = append(out, items[i])
		}
	}
	return out
}
