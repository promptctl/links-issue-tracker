package store

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// priorityProbe is a representative spread across the int domain: below-range,
// the two canonical values, legacy 5-level values (2..4), and far out-of-range.
// The contracts below must hold for every one of them, not just the happy pair.
var priorityProbe = []int{-7, -1, model.PriorityNormal, model.PriorityUrgent, 2, 3, 4, 99}

// The single invariant this whole ticket exists to guarantee: the live validator
// and the import-boundary normalizer read "what is a legal priority" from the
// same authority, so they cannot disagree about the domain. Concretely,
// validatePriority accepts a value iff that value is a fixed point of
// canonicalPriority (i.e. canonicalPriority would not rewrite it).
func TestValidatorAcceptsExactlyCanonicalFixedPoints(t *testing.T) {
	for _, p := range priorityProbe {
		accepted := validatePriority(p) == nil
		isFixedPoint := canonicalPriority(p) == p
		if accepted != isFixedPoint {
			t.Errorf("priority %d: validatePriority accepts=%v but canonicalPriority fixed-point=%v; the live and import decisions have drifted", p, accepted, isFixedPoint)
		}
	}
}

// The import path coerces rather than rejects; whatever it produces must always
// be a value the live validator would accept, or a restored row could carry a
// priority the live write path forbids.
func TestCanonicalizedPriorityAlwaysPassesValidator(t *testing.T) {
	for _, p := range priorityProbe {
		if err := validatePriority(canonicalPriority(p)); err != nil {
			t.Errorf("priority %d canonicalized to %d which the validator rejects: %v", p, canonicalPriority(p), err)
		}
	}
}

// canonicalPriority is the domain authority only if it is idempotent — its range
// must equal its set of fixed points, which is what makes the validator's
// fixed-point check a faithful test of domain membership.
func TestCanonicalPriorityIsIdempotent(t *testing.T) {
	for _, p := range priorityProbe {
		once := canonicalPriority(p)
		if twice := canonicalPriority(once); twice != once {
			t.Errorf("priority %d: canonicalPriority not idempotent (%d -> %d -> %d)", p, p, once, twice)
		}
	}
}

// Restore tolerance the import boundary must preserve: legacy 5-level exports
// (2..4) coerce to normal so a restore is never rejected, while urgent and
// normal survive unchanged.
func TestCanonicalPriorityPreservesRestoreTolerance(t *testing.T) {
	cases := map[int]int{
		model.PriorityNormal: model.PriorityNormal,
		model.PriorityUrgent: model.PriorityUrgent,
		2:                    model.PriorityNormal,
		3:                    model.PriorityNormal,
		4:                    model.PriorityNormal,
	}
	for in, want := range cases {
		if got := canonicalPriority(in); got != want {
			t.Errorf("canonicalPriority(%d) = %d, want %d", in, got, want)
		}
	}
}
