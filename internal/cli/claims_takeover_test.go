package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestRelationOf is the gate's predicate stated as its own contract: every
// standing claims.Derive can build, read against the two identities a caller
// can have, and the relation each pair yields. The takeover gate and routing
// both consume this value, so what it pins is what both of them do.
// [LAW:behavior-not-structure]
//
// There is no row for an expired claim because there is no standing for one:
// the derivation returns Unclaimed the moment the window closes, so this
// function never learns a claim existed and cannot treat it as a grade of
// hold. That absence is the ruling of links-claims-y6yz, and it is pinned
// where a reader would go looking for the missing row.
func TestRelationOf(t *testing.T) {
	tests := []struct {
		name     string
		standing claims.Standing
		self     model.Attribution
		want     laneRelation
	}{
		{"unclaimed is nobody's", claims.Unclaimed{}, selfAttribution, laneUnclaimed},
		{"a nil standing reads as unclaimed, never as a panic", nil, selfAttribution, laneUnclaimed},
		{"held by self is ours", heldBy(selfAttribution), selfAttribution, laneOurs},
		{"held by someone else is foreign", heldBy(otherAttribution), selfAttribution, laneHeldForeign},
		// The public checkout is every unattributed writer at once, and a
		// checkout with no minted token has recorded nothing, so a zero self
		// equal to a zero holder proves nothing about whose lane it is.
		{"held by the public checkout is foreign even to an unminted self", heldBy(publicAttribution), publicAttribution, laneHeldForeign},
		{"held by the public checkout is foreign to a minted self", heldBy(publicAttribution), selfAttribution, laneHeldForeign},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relationOf(tc.standing, tc.self); got != tc.want {
				t.Errorf("relationOf(%#v, %v) = %v, want %v", tc.standing, tc.self, got, tc.want)
			}
		})
	}
}
