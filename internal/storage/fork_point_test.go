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

// TestBehindIsNothingLocalPastTheRemoteWhateverTheBase checks State apart
// from the commit questions: a branch with nothing to replay is behind under
// either base, but only a base the heads share means the remote carries the
// local head's commit, and under a fork-point base the commits diverge.
func TestBehindIsNothingLocalPastTheRemoteWhateverTheBase(t *testing.T) {
	shared := ReconcileBase{Source: ReconcileBaseMergeBase, Commit: "c"}
	forked := ReconcileBase{Source: ReconcileBaseForkPoint, Commit: "c"}
	unrelated := ReconcileBase{Source: ReconcileBaseNone}
	for _, tc := range []struct {
		fresh             SyncFreshness
		state             SyncFreshnessState
		carries, diverged bool
	}{
		{SyncFreshness{}, SyncNeverSynced, false, false},
		{SyncFreshness{Synced: true, Base: shared}, SyncUpToDate, true, false},
		{SyncFreshness{Synced: true, Behind: 2, Base: shared}, SyncBehind, true, false},
		{SyncFreshness{Synced: true, Ahead: 1, Base: shared}, SyncAhead, false, false},
		{SyncFreshness{Synced: true, Ahead: 1, Behind: 2, Base: shared}, SyncDiverged, false, true},
		{SyncFreshness{Synced: true, Behind: 2, Base: forked}, SyncBehind, false, true},
		{SyncFreshness{Synced: true, Ahead: 1, Behind: 2, Base: forked}, SyncDiverged, false, true},
		{SyncFreshness{Synced: true, Ahead: 3, Behind: 2, Base: unrelated}, SyncDiverged, false, true},
	} {
		state, carries, diverged := tc.fresh.State(), tc.fresh.RemoteCarriesHead(), tc.fresh.CommitsDiverged()
		if state != tc.state || carries != tc.carries || diverged != tc.diverged {
			t.Errorf("%+v: State() = %q, RemoteCarriesHead() = %v, CommitsDiverged() = %v; want %q, %v, %v", tc.fresh, state, carries, diverged, tc.state, tc.carries, tc.diverged)
		}
	}
}
