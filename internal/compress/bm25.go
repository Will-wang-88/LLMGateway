package compress

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// BM25 ranking constants (spec §5.2(5)) — fixed, not user knobs.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// tokenRe matches, in priority order, a UUID, a 4+digit run, or an
// alphanumeric word. Input is lowercased before matching so [a-f] covers hex.
var tokenRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9]{4,}|[a-z0-9]+`)

// tokenize splits s into BM25 terms (UUID | 4+digit | alphanumeric, lowercased).
func tokenize(s string) []string {
	return tokenRe.FindAllString(strings.ToLower(s), -1)
}

// bm25Rank scores each document against the query and returns document indices
// ordered by score descending; ties broken by ascending index (deterministic).
// If the query has no terms, indices are returned in ascending order.
func bm25Rank(docs []string, query string) []int {
	n := len(docs)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	qTerms := tokenize(query)
	if len(qTerms) == 0 || n == 0 {
		return order // natural order
	}

	docTokens := make([][]string, n)
	docLen := make([]int, n)
	totalLen := 0
	df := map[string]int{}
	for i, d := range docs {
		toks := tokenize(d)
		docTokens[i] = toks
		docLen[i] = len(toks)
		totalLen += len(toks)
		seen := map[string]bool{}
		for _, t := range toks {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}
	avgdl := 1.0
	if n > 0 && totalLen > 0 {
		avgdl = float64(totalLen) / float64(n)
	}

	// Unique query terms with their floored Lucene IDF.
	idf := map[string]float64{}
	for _, t := range qTerms {
		if _, ok := idf[t]; ok {
			continue
		}
		nq := df[t]
		// Lucene floored IDF: log(1 + (N - n + 0.5)/(n + 0.5)).
		idf[t] = math.Log(1 + (float64(n)-float64(nq)+0.5)/(float64(nq)+0.5))
	}

	scores := make([]float64, n)
	for i := range docs {
		tf := map[string]int{}
		for _, t := range docTokens[i] {
			tf[t]++
		}
		dl := float64(docLen[i])
		var s float64
		for t, w := range idf {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			denom := f + bm25K1*(1-bm25B+bm25B*dl/avgdl)
			s += w * (f * (bm25K1 + 1)) / denom
		}
		scores[i] = s
	}

	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if scores[ia] != scores[ib] {
			return scores[ia] > scores[ib]
		}
		return ia < ib
	})
	return order
}
