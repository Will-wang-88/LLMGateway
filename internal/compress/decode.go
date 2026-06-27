package compress

import (
	"encoding/json"
	"strconv"
	"strings"
)

// decColumn is a parsed declaration column.
type decColumn struct {
	name     string
	path     []string
	typ      cellType
	nullable bool
}

type cell struct {
	text   string
	quoted bool
}

// DecodeCSVSchema reverses the CSV-schema encoding back to a slice of rows. It
// returns (nil,false) if s is not a recognized CSV-schema payload. Used by the
// round-trip tests (and available for retrieval/debugging). Null and missing
// cells both decode to a present key with a nil value, per spec §4.3.
func DecodeCSVSchema(s string) ([]any, bool) {
	switch {
	case strings.HasPrefix(s, "__buckets:"):
		return decodeBuckets(s)
	case strings.HasPrefix(s, "["):
		return decodeTable(s)
	default:
		return nil, false
	}
}

func decodeTable(s string) ([]any, bool) {
	nl := strings.IndexByte(s, '\n')
	decl := s
	data := ""
	if nl >= 0 {
		decl = s[:nl]
		data = s[nl+1:]
	}
	cols, ok := parseDeclaration(decl)
	if !ok {
		return nil, false
	}
	records := parseRecords(data)
	return recordsToRows(records, cols, "", ""), true
}

func decodeBuckets(s string) ([]any, bool) {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "__buckets:") {
		return nil, false
	}
	disc := strings.TrimPrefix(lines[0], "__buckets:")
	// Drop a trailing " __dropped:N" if present.
	if i := strings.Index(disc, " __dropped:"); i >= 0 {
		disc = disc[:i]
	}
	var out []any
	i := 1
	for i < len(lines) {
		if !strings.HasPrefix(lines[i], "__key:") {
			i++
			continue
		}
		key := strings.TrimPrefix(lines[i], "__key:")
		i++
		if i >= len(lines) {
			break
		}
		cols, ok := parseDeclaration(lines[i])
		if !ok {
			return nil, false
		}
		i++
		// Collect this bucket's data lines until the next __key or end.
		var dataLines []string
		for i < len(lines) && !strings.HasPrefix(lines[i], "__key:") {
			dataLines = append(dataLines, lines[i])
			i++
		}
		records := parseRecords(strings.Join(dataLines, "\n"))
		out = append(out, recordsToRows(records, cols, disc, key)...)
	}
	return out, true
}

// recordsToRows turns parsed CSV records into row maps using the column schema.
// If discName is non-empty, every row gets discName=discValue re-injected
// (bucket decode).
func recordsToRows(records [][]cell, cols []decColumn, discName, discValue string) []any {
	rows := make([]any, 0, len(records))
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		row := map[string]any{}
		for ci, c := range cols {
			if ci >= len(rec) {
				setPath(row, c.path, nil)
				continue
			}
			setPath(row, c.path, decodeCell(rec[ci], c.typ))
		}
		if discName != "" {
			row[discName] = discValue
		}
		rows = append(rows, row)
	}
	return rows
}

func decodeCell(cl cell, typ cellType) any {
	if !cl.quoted && cl.text == "" {
		return nil // null / missing
	}
	if cl.quoted {
		// Quoted: a string value, or quoted JSON for a json-typed column.
		if typ == typeJSON {
			if v, ok := unmarshalAny(cl.text); ok {
				return v
			}
		}
		return cl.text
	}
	switch typ {
	case typeInt, typeFloat:
		return json.Number(cl.text)
	case typeBool:
		return cl.text == "true"
	case typeJSON:
		if v, ok := unmarshalAny(cl.text); ok {
			return v
		}
		return cl.text
	case typeNull:
		return nil
	default: // string
		return cl.text
	}
}

func unmarshalAny(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// parseDeclaration parses `[N]{name:typ?,...}` (with an optional trailing
// ` __dropped:N`) into the column schema.
func parseDeclaration(line string) ([]decColumn, bool) {
	if len(line) == 0 || line[0] != '[' {
		return nil, false
	}
	rb := strings.IndexByte(line, ']')
	if rb < 0 {
		return nil, false
	}
	if _, err := strconv.Atoi(line[1:rb]); err != nil {
		return nil, false
	}
	rest := line[rb+1:]
	if len(rest) == 0 || rest[0] != '{' {
		return nil, false
	}
	cb := strings.IndexByte(rest, '}')
	if cb < 0 {
		return nil, false
	}
	colpart := rest[1:cb]
	if strings.TrimSpace(colpart) == "" {
		return []decColumn{}, true
	}
	var cols []decColumn
	for _, spec := range strings.Split(colpart, ",") {
		colon := strings.IndexByte(spec, ':')
		if colon < 0 {
			return nil, false
		}
		name := spec[:colon]
		typ := spec[colon+1:]
		nullable := strings.HasSuffix(typ, "?")
		typ = strings.TrimSuffix(typ, "?")
		cols = append(cols, decColumn{
			name:     name,
			path:     strings.Split(name, "."),
			typ:      cellType(typ),
			nullable: nullable,
		})
	}
	return cols, true
}

// parseRecords parses CSV-ish data into records of cells, tracking whether each
// field was quoted (so a null empty cell is distinguishable from an explicit
// empty string). Handles RFC-4180 quoting incl. embedded commas/newlines and
// "" escaping. Operates on bytes; ASCII delimiters never collide with UTF-8
// continuation bytes, so it is rune-safe.
func parseRecords(data string) [][]cell {
	var records [][]cell
	var cur []cell
	i, n := 0, len(data)
	for i < n {
		var text strings.Builder
		quoted := false
		if data[i] == '"' {
			quoted = true
			i++
			for i < n {
				c := data[i]
				if c == '"' {
					if i+1 < n && data[i+1] == '"' {
						text.WriteByte('"')
						i += 2
						continue
					}
					i++ // consume closing quote
					break
				}
				text.WriteByte(c)
				i++
			}
		} else {
			for i < n && data[i] != ',' && data[i] != '\n' {
				if data[i] == '\r' {
					i++
					continue
				}
				text.WriteByte(data[i])
				i++
			}
		}
		cur = append(cur, cell{text: text.String(), quoted: quoted})
		switch {
		case i < n && data[i] == ',':
			i++
		case i < n && (data[i] == '\n' || data[i] == '\r'):
			if data[i] == '\r' && i+1 < n && data[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
			records = append(records, cur)
			cur = nil
		default: // EOF
			i++
		}
	}
	if len(cur) > 0 {
		records = append(records, cur)
	}
	return records
}

// setPath assigns value at a nested access path, creating intermediate maps.
func setPath(row map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	cur := row
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}
