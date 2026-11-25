package models

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidReportedName identifies malformed input, distinct from a valid
// name that simply is not in the current catalog.
var ErrInvalidReportedName = errors.New("invalid reported model name")

// Resolution retains the normalized name even when no model matches.
// ID is positive only for a match; zero does not denote a total here.
type Resolution struct {
	NormalizedName string
	CanonicalName  string
	ID             int64
	Matched        bool
}

// Resolve removes surrounding whitespace and the path through the final '/'.
// Matching is exact, case-sensitive and limited to the current catalog.
func (r *Registry) Resolve(raw string) (Resolution, error) {
	name, err := NormalizeReportedName(raw)
	if err != nil {
		return Resolution{}, err
	}
	result := Resolution{NormalizedName: name}
	if index, ok := r.catalog.byName[name]; ok {
		canonical := r.catalog.models[index].Name
		result.CanonicalName = canonical
		result.ID = r.ids[canonical]
		result.Matched = true
	}
	return result, nil
}

// NormalizeReportedName validates and normalizes without consulting a catalog.
func NormalizeReportedName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	if name == "" {
		return "", fmt.Errorf("%w: final name is empty", ErrInvalidReportedName)
	}
	if !utf8.ValidString(name) || len(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: expected valid UTF-8 of at most 128 bytes without control characters", ErrInvalidReportedName)
	}
	return name, nil
}
