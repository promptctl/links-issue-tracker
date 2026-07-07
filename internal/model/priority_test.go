package model

import "testing"

// priorityProbe is a representative spread across the int domain: below-range,
// the two canonical values, legacy 5-level values (2..4), and far out-of-range.
// The contracts below must hold for every one of them, not just the happy pair.
var priorityProbe = []int{-7, -1, int(PriorityNormal), int(PriorityUrgent), 2, 3, 4, 99}

// The single invariant the sealed type exists to guarantee: the strict gate
// and the salvage normalizer read "what is a legal priority" from the same
// authority, so they cannot disagree about the domain. Concretely,
// ParsePriority accepts a value iff that value is a fixed point of
// CanonicalPriority (i.e. CanonicalPriority would not rewrite it).
func TestParsePriorityAcceptsExactlyCanonicalFixedPoints(t *testing.T) {
	for _, p := range priorityProbe {
		_, err := ParsePriority(p)
		accepted := err == nil
		isFixedPoint := int(CanonicalPriority(p)) == p
		if accepted != isFixedPoint {
			t.Errorf("priority %d: ParsePriority accepts=%v but CanonicalPriority fixed-point=%v; the strict and salvage decisions have drifted", p, accepted, isFixedPoint)
		}
	}
}

// The salvage path coerces rather than rejects; whatever it produces must
// always be a value the strict gate would accept, or a restored row could
// carry a priority the live write path forbids.
func TestCanonicalizedPriorityAlwaysParses(t *testing.T) {
	for _, p := range priorityProbe {
		if _, err := ParsePriority(int(CanonicalPriority(p))); err != nil {
			t.Errorf("priority %d canonicalized to %d which the parse gate rejects: %v", p, CanonicalPriority(p), err)
		}
	}
}

// CanonicalPriority is the domain authority only if it is idempotent — its
// range must equal its set of fixed points, which is what makes the parse
// gate's fixed-point check a faithful test of domain membership.
func TestCanonicalPriorityIsIdempotent(t *testing.T) {
	for _, p := range priorityProbe {
		once := CanonicalPriority(p)
		if twice := CanonicalPriority(int(once)); twice != once {
			t.Errorf("priority %d: CanonicalPriority not idempotent (%d -> %d -> %d)", p, p, once, twice)
		}
	}
}

// Restore tolerance the salvage boundary must preserve: legacy 5-level exports
// (2..4) coerce to normal so a restore is never rejected, while urgent and
// normal survive unchanged.
func TestCanonicalPriorityPreservesRestoreTolerance(t *testing.T) {
	cases := map[int]Priority{
		int(PriorityNormal): PriorityNormal,
		int(PriorityUrgent): PriorityUrgent,
		2:                   PriorityNormal,
		3:                   PriorityNormal,
		4:                   PriorityNormal,
	}
	for in, want := range cases {
		if got := CanonicalPriority(in); got != want {
			t.Errorf("CanonicalPriority(%d) = %d, want %d", in, got, want)
		}
	}
}

// The display names are part of the CLI's observable output contract.
func TestPriorityString(t *testing.T) {
	if got := PriorityNormal.String(); got != "normal" {
		t.Errorf("PriorityNormal.String() = %q, want %q", got, "normal")
	}
	if got := PriorityUrgent.String(); got != "urgent" {
		t.Errorf("PriorityUrgent.String() = %q, want %q", got, "urgent")
	}
}
