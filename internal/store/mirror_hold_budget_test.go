package store

import (
	"testing"
	"time"
)

// The sizing chains in store.go are arithmetic over measured facts, so these
// tests assert the consts rather than the package vars the deadline regression
// tests shrink: the design is what is being pinned, not a test's knob.

// TestMirrorHoldBudgetExceedsObservedCloneCost is the regression pin for
// links-sync-dauk, carried onto the hold the mirror now takes. The retired test
// asserted only that the budget fit inside the foreground's retry, which a
// budget of one second would also satisfy — nothing anywhere checked the budget
// against the cost of the work it bounds, so a 20s budget sat below the
// measured p90 of a mirror cycle, cut 15.8% of them, and reported each one as a
// remote-transport fault.
//
// A hold budget is a stall detector. Its whole claim is that work still running
// at the deadline has stopped making progress, and that claim is false the
// moment the deadline lands inside the distribution of healthy runs. So the
// check is against mirrorCloneObservedTail — the slowest healthy clone step
// ever measured — and it demands real separation, not a hair above it.
// [LAW:verifiable-goals] the relation is a number, so it is checked, not prose.
func TestMirrorHoldBudgetExceedsObservedCloneCost(t *testing.T) {
	t.Parallel()
	if mirrorHoldStallFactor < 2 {
		t.Fatalf("mirrorHoldStallFactor is %d; a budget under twice the slowest healthy hold fires on healthy holds, which is the defect links-sync-dauk filed",
			mirrorHoldStallFactor)
	}
	if mirrorHoldBudget <= mirrorCloneObservedTail {
		t.Fatalf("mirrorHoldBudget (%s) does not exceed mirrorCloneObservedTail (%s); the budget is sized inside the cost of the work it bounds, so healthy holds are cut and reported as faults",
			mirrorHoldBudget, mirrorCloneObservedTail)
	}
}

// TestMirrorHoldBudgetIsAHoldNotARoundTrip pins what links-scale-om3r.s2h
// changed: the mirror's hold on the live store is a local clone, so its budget
// must sit far under the network round trip the mirror used to hold the store
// across. A hold budget that could accommodate a push would mean the push had
// crept back under the hold — the wait every co-resident command paid.
func TestMirrorHoldBudgetIsAHoldNotARoundTrip(t *testing.T) {
	t.Parallel()
	if mirrorHoldBudget >= mirrorPushObservedTail {
		t.Fatalf("mirrorHoldBudget (%s) is at or above the slowest healthy push (%s); the hold on the live store is a clone, not a round trip, and its budget must say so",
			mirrorHoldBudget, mirrorPushObservedTail)
	}
	if mirrorHoldCeiling >= time.Second*2 {
		t.Fatalf("mirrorHoldCeiling (%s) is not sub-two-second; the ticket's contract is a hold under one second on every healthy cycle, and a ceiling this high means the budget is not sized from the clone's measured cost",
			mirrorHoldCeiling)
	}
}

// TestMirrorPushDeadlineExceedsObservedPushCost pins the push deadline against
// the operation it bounds, exactly as the hold budget is pinned against the
// clone: the push holds nothing on the live store, but a deadline inside the
// distribution of healthy pushes still cuts healthy pushes and reports each as
// a transport fault.
func TestMirrorPushDeadlineExceedsObservedPushCost(t *testing.T) {
	t.Parallel()
	if mirrorPushStallFactor < 2 {
		t.Fatalf("mirrorPushStallFactor is %d; a deadline under twice the slowest healthy push fires on healthy pushes",
			mirrorPushStallFactor)
	}
	if mirrorPushDeadline <= mirrorPushObservedTail {
		t.Fatalf("mirrorPushDeadline (%s) does not exceed mirrorPushObservedTail (%s); the deadline is sized inside the cost of the work it bounds",
			mirrorPushDeadline, mirrorPushObservedTail)
	}
}

// TestCoResidentWaitOutlastsMirrorHoldCeiling pins the relation the retired
// test got wrong. A foreground write open waits out a co-resident holder for
// coResidentHolderWait, and the holder it was sized for is the mirror — so the
// wait has to outlast the mirror's longest LEGAL hold. That hold is not the
// budget: a cut does not land on its deadline, so a wait sized against the
// budget alone is sized against a number the hold does not respect.
func TestCoResidentWaitOutlastsMirrorHoldCeiling(t *testing.T) {
	t.Parallel()
	if coResidentHolderWait <= mirrorHoldCeiling {
		t.Fatalf("coResidentHolderWait (%s) does not outlast mirrorHoldCeiling (%s); a foreground open can starve against a legal mirror hold and fail as though the workspace were wedged",
			coResidentHolderWait, mirrorHoldCeiling)
	}
	if mirrorHoldCeiling <= mirrorHoldBudget {
		t.Fatalf("mirrorHoldCeiling (%s) does not exceed mirrorHoldBudget (%s); the ceiling exists because a cut does not land on its deadline, and a ceiling equal to the budget restates the budget instead of correcting it",
			mirrorHoldCeiling, mirrorHoldBudget)
	}
	// The mirror is not the only routine holder while the inline receive and
	// the explicit push still run on the live store's locks: a wait sized
	// under either fails a write open against a peer's `lit show` or
	// `git push` doing exactly what it was designed to do.
	for _, holder := range []struct {
		name    string
		ceiling time.Duration
	}{
		{"inlineReceiveCeiling", inlineReceiveCeiling},
		{"foregroundPushObservedTail", foregroundPushObservedTail},
	} {
		if coResidentHolderWait <= holder.ceiling {
			t.Fatalf("coResidentHolderWait (%s) does not outlast %s (%s); a foreground open fails against a routine holder", coResidentHolderWait, holder.name, holder.ceiling)
		}
	}
}

// TestJournalRetryAttemptsReconstructCoResidentWait pins that the journal
// lock's retry loop waits the co-resident wait and not some rounding of it.
// The attempt count is a division, and a wait that is not a whole multiple of
// doltJournalRetryDelay truncates — silently, and always downward, which is
// the direction that starves. The two representations of this one wait
// (engineOpenRetryMaxElapsed, and delay × attempts) agree only while the
// division is exact. [LAW:one-source-of-truth]
func TestJournalRetryAttemptsReconstructCoResidentWait(t *testing.T) {
	t.Parallel()
	if got := time.Duration(doltJournalRetryAttempts) * doltJournalRetryDelay; got != coResidentHolderWait {
		t.Fatalf("doltJournalRetryAttempts (%d) × doltJournalRetryDelay (%s) = %s, want coResidentHolderWait (%s); the division truncated and the journal wait is now shorter than the engine-open wait for the same holder",
			doltJournalRetryAttempts, doltJournalRetryDelay, got, coResidentHolderWait)
	}
}
