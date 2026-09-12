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

		// The three worktree states an expired foreign claim can be in. The
		// clock has run out identically in all three; only what this machine
		// can see of the holder differs, and that is what decides the gate.
		{
			"stale, holder's worktree gone from this machine, proceeds informed",
			claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: claims.Gone},
			takeoverStaleInformed,
		},
		{
			// Presence alone must NOT gate: a worktree routinely outlives the
			// session that made it, so requiring --take here would make every
			// uncleaned tree an unclaimable lane and defeat the age-out.
			"stale, holder's worktree merely present, still proceeds informed",
			claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: claims.Present},
			takeoverStaleInformed,
		},
		{
			// The ticket's headline: `git worktree lock` is the holder's own
			// do-not-disturb, so the lane is gated exactly as a fresh claim is.
			"stale, holder's worktree locked, demands a deliberate act",
			claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: claims.Locked},
			takeoverFreshConfirm,
		},
		{
			// A lock on our OWN lane is still our lane. Staleness there is
			// evidence we stepped away from work that remains ours, and being
			// made to pass --take to resume it would be the prompt the design
			// promises never to show on the happy path.
			"stale and locked, but ours, needs no ceremony",
			claims.Stale{Tenure: claims.Tenure{By: selfAttribution}, Holder: claims.Locked},
			takeoverNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTakeover(tc.standing, selfAttribution); got != tc.want {
				t.Errorf("classifyTakeover(%#v, self) = %v, want %v", tc.standing, got, tc.want)
			}
		})
	}
}
