package store

import (
	"testing"
	"time"
)

// TestMirrorHoldBudgetFitsInsideOpenRetryBudget pins the sizing relation the
// two budgets are designed around: a foreground write open retries against a
// co-resident holder for engineOpenRetryMaxElapsed, so the longest hold the
// background mirror may impose (MirrorHoldBudget) must fit inside it with
// headroom for the mirror's engine close and the waiter's retry interval —
// otherwise a foreground command that arrives as a mirror cycle begins can
// exhaust its open budget against a hold that is, by design, still legal.
// [LAW:verifiable-goals] the relation is a number, so it is checked, not prose.
func TestMirrorHoldBudgetFitsInsideOpenRetryBudget(t *testing.T) {
	t.Parallel()
	const headroom = 5 * time.Second
	if MirrorHoldBudget+headroom > engineOpenRetryMaxElapsed {
		t.Fatalf("MirrorHoldBudget (%s) + %s headroom exceeds engineOpenRetryMaxElapsed (%s); a foreground open can starve against a legal mirror hold",
			MirrorHoldBudget, headroom, engineOpenRetryMaxElapsed)
	}
}
