package compress

import "sort"

// Compress applies the deterministic compression pipeline to an LLM request
// body and returns the (possibly rewritten) body plus stats. It is a PURE
// function of (reqBody, cfg): retrieval markers are returned in stats.Markers
// for the caller to persist, never written here, so the output bytes are
// identical with or without a store.
//
// Contract: on ANY failure, uncertainty, or net non-improvement, out is
// byte-identical to reqBody. The caller can always forward out safely.
func Compress(reqBody []byte, cfg Config) (out []byte, stats Stats) {
	stats = noopStats(reqBody)
	out = reqBody

	// Backstop: a panic anywhere in the pipeline degrades to a safe no-op.
	defer func() {
		if r := recover(); r != nil {
			out = reqBody
			stats = noopStats(reqBody)
			stats.Warnings = append(stats.Warnings, "recovered from panic")
		}
	}()

	if !cfg.Enabled {
		return reqBody, stats
	}

	env, ok := parseEnvelope(reqBody, cfg)
	if !ok || len(env.movables) == 0 {
		return reqBody, stats
	}

	transforms := map[string]bool{}

	// Stage 3 — lossless transforms, applied unconditionally per movable block.
	for _, m := range env.movables {
		newContent, names := losslessCompact(m.content, cfg)
		if len(names) > 0 && newContent != m.content {
			env.setContent(m, newContent)
			for _, n := range names {
				transforms[n] = true
			}
		}
	}

	// Stage 4 — conservative lossy, only if still over the token budget.
	if lossyPass(env, cfg, &stats, transforms) {
		stats.Lossy = true
	}

	if !env.changed() {
		return reqBody, stats
	}

	rendered, err := env.render()
	if err != nil || len(rendered) >= len(reqBody) {
		// Net non-improvement (or render failure) => safe no-op.
		return reqBody, noopStats(reqBody)
	}

	stats.Applied = true
	stats.BytesAfter = len(rendered)
	stats.TokensAfter = EstimateTokens(rendered)
	stats.Ratio = ratio(stats.TokensBefore, stats.TokensAfter)
	stats.TransformsApplied = sortedSet(transforms)
	return rendered, stats
}

// losslessCompact runs the per-block lossless transforms and returns the
// rewritten content plus the names of transforms that fired. If nothing
// applied it returns (content, nil).
func losslessCompact(content string, cfg Config) (string, []string) {
	if cfg.CompactionFormat == "json" {
		// CSV-schema disabled; only the cheap text transform applies.
		if out, applied := dedupTextLines(content, cfg); applied {
			return out, []string{"dedup-lines"}
		}
		return content, nil
	}

	// Structured path: array of uniform objects -> CSV-schema table. First try
	// §3d stringified-JSON revival so a doubly-encoded array can tabularize.
	candidate, revived := maybeReviveToArray(content, cfg)
	if items, ok := decodeArray(candidate); ok {
		if encoded, applied := compactArray(items, cfg, 0, ""); applied {
			if adopt(len(content), len(encoded), cfg) {
				names := []string{"csv-schema"}
				if revived {
					names = append(names, "revive")
				}
				return encoded, names
			}
		}
		return content, nil
	}

	// Plain-text path: exact duplicate-line removal (lossless).
	if out, applied := dedupTextLines(content, cfg); applied {
		return out, []string{"dedup-lines"}
	}
	return content, nil
}

// compactArray renders a decoded array as a CSV-schema payload if it qualifies.
// droppedN > 0 records rows removed by the lossy stage. Returns ("",false) if
// the array does not qualify or cannot be safely encoded.
func compactArray(items []any, cfg Config, droppedN int, marker string) (string, bool) {
	d := decideArray(items, cfg)
	rows, ok := asObjectRows(items)
	if !ok {
		return "", false
	}
	switch d.kind {
	case decHomogeneous, decSparse:
		cols := buildColumns(rows, cfg)
		if len(cols) == 0 {
			return "", false // unsafe (e.g. dotted keys) -> passthrough
		}
		return encodeTable(rows, cols, droppedN, marker)
	case decBuckets:
		return encodeBuckets(rows, d.discriminator, cfg, droppedN, marker)
	default:
		return "", false
	}
}

// adopt implements the §3f gate: only adopt a transform if it saves at least
// LosslessMinSavingsRatio of the original bytes.
func adopt(before, after int, cfg Config) bool {
	if before <= 0 || after >= before {
		return false
	}
	savings := float64(before-after) / float64(before)
	return savings >= cfg.LosslessMinSavingsRatio
}

func noopStats(reqBody []byte) Stats {
	t := EstimateTokens(reqBody)
	return Stats{
		BytesBefore:  len(reqBody),
		BytesAfter:   len(reqBody),
		TokensBefore: t,
		TokensAfter:  t,
		Ratio:        1.0,
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
