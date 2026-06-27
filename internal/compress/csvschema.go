package compress

import (
	"encoding/json"
	"strconv"
	"strings"
)

// encodeTable renders a homogeneous or sparse table (spec §4.3). droppedN > 0
// appends the ` __dropped:N` suffix; a non-empty marker (e.g. a ccr retrieval
// marker) is appended after it. Returns ("",false) if the data cannot be safely
// represented (e.g. a column name containing a structural delimiter).
func encodeTable(rows []map[string]any, cols []column, droppedN int, marker string) (string, bool) {
	if !safeColumns(cols) {
		return "", false
	}
	var b strings.Builder
	writeDeclaration(&b, len(rows), cols, droppedN, marker)
	for _, r := range rows {
		b.WriteByte('\n')
		writeRow(&b, r, cols)
	}
	return b.String(), true
}

// encodeBuckets renders the bucketed form (spec §4.3) keyed by a discriminator
// column. The discriminator column is omitted from each per-bucket schema and
// carried on the __key line; the decoder re-injects it.
func encodeBuckets(rows []map[string]any, disc string, cfg Config, droppedN int, marker string) (string, bool) {
	// Group rows by discriminator value (deterministic order: value ascending).
	groups := map[string][]map[string]any{}
	for _, r := range rows {
		v, _ := r[disc].(string)
		if strings.ContainsAny(v, "\n\r") {
			return "", false // unsafe on the __key line
		}
		groups[v] = append(groups[v], r)
	}
	var b strings.Builder
	b.WriteString("__buckets:")
	b.WriteString(disc)
	if droppedN > 0 {
		b.WriteString(" __dropped:")
		b.WriteString(strconv.Itoa(droppedN))
	}
	if marker != "" {
		b.WriteByte(' ')
		b.WriteString(marker)
	}
	for _, key := range sortedKeys(groups) {
		bucket := groups[key]
		// Build columns for this bucket, excluding the discriminator column.
		cols := buildColumns(bucket, cfg)
		if len(cols) == 0 {
			return "", false // unsafe (e.g. dotted keys) -> passthrough
		}
		cols = dropColumn(cols, disc)
		if !safeColumns(cols) {
			return "", false
		}
		b.WriteByte('\n')
		b.WriteString("__key:")
		b.WriteString(key)
		b.WriteByte('\n')
		writeDeclaration(&b, len(bucket), cols, 0, "")
		for _, r := range bucket {
			b.WriteByte('\n')
			writeRow(&b, r, cols)
		}
	}
	return b.String(), true
}

func dropColumn(cols []column, name string) []column {
	out := cols[:0:0]
	for _, c := range cols {
		if len(c.path) == 1 && c.path[0] == name {
			continue
		}
		out = append(out, c)
	}
	return out
}

func writeDeclaration(b *strings.Builder, rowCount int, cols []column, droppedN int, marker string) {
	b.WriteByte('[')
	b.WriteString(strconv.Itoa(rowCount))
	b.WriteString("]{")
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.name)
		b.WriteByte(':')
		b.WriteString(string(c.typ))
		if c.nullable {
			b.WriteByte('?')
		}
	}
	b.WriteByte('}')
	if droppedN > 0 {
		b.WriteString(" __dropped:")
		b.WriteString(strconv.Itoa(droppedN))
	}
	if marker != "" {
		b.WriteByte(' ')
		b.WriteString(marker)
	}
}

func writeRow(b *strings.Builder, row map[string]any, cols []column) {
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(cellString(row, c))
	}
}

// cellString renders one cell (spec §4.3 value table).
func cellString(row map[string]any, c column) string {
	v, present := getPath(row, c.path)
	if !present || v == nil {
		return "" // null / missing -> empty cell
	}
	// In a json-typed (mixed) column every value — including strings, numbers,
	// and bools — must be rendered as compact JSON so the decoder can tell a
	// string "42" from the number 42. Rendering by dynamic type here would emit
	// a bare 42 that decodeCell would parse back as a number (a lossless bug).
	if c.typ == typeJSON {
		return csvEscape(compactJSON(v))
	}
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return string(t) // raw literal -> byte-exact round-trip
	case string:
		return csvEscape(t)
	default:
		// Nested object/array: compact JSON, then CSV-quote.
		return csvEscape(compactJSON(v))
	}
}

// csvEscape applies RFC-4180 quoting. An explicit empty string becomes a quoted
// empty ("") so the decoder can distinguish it from a null/missing empty cell.
func csvEscape(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// safeColumns reports whether every column name is free of structural
// delimiters that would corrupt the declaration line.
func safeColumns(cols []column) bool {
	for _, c := range cols {
		if strings.ContainsAny(c.name, ",:{}?\n\r") {
			return false
		}
	}
	return true
}
