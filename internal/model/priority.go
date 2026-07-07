package model

import "fmt"

// Priority is the sealed two-level priority domain.
// [LAW:types-are-the-program] Every legal priority is a named constant and
// nothing else is constructible through ParsePriority, so downstream code
// assumes validity instead of re-checking a raw int. Values originate from
// ParsePriority at trust boundaries or from the constants; raw conversions
// are reserved for salvage paths that must conserve values the DB CHECK
// constraint already vouches for.
type Priority int

const (
	PriorityNormal Priority = 0
	PriorityUrgent Priority = 1
)

// CanonicalPriority is the single authority on the priority domain: it maps
// any raw int onto the canonical {normal, urgent} set and is idempotent, so
// its fixed points ARE the legal priorities. ParsePriority (live writes) and
// the import boundary (legacy restores) are both defined in terms of it, so
// they cannot disagree about what a legal priority is — extending the domain
// is a one-function edit here. [LAW:one-source-of-truth] [LAW:single-enforcer]
func CanonicalPriority(v int) Priority {
	if Priority(v) == PriorityUrgent {
		return PriorityUrgent
	}
	return PriorityNormal
}

// ParsePriority maps an untrusted priority int (CLI flag, import payload)
// into the sealed set, rejecting exactly the values CanonicalPriority would
// rewrite — i.e. anything that is not already canonical. Live write
// boundaries reject where salvage paths coerce, but both read "what is a
// legal priority" from CanonicalPriority, so the two resolutions stay in
// lockstep. [LAW:single-enforcer]
func ParsePriority(v int) (Priority, error) {
	p := CanonicalPriority(v)
	if int(p) != v {
		return 0, fmt.Errorf("priority must be %d (normal) or %d (urgent)", int(PriorityNormal), int(PriorityUrgent))
	}
	return p, nil
}

// String renders the priority's display name.
func (p Priority) String() string {
	if p == PriorityUrgent {
		return "urgent"
	}
	return "normal"
}
