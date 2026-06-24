package model

import (
	"errors"
	"strings"
)

// RelationType is the sealed set of edge types the relations graph carries.
// [LAW:types-are-the-program] Every legal edge type is a named constant and
// nothing else is constructible through ParseRelationType, so downstream code
// matches on the constants instead of re-validating strings. Values originate
// from ParseRelationType at trust boundaries or from the constants; raw
// conversions are reserved for salvage paths that must conserve bytes.
type RelationType string

const (
	RelBlocks      RelationType = "blocks"
	RelParentChild RelationType = "parent-child"
	RelRelatedTo   RelationType = "related-to"
)

// ParseRelationType maps an untrusted relation-type string (CLI flag, import
// payload) into the sealed set.
// [LAW:single-enforcer] The only string-to-RelationType gate; every trust
// boundary calls this instead of carrying its own equality chain.
func ParseRelationType(s string) (RelationType, error) {
	switch rt := RelationType(strings.TrimSpace(s)); rt {
	case RelBlocks, RelParentChild, RelRelatedTo:
		return rt, nil
	default:
		return "", errors.New("relation type must be blocks, parent-child, or related-to")
	}
}

// StoreEndpoints maps an endpoint pair between the CLI's human order and the
// store's canonical orientation. blocks is stored dependent->dependency, the
// reverse of the human "<blocker> blocks <blocked>" reading; the swap is an
// involution, so the same call converts store order back to display order.
// [LAW:one-source-of-truth] The blocks orientation rule lives here only.
func (rt RelationType) StoreEndpoints(from, to string) (string, string) {
	if rt == RelBlocks {
		return to, from
	}
	return from, to
}

// SingleValuedFromSrc reports whether a src endpoint may have at most one edge
// of this type — the type's outgoing cardinality. parent-child is single-valued
// (a child has at most one parent); blocks and related-to are many-valued. The
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
