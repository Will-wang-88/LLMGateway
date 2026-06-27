package compress

import (
	"encoding/json"
	"sort"
	"strings"
)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cellType is a CSV-schema column type tag (spec §4.3).
type cellType string

const (
	typeInt    cellType = "int"
	typeFloat  cellType = "float"
	typeBool   cellType = "bool"
	typeString cellType = "string"
	typeNull   cellType = "null"
	typeJSON   cellType = "json"
)

// nativeType returns the CSV-schema type for a single decoded JSON value, where
// numbers arrived as json.Number (via UseNumber). It never returns typeNull for
// a present value other than JSON null.
func nativeType(v any) cellType {
	switch t := v.(type) {
	case nil:
		return typeNull
	case bool:
		return typeBool
	case json.Number:
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			return typeFloat
		}
		return typeInt
	case string:
		return typeString
	default:
		// objects, arrays => embedded JSON
		return typeJSON
	}
}

// mergeType combines an accumulated column type with a new value's type per the
// inference rule: all-same-native => that tag; mixed => json; nulls ignored.
func mergeType(acc cellType, v any) cellType {
	nt := nativeType(v)
	if nt == typeNull {
		return acc // nulls don't constrain the type
	}
	if acc == "" || acc == typeNull {
		return nt
	}
	if acc == nt {
		return acc
	}
	// int + float are both numeric but render identically from json.Number
	// literals; keep them distinct only as int/float, otherwise json.
	if (acc == typeInt && nt == typeFloat) || (acc == typeFloat && nt == typeInt) {
		return typeFloat
	}
	return typeJSON
}
