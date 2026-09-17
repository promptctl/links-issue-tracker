// Package lawtokens is the in-repo authority for the architectural-law marker
// convention. Throughout this tree, a decision shaped by an architectural law
// is cited inline as a marker — "[LAW:single-enforcer]", "[FRAMING:representation]"
// — and that convention is the codebase's machine-greppable self-audit
// substrate and its contract with future agents.
//
// The set of legal tokens (the "Token index") is owned upstream, by the code
// skill of the universal-laws plugin (UpstreamIndexURL). This package does not
// transcribe it by hand: tools/lawtokens-sync fetches that document, parses
// the index, and writes canonical_gen.go, so the in-repo copy is generated
// output with a checkable source. The nightly workflow runs the same tool with
// -check and fails when the upstream index and the generated file differ.
//
// [LAW:one-source-of-truth] the upstream index is the authority for which
// markers are legal; Canonical is its derived copy, and every consumer in this
// tree (the repo gate test here today, a pre-commit hook or `lit lint`
// tomorrow) reads Canonical rather than minting a second list.
package lawtokens

import "sort"

// Canonical is the set of legal markers, keyed by the full "NAMESPACE:token"
// string a citation carries between its brackets, built from the generated
// canonicalKeys. Do not add a token here to make code compile: a marker the
// upstream index does not contain is drift, and the gate is meant to catch it.
//
// Keying by the full "NAMESPACE:token" (not the bare token) is load-bearing: it
// makes a right-token/wrong-namespace citation — one pairing the LAW namespace
// with `representation`, which is a FRAMING idea, not a LAW — unrepresentable as
// canonical, because only the `FRAMING:representation` key is a member.
//
// This doc names example tokens in backticks rather than in bracketed marker
// form on purpose: the gate scans this file too, so the only bracketed
// citations it may legally contain are the real canonical ones cited inline,
// never a non-canonical example.
var Canonical = newMarkerSet(canonicalKeys...)

// markerSet is a membership-only set over canonical "NAMESPACE:token" keys.
type markerSet map[string]struct{}

func newMarkerSet(keys ...string) markerSet {
	s := make(markerSet, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

// Has reports whether key (a full "NAMESPACE:token" string) is canonical.
func (s markerSet) Has(key string) bool {
	_, ok := s[key]
	return ok
}

// Sorted returns the canonical keys in a stable order, for diagnostics.
func (s markerSet) Sorted() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
