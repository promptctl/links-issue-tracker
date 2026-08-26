package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
)

// TestClassifyTakeover is the predicate's own contract: it holds the pure
// function's input fixed to the five standings a lane can report and checks
// the takeover requirement design-docs/work-claims.md's "Release and
// abandonment" section names for each, independent of any CLI plumbing.
// [LAW:behavior-not-structure]
func TestClassifyTakeover(t *testing.T) {
	tests := []struct {
		name     string
		standing claims.Standing
		want     takeoverRequirement
	}{
		{"unclaimed needs no ceremony", claims.Unclaimed{}, takeoverNone},
		{"held by self needs no ceremony", heldBy(selfAttribution), takeoverNone},
		{"held by someone else demands a deliberate act", heldBy(otherAttribution), takeoverFreshConfirm},
		{"stale, still self, needs no ceremony", claims.Stale{Tenure: claims.Tenure{By: selfAttribution}}, takeoverNone},
		{"stale, held by someone else, proceeds informed", claims.Stale{Tenure: claims.Tenure{By: otherAttribution}}, takeoverStaleInformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTakeover(tc.standing, selfAttribution); got != tc.want {
				t.Errorf("classifyTakeover(%#v, self) = %v, want %v", tc.standing, got, tc.want)
			}
		})
	}
}
