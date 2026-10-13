package model

import (
	"strings"
)

// RelationType is the sealed set of edge types the relations graph carries.
// Every type reads as the sentence "<src> <type> <dst>": a blocked-by edge is
// "<waiting ticket> blocked-by <blocker>", a parent-child edge is
// "<child> parent-child <parent>", and related-to is symmetric. The store, the
// export, and every command name an edge in that one order, so no layer flips
// endpoints on the way through. [LAW:one-source-of-truth]
// [LAW:types-are-the-program] Every legal edge type is a named constant and
// nothing else is constructible through ParseRelationType, so downstream code
// matches on the constants instead of re-validating strings. Values originate
// from ParseRelationType at trust boundaries or from the constants; raw
// conversions are reserved for salvage paths that must conserve bytes.
type RelationType string

const (
	RelBlockedBy   RelationType = "blocked-by"
	RelParentChild RelationType = "parent-child"
	RelRelatedTo   RelationType = "related-to"
)

// RelationTypeFromStored reads a relation type as an older store or export
// wrote it. Before schema v8 and export version 3 the blocked-by edge was spelled
// "blocks", with the same orientation — src waited for dst — so the rename is
// the whole conversion. Every other spelling passes through unchanged: this is
// a reader of legacy bytes, not a trust boundary, and what it passes through is
// still checked where it is written.
func RelationTypeFromStored(s string) RelationType {
	if s == legacyBlockedBySpelling {
		return RelBlockedBy
	}
	return RelationType(s)
}

const legacyBlockedBySpelling = "blocks"

// ParseRelationType maps an untrusted relation-type string (CLI flag, import
// payload) into the sealed set.
// [LAW:single-enforcer] The only string-to-RelationType gate; every trust
// boundary calls this instead of carrying its own equality chain.
func ParseRelationType(s string) (RelationType, error) {
	switch rt := RelationType(strings.TrimSpace(s)); rt {
	case RelBlockedBy, RelParentChild, RelRelatedTo:
		return rt, nil
	default:
		return "", ValidationError{Message: "relation type must be blocked-by, parent-child, or related-to"}
	}
}

// SingleValuedFromSrc reports whether a src endpoint may have at most one edge
// of this type — the type's outgoing cardinality. parent-child is single-valued
// (a child has at most one parent); blocked-by and related-to are many-valued. The
// rule lives on the type so every write boundary enforces it by asking the type,
// not by which store method the caller happened to invoke.
// [LAW:types-are-the-program] Single-parent cardinality is a property of the
// relation type, which makes a two-parent child unrepresentable at the boundary.
func (rt RelationType) SingleValuedFromSrc() bool {
	return rt == RelParentChild
}

// CanonicalEndpoints returns the endpoint pair in storage-canonical order.
// related-to is undirected, so its endpoints are stored sorted to give each
// pair exactly one representation; directed types pass through unchanged.
// [LAW:one-source-of-truth] The undirected-edge normalization lives here only.
func (rt RelationType) CanonicalEndpoints(src, dst string) (string, string) {
	if rt == RelRelatedTo && dst < src {
		return dst, src
	}
	return src, dst
}
