package compress

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// canonicalJSON renders v with map keys sorted. Go's encoding/json already
// sorts map[string]... keys alphabetically when marshaling, so json.Marshal is
// naturally canonical for our object rows. Numbers are preserved as json.Number
// when v came from a UseNumber decoder.
func canonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// contentHash returns the SHA-256[:12] hex digest of b. It is a pure function
// of the bytes: identical dropped content always yields an identical marker,
// independent of any store state.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// rowsHash hashes the canonical JSON of a slice of dropped rows.
func rowsHash(rows []any) string {
	return contentHash(canonicalJSON(rows))
}

// dedupKey returns a stable key for row-content deduplication: the MD5 of the
// row's key-sorted canonical JSON. Input key order does not affect the key.
func dedupKey(v any) string {
	sum := md5.Sum(canonicalJSON(v))
	return hex.EncodeToString(sum[:])
}

// humanSize renders a byte count like "4.5KB" for opaque markers.
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// arrayMarker renders the array-form retrieval sentinel:
//
//	<<ccr:HASH N_rows_offloaded>>
func arrayMarker(hash string, n int) string {
	return fmt.Sprintf("<<ccr:%s %d_rows_offloaded>>", hash, n)
}

// opaqueMarker renders the opaque-blob cell marker:
//
//	<<ccr:HASH,KIND,SIZE>>
func opaqueMarker(hash, kind string, size int) string {
	return fmt.Sprintf("<<ccr:%s,%s,%s>>", hash, kind, humanSize(size))
}

// isCCRMarker reports whether s looks like any ccr marker produced by this package.
func isCCRMarker(s string) bool {
	return strings.Contains(s, "<<ccr:")
}
