package store

import (
	"testing"
	"time"
)

// The sizing chain in store.go is arithmetic over two measured facts, so these
// tests assert the consts rather than the package vars the deadline regression
// tests shrink: the design is what is being pinned, not a test's knob.

// TestMirrorHoldBudgetExceedsObservedCycleCost is the regression pin for
// links-sync-dauk. The retired test asserted only that the budget fit inside
// the foreground's retry, which a budget of one second would also satisfy —
// nothing anywhere checked the budget against the cost of the work it bounds,
// so a 20s budget sat below the measured p90 of a mirror cycle, cut 15.8% of
// them, and reported each one as a remote-transport fault.
//
// A hold budget is a stall detector. Its whole claim is that work still running
// at the deadline has stopped making progress, and that claim is false the
// moment the deadline lands inside the distribution of healthy runs. So the
// check is against mirrorCycleObservedTail — the slowest healthy cycle ever
// measured — and it demands real separation, not a hair above it.
// [LAW:verifiable-goals] the relation is a number, so it is checked, not prose.
func TestMirrorHoldBudgetExceedsObservedCycleCost(t *testing.T) {
	t.Parallel()
	if mirrorHoldStallFactor < 2 {
		t.Fatalf("mirrorHoldStallFactor is %d; a budget under twice the slowest healthy cycle fires on healthy cycles, which is the defect links-sync-dauk filed",
			mirrorHoldStallFactor)
	}
	if mirrorHoldBudget <= mirrorCycleObservedTail {
		t.Fatalf("mirrorHoldBudget (%s) does not exceed mirrorCycleObservedTail (%s); the budget is sized inside the cost of the work it bounds, so healthy cycles are cut and reported as transport faults",
			mirrorHoldBudget, mirrorCycleObservedTail)
	}
}

// TestCoResidentWaitOutlastsMirrorHoldCeiling pins the relation the retired
// test got wrong. A foreground write open waits out a co-resident holder for
// coResidentHolderWait, and the holder it was sized for is the mirror — so the
// wait has to outlast the mirror's longest LEGAL hold. That hold is not the
// budget: measured over 44 cut cycles, cancellation lands up to 21.4s after the
// deadline, so a wait sized against the budget plus a guessed 5s of headroom
// was short of the real ceiling by four times its own headroom, and a
// foreground command could exhaust its open budget against a mirror doing
// exactly what it was designed to do.
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
