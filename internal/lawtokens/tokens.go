// Package lawtokens is the in-repo authority for the architectural-law marker
// convention. Throughout this tree, a decision shaped by an architectural law
// is cited inline as a marker — "[LAW:single-enforcer]", "[FRAMING:representation]"
// — and that convention is the codebase's machine-greppable self-audit
// substrate and its contract with future agents.
//
// The set of legal tokens (the "Token index") originates in a human-facing
// rubric that lives in global agent configuration, OUTSIDE any repository and
// unreadable by CI. Left there, the index enforces nothing: any author can coin
// a token and it rides into the tree unflagged until a human-directed cleanup.
// This package fixes that by making the index live IN-repo as machine-readable
// data — Canonical below — so a deterministic check can read it and fail loudly
// on drift.
//
// [LAW:one-source-of-truth] Canonical is the single in-repo authority for which
// markers are legal; every other consumer (the repo gate test here today, a
// pre-commit hook or `lit lint` tomorrow) derives from it rather than minting a
// second list.
package lawtokens

import "sort"

// Canonical is the set of legal markers, keyed by the full "NAMESPACE:token"
// string a citation carries between its brackets. It is transcribed verbatim
// from the universal-laws Token index. Two namespaces, deliberately kept small;
// do not coin new tokens here to make code compile — a marker the index does
// not contain is drift, and the gate is meant to catch it.
//
// Keying by the full "NAMESPACE:token" (not the bare token) is load-bearing: it
// makes a right-token/wrong-namespace citation — e.g. "[LAW:representation]",
// where representation is a FRAMING idea, not a LAW — unrepresentable as
// canonical, because only "FRAMING:representation" is a member.
var Canonical = newMarkerSet(
	// Framing — higher-level ideas referenced in reasoning, rarely cited in code.
	"FRAMING:parts-and-seams",
	"FRAMING:representation",

	// Laws — cited at the callsite.
	"LAW:decomposition",
	"LAW:types-are-the-program",
	"LAW:composability",
	"LAW:carrying-cost",
	"LAW:no-ambient-temporal-coupling",
	"LAW:effects-at-boundaries",
	"LAW:one-source-of-truth",
	"LAW:single-enforcer",
	"LAW:comments-explain-why-only",
	"LAW:dataflow-not-control-flow",
	"LAW:one-type-per-behavior",
	"LAW:no-mode-explosion",
	"LAW:no-defensive-null-guards",
	"LAW:locality-or-seam",
	"LAW:one-way-deps",
	"LAW:no-shared-mutable-globals",
	"LAW:verifiable-goals",
	"LAW:behavior-not-structure",
	"LAW:no-silent-failure",
)

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
