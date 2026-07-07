package model

import (
	"errors"
	"strings"
)

// IssueType is the sealed set of issue kinds an issue record can carry.
// [LAW:types-are-the-program] Every legal kind is a named constant and nothing
// else is constructible through ParseIssueType, so downstream code matches on
// the constants instead of re-validating strings. Values originate from
// ParseIssueType at trust boundaries or from the constants; raw conversions
// are reserved for salvage paths that must conserve bytes the DB CHECK
// constraint already vouches for.
type IssueType string

const (
	TypeTask    IssueType = "task"
	TypeFeature IssueType = "feature"
	TypeBug     IssueType = "bug"
	TypeChore   IssueType = "chore"
	TypeEpic    IssueType = "epic"
)

// IssueTypes returns the canonical enumeration in canonical order.
// [LAW:one-source-of-truth] Issue-type vocabulary lives here; the parse gate,
// help text, matrix tests, and the schema CHECK clauses all derive from this
// list rather than repeating it. A fresh slice per call keeps the
// authoritative storage unreachable, so the sealed set cannot be widened at
// runtime the way an exported slice variable could. [LAW:types-are-the-program]
func IssueTypes() []IssueType {
	return []IssueType{TypeTask, TypeFeature, TypeBug, TypeChore, TypeEpic}
}

var errInvalidIssueType = errors.New("issue type must be " + oxfordOr(IssueTypes()))

// ParseIssueType maps an untrusted issue-type string (CLI flag, query token,
// import payload) into the sealed set, canonicalizing case and surrounding
// whitespace.
// [LAW:single-enforcer] The only string-to-IssueType gate; every trust
// boundary calls this instead of carrying its own membership scan.
func ParseIssueType(s string) (IssueType, error) {
	t := IssueType(strings.ToLower(strings.TrimSpace(s)))
	for _, valid := range IssueTypes() {
		if t == valid {
			return t, nil
		}
	}
	return "", errInvalidIssueType
}

// IsContainer reports whether the type uses container-style lifecycle: state
// derived from children, no leaf status primitive.
// [LAW:types-are-the-program] Container-ness is a property of the type, not a
// runtime lookup against a parallel set that could drift from the vocabulary.
func (t IssueType) IsContainer() bool {
	return t == TypeEpic
}

// ContainerTypes returns the subset of IssueTypes that use container-style
// lifecycle, in canonical order. Derived from the IsContainer predicate so the
// set and the predicate cannot disagree. [LAW:one-source-of-truth]
func ContainerTypes() []IssueType {
	var out []IssueType
	for _, t := range IssueTypes() {
		if t.IsContainer() {
			out = append(out, t)
		}
	}
	return out
}

// oxfordOr renders the vocabulary as prose ("a, b, or c") for the parse error,
// so the message the user sees names exactly the sealed set.
func oxfordOr(types []IssueType) string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}
