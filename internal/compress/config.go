// Package compress implements a deterministic, lossless-first token compression
// pass over LLM request bodies. It rewrites large, uniform tool-result payloads
// (the §3b CSV-schema transform) and, only when a request is still over budget,
// applies a conservative lossy row-drop with retrievable markers.
//
// The cardinal contract: Compress returns bytes byte-identical to its input on
// any failure or uncertainty. It is a pure function of (input, config) — the
// retrieval store is written by the caller from the returned markers, never by
// Compress itself, so output bytes do not depend on store state.
package compress

import "time"

// Config holds every tunable knob (spec §8). Construct with DefaultConfig and
// override individual fields. All durations and counts are absolute; budget is
// estimated cheaply via EstimateTokens (bytes/4), never a real tokenizer.
type Config struct {
	Enabled bool

	// Lossless gating.
	MinTokensToCrush        int     // skip a block below this estimated token size
	MinItems                int     // array needs at least this many rows to tabularize
	CoreFieldFraction       float64 // fraction of rows a key must appear in to be "core"
	HeterogeneousCoreRatio  float64 // core/total key ratio below which we bucket/sparse
	MaxFlattenInnerKeys     int     // nested object flattened only if it has <= this many keys
	MinBuckets              int
	MaxBuckets              int
	OpaqueMinBytes          int     // string longer than this may become an opaque marker
	LosslessMinSavingsRatio float64 // adopt a transform only if it saves at least this fraction
	LosslessOnly            bool    // true => never emit markers, row-drop, or opaque blobs
	CompactionFormat        string  // "csv-schema" (default) | "json"
	ProtectRecentTurns      int     // never touch the most recent N tool results (cache-stable tail)
	FactorOutConstants      bool    // §3a, MVP off

	// Lossy (budget-gated) knobs.
	TokenBudget        int     // total request budget; <=0 disables the lossy stage
	MaxItemsAfterCrush int     // target row count for an oversized array
	FirstFraction      float64 // always-keep leading fraction of rows
	LastFraction       float64 // always-keep trailing fraction of rows
	DedupThreshold     float64 // prose near-dup shingle-Jaccard threshold
	CCRMarkerEnabled   bool    // false => strictly lossless (no markers at all)
	CCRTTL             time.Duration
	MaxDepth           int // recursion bound for nested arrays / stringified JSON
}

// DefaultConfig returns the spec §8 defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:                 true,
		MinTokensToCrush:        200,
		MinItems:                5,
		CoreFieldFraction:       0.8,
		HeterogeneousCoreRatio:  0.6,
		MaxFlattenInnerKeys:     6,
		MinBuckets:              2,
		MaxBuckets:              8,
		OpaqueMinBytes:          256,
		LosslessMinSavingsRatio: 0.15,
		LosslessOnly:            false,
		CompactionFormat:        "csv-schema",
		ProtectRecentTurns:      2,
		FactorOutConstants:      false,
		TokenBudget:             0,
		MaxItemsAfterCrush:      15,
		FirstFraction:           0.3,
		LastFraction:            0.15,
		DedupThreshold:          0.85,
		CCRMarkerEnabled:        true,
		CCRTTL:                  5 * time.Minute,
		MaxDepth:                4,
	}
}

// Marker records a single retrievable blob produced by the lossy stage. The
// caller writes these into a RetrievalStore; Compress never does.
type Marker struct {
	Hash    string // SHA-256[:12] hex of Content (a pure function of Content)
	Content []byte // the original bytes that were offloaded
	Kind    string // "rows", "base64", "html", "string", ...
}

// Stats describes what Compress did, for observability and the request log.
type Stats struct {
	Applied           bool     // true if the output differs from the input
	Lossy             bool     // true if any lossy transform fired
	BytesBefore       int      // len(input)
	BytesAfter        int      // len(output)
	TokensBefore      int      // EstimateTokens(input)
	TokensAfter       int      // EstimateTokens(output)
	Ratio             float64  // TokensAfter / TokensBefore (1.0 if not applied)
	RowsOffloaded     int      // rows dropped by the lossy stage
	TransformsApplied []string // names of transforms that fired, for debugging
	Markers           []Marker // retrievable blobs the caller should persist
	Warnings          []string // non-fatal notes
}

// ratio is TokensAfter/TokensBefore, clamped to [0,1]; 1.0 when before is zero.
func ratio(before, after int) float64 {
	if before <= 0 {
		return 1.0
	}
	r := float64(after) / float64(before)
	if r > 1.0 {
		return 1.0
	}
	if r < 0 {
		return 0
	}
	return r
}
