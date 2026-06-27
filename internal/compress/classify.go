package compress

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

type decisionKind int

const (
	decUntouched decisionKind = iota
	decHomogeneous
	decBuckets
	decSparse
)

type decision struct {
	kind          decisionKind
	discriminator string // set when kind == decBuckets
}

// asObjectRows returns the rows as []map[string]any if every element is a JSON
// object, else (nil,false).
func asObjectRows(items []any) ([]map[string]any, bool) {
	out := make([]map[string]any, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		out[i] = m
	}
	return out, true
}

// decideArray implements spec §4.1.
func decideArray(items []any, cfg Config) decision {
	if len(items) < cfg.MinItems {
		return decision{kind: decUntouched}
	}
	rows, ok := asObjectRows(items)
	if !ok {
		return decision{kind: decUntouched}
	}
	total := len(rows)
	keyFreq := map[string]int{}
	for _, r := range rows {
		for k := range r {
			keyFreq[k]++
		}
	}
	if len(keyFreq) == 0 {
		return decision{kind: decUntouched}
	}
	coreThreshold := int(math.Ceil(float64(total) * cfg.CoreFieldFraction))
	coreKeys := 0
	for _, f := range keyFreq {
		if f >= coreThreshold {
			coreKeys++
		}
	}
	coreRatio := float64(coreKeys) / float64(len(keyFreq))
	if coreRatio >= cfg.HeterogeneousCoreRatio {
		return decision{kind: decHomogeneous}
	}
	if disc := detectDiscriminator(rows, cfg); disc != "" {
		return decision{kind: decBuckets, discriminator: disc}
	}
	return decision{kind: decSparse}
}

// detectDiscriminator implements spec §4.2: a key present in every row, always
// a string, with a distinct-value count in [min,max] and distinct/total <= 0.7.
// Among qualifying keys the one with the most distinct values wins; ties broken
// by name ascending for determinism.
func detectDiscriminator(rows []map[string]any, cfg Config) string {
	total := len(rows)
	type cand struct {
		key      string
		distinct int
	}
	var cands []cand
	// Collect candidate keys deterministically (sorted).
	keySet := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			keySet[k] = true
		}
	}
	for _, k := range sortedKeys(keySet) {
		distinct := map[string]bool{}
		ok := true
		for _, r := range rows {
			v, present := r[k]
			if !present {
				ok = false
				break
			}
			s, isStr := v.(string)
			if !isStr {
				ok = false
				break
			}
			distinct[s] = true
		}
		if !ok {
			continue
		}
		d := len(distinct)
		if d < cfg.MinBuckets || d > cfg.MaxBuckets {
			continue
		}
		if float64(d)/float64(total) > 0.7 {
			continue
		}
		cands = append(cands, cand{key: k, distinct: d})
	}
	best := ""
	bestD := -1
	for _, c := range cands {
		if c.distinct > bestD {
			best, bestD = c.key, c.distinct
		}
	}
	return best
}

// column describes one CSV-schema column. path is the access path into a row
// (length 1 normally, length 2 for a flattened nested field like meta.region).
type column struct {
	name     string
	path     []string
	typ      cellType
	nullable bool
	freq     int
}

// buildColumns derives the ordered column set for a set of object rows,
// applying §3c nested-uniform flattening. Column order is frequency-desc then
// name-asc (spec §4.3, the determinism-critical rule).
func buildColumns(rows []map[string]any, cfg Config) []column {
	total := len(rows)
	// Discover top-level keys.
	topKeys := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			topKeys[k] = true
		}
	}

	// A literal '.' in a top-level key is ambiguous with the dotted column names
	// produced by §3c flattening — the decoder splits column names on '.', so it
	// would reconstruct {"a.b":v} as {"a":{"b":v}}. Bail (no columns) so such an
	// array passes through uncompacted, preserving losslessness.
	for k := range topKeys {
		if strings.Contains(k, ".") {
			return nil
		}
	}

	// Determine which top keys are flattenable: present in every row, always an
	// object, with an identical inner-key set of size in [1, MaxFlattenInnerKeys].
	flatten := map[string][]string{} // top key -> sorted inner keys
	for _, k := range sortedKeys(topKeys) {
		var innerSig []string
		ok := true
		for _, r := range rows {
			v, present := r[k]
			if !present {
				ok = false
				break
			}
			obj, isObj := v.(map[string]any)
			if !isObj {
				ok = false
				break
			}
			sig := sortedKeys(obj)
			if innerSig == nil {
				innerSig = sig
			} else if !equalStrings(innerSig, sig) {
				ok = false
				break
			}
		}
		// Don't flatten if an inner key contains '.', which would make the dotted
		// column name ambiguous on decode; render that field as a json cell instead.
		if ok && anyContainsDot(innerSig) {
			ok = false
		}
		if ok && len(innerSig) >= 1 && len(innerSig) <= cfg.MaxFlattenInnerKeys {
			flatten[k] = innerSig
		}
	}

	// Build the column paths.
	var cols []column
	for _, k := range sortedKeys(topKeys) {
		if inner, isFlat := flatten[k]; isFlat {
			for _, ik := range inner {
				cols = append(cols, column{name: k + "." + ik, path: []string{k, ik}})
			}
			continue
		}
		cols = append(cols, column{name: k, path: []string{k}})
	}

	// Compute type, nullability, frequency for each column.
	for ci := range cols {
		c := &cols[ci]
		var typ cellType
		nullable := false
		freq := 0
		for _, r := range rows {
			v, present := getPath(r, c.path)
			if !present {
				nullable = true
				continue
			}
			freq++
			if v == nil {
				nullable = true
				continue
			}
			typ = mergeType(typ, v)
		}
		if freq < total {
			nullable = true
		}
		if typ == "" {
			typ = typeString // all null/missing
		}
		c.typ = typ
		c.nullable = nullable
		c.freq = freq
	}

	// Order: frequency desc, then name asc.
	sort.SliceStable(cols, func(i, j int) bool {
		if cols[i].freq != cols[j].freq {
			return cols[i].freq > cols[j].freq
		}
		return cols[i].name < cols[j].name
	})
	return cols
}

// getPath walks a nested access path into a row. Returns (value, present).
func getPath(row map[string]any, path []string) (any, bool) {
	var cur any = row
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, present := m[p]
		if !present {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func anyContainsDot(keys []string) bool {
	for _, k := range keys {
		if strings.Contains(k, ".") {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// decodeArray decodes a JSON array string with UseNumber. Returns (nil,false)
// if s is not a JSON array.
func decodeArray(s string) ([]any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var items []any
	if err := dec.Decode(&items); err != nil {
		return nil, false
	}
	// Reject trailing data after the array: re-encoding would silently drop it,
	// which would not be lossless. Treat as "not a clean array" -> no-op.
	if dec.More() {
		return nil, false
	}
	return items, true
}
