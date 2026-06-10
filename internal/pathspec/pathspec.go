// Package pathspec carries a possibly-absent filesystem path as a value.
//
// [LAW:types-are-the-program] The theorem PathSpec states: the value is
// whitespace-trimmed and absence is the zero value. Trimming happens exactly
// once, in New ([LAW:single-enforcer]); everything downstream assumes it and
// never re-trims. Absence flows through Or and Join as data rather than being
// re-guarded at each consumer ([LAW:dataflow-not-control-flow]).
package pathspec

import (
	"path/filepath"
	"strings"
)

// PathSpec is a filesystem path normalized at construction. The zero value
// is the absent path.
type PathSpec struct {
	value string
}

// New trims raw; whitespace-only input yields the absent PathSpec.
func New(raw string) PathSpec {
	return PathSpec{value: strings.TrimSpace(raw)}
}

// String returns the trimmed path, or "" when absent.
func (p PathSpec) String() string {
	return p.value
}

// IsEmpty reports whether the path is absent.
func (p PathSpec) IsEmpty() bool {
	return p.value == ""
}

// Or returns p when present, fallback otherwise.
func (p PathSpec) Or(fallback PathSpec) PathSpec {
	if p.value == "" {
		return fallback
	}
	return p
}

// Join appends path elements to a present path; an absent path stays absent.
// [LAW:dataflow-not-control-flow] filepath.Join("", x) would invent a relative
// path from nothing — absence must propagate, not mutate into a value.
func (p PathSpec) Join(elem ...string) PathSpec {
	if p.value == "" {
		return p
	}
	return PathSpec{value: filepath.Join(append([]string{p.value}, elem...)...)}
}
