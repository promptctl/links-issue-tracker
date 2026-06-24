package lawtokens

import (
	"regexp"
	"strings"
)

// Marker is one architectural-law citation found in some text: the namespace
// ("LAW" or "FRAMING"), the token between the colon and the closing bracket,
// and the 1-based line it sits on.
type Marker struct {
	Namespace string
	Token     string
	Line      int
}

// Key is the "NAMESPACE:token" string used to test membership in Canonical.
func (m Marker) Key() string {
	return m.Namespace + ":" + m.Token
}

// String renders the marker as it appears in source, "[NAMESPACE:token]".
func (m Marker) String() string {
	return "[" + m.Key() + "]"
}

// markerPattern matches a citation by its SHAPE, not by a fixed token list:
// "[", a namespace, ":", then everything up to the first "]" on the line. The
// token is captured loosely (any run of non-"]" characters) on purpose — a
// miscased or malformed token (say `No-Silent-Failure` rather than the
// canonical lowercase `no-silent-failure`) must still be *recognized* as a
// marker so it can be reported as non-canonical, rather than silently failing
// to match and riding in unflagged ([LAW:no-silent-failure]). Canonicity is
// then decided by exact membership in Canonical, not by the regex.
var markerPattern = regexp.MustCompile(`\[(LAW|FRAMING):([^\]\n]+)\]`)

// ScanMarkers returns every architectural-law citation in content, in order,
// each tagged with its 1-based line number. It is a pure function of content:
// no IO, no globals — so the gate can feed it real files and the unit tests can
// feed it synthetic strings ([LAW:effects-at-boundaries]).
func ScanMarkers(content string) []Marker {
	var markers []Marker
	for i, line := range strings.Split(content, "\n") {
		for _, m := range markerPattern.FindAllStringSubmatch(line, -1) {
			markers = append(markers, Marker{
				Namespace: m[1],
				Token:     m[2],
				Line:      i + 1,
			})
		}
	}
	return markers
}

// NonCanonical returns the subset of markers whose key is absent from Canonical.
func NonCanonical(markers []Marker) []Marker {
	var bad []Marker
	for _, m := range markers {
		if !Canonical.Has(m.Key()) {
			bad = append(bad, m)
		}
	}
	return bad
}
