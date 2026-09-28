package store

import (
	"testing"
	"time"
)

// The sizing chains in store.go are arithmetic over measured facts, so these
// tests assert the consts rather than the package vars the deadline regression
// tests shrink: the design is what is being pinned, not a test's knob.

// TestMirrorHoldBudgetExceedsObservedCloneCost checks the budget against the
// cost of the work it bounds. A check that the budget fits inside the
// foreground's retry would pass a budget of one second.
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

// TestMirrorHoldBudgetIsAHoldNotARoundTrip pins that the mirror's hold on the
// live store is a local clone, so its budget must sit far under the network
// round trip. A hold budget that could accommodate a push would mean the push
// had crept under the hold — a wait every co-resident command would pay.
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

// TestCoResidentWaitIsSizedToTheMirrorHold pins the relation between the wait
// and the hold. A foreground write open waits out a co-resident holder for
// coResidentHolderWait, and the holder it is sized for is the mirror — so the
// wait has to outlast the mirror's longest LEGAL hold. That hold is not the
// budget: a cut does not land on its deadline, so a wait sized against the
// budget alone is sized against a number the hold does not respect.
//
// The other half of the same relation is the ticket's contract
// (links-scale-om3r.zhq): a command that cannot get the store fails within
// five seconds. The wait, plus one poll and one of dolt's own 100ms attempts
// (a refusal can land that long after the wait elapses), stays inside it.
func TestCoResidentWaitIsSizedToTheMirrorHold(t *testing.T) {
	t.Parallel()
	if coResidentHolderWait <= mirrorHoldCeiling {
		t.Fatalf("coResidentHolderWait (%s) does not outlast mirrorHoldCeiling (%s); a foreground open can starve against a legal mirror hold and fail as though the workspace were wedged",
			coResidentHolderWait, mirrorHoldCeiling)
	}
	if mirrorHoldCeiling <= mirrorHoldBudget {
		t.Fatalf("mirrorHoldCeiling (%s) does not exceed mirrorHoldBudget (%s); the ceiling exists because a cut does not land on its deadline, and a ceiling equal to the budget restates the budget instead of correcting it",
			mirrorHoldCeiling, mirrorHoldBudget)
	}
	if refusal := coResidentHolderWait + 2*storeLockPollInterval; refusal >= 5*time.Second {
		t.Fatalf("a contender's refusal lands after %s; the contract is that a command that cannot get the store fails within five seconds", refusal)
	}
}

// TestCommitLockWaiterOutlastsARotation pins the order the package's tolerated
// lock inversion depends on: a mutation's GC-contention rotation re-opens
// LOCK under the held commit lock, and a peer that took LOCK in the gap waits
// on that commit lock. The re-open gives up after coResidentHolderWait plus
// the old engine's close; the peer's wait must be strictly longer, or both
// fail at once where the holder alone was meant to.
func TestCommitLockWaiterOutlastsARotation(t *testing.T) {
	t.Parallel()
	if commitLockWaiterBudget() <= rotationReserve() {
		t.Fatalf("commitLockWaiterBudget (%s) does not outlast rotationReserve (%s); a commit-lock waiter gives up before the holder's rotation does",
			commitLockWaiterBudget(), rotationReserve())
	}
}
