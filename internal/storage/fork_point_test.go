package storage

import "testing"

// TestOnlyARealBaseIsFound checks that Found reports a base for the two
// sources that carry one, and none for none or a zero value — the base of a
// freshness that never synced.
func TestOnlyARealBaseIsFound(t *testing.T) {
	for _, tc := range []struct {
		base  ReconcileBase
		found bool
	}{
		{ReconcileBase{Source: ReconcileBaseMergeBase, Commit: "c"}, true},
		{ReconcileBase{Source: ReconcileBaseForkPoint, Commit: "c"}, true},
		{ReconcileBase{Source: ReconcileBaseNone}, false},
		{ReconcileBase{}, false},
	} {
		if _, found := tc.base.Found(); found != tc.found {
			t.Errorf("%+v.Found() = %v, want %v", tc.base, found, tc.found)
		}
	}
}
