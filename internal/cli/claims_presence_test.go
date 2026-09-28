package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// One rule: a word that asserts a row is abandoned may only be printed where
// nobody holds its lane. The row's own clock is identical in every case below —
// these tests move the lane's standing and nothing else.
//
// The lock is folded into the derivation — a locked holder is simply Held — so
// the rule here is stated against the standing.

// TestInProgressSuffixWithdrawsORPHANEDInAHeldLane covers `lit backlog`'s row
// suffix. ORPHANED is the loudest thing the backlog says about a row and the
// one an agent scans for, so it is withdrawn wherever the claim line printed
// directly beneath the row would contradict it.
func TestInProgressSuffixWithdrawsORPHANEDInAHeldLane(t *testing.T) {
	orphaned := annotation.AnnotatedIssue{
		Annotations: []annotation.Annotation{{
			Kind:    annotation.Orphaned,
			Message: "in_progress for 100h0m0s with no update",
		}},
	}

	if got := inProgressSuffix(orphaned, false); !strings.Contains(got, "(ORPHANED)") {
		t.Fatalf("inProgressSuffix(orphaned, lane unheld) = %q, want it to contain %q", got, "(ORPHANED)")
	}
	if got := inProgressSuffix(orphaned, true); strings.Contains(got, "ORPHANED") {
		t.Fatalf("inProgressSuffix(orphaned, lane held) = %q, want no %q — the row would contradict the claim line printed directly beneath it, which names a live holder", got, "ORPHANED")
	}
}

// TestInProgressSuffixSaysNothingWithoutTheAnnotation pins that the lane's
// standing only ever WITHDRAWS a word. A row inside its window is not orphaned
// whether or not anybody holds its lane, and a suffix inventing a verdict
// there would be a second orphan clock beside the annotation's.
func TestInProgressSuffixSaysNothingWithoutTheAnnotation(t *testing.T) {
	for _, laneHeld := range []bool{false, true} {
		got := inProgressSuffix(annotation.AnnotatedIssue{}, laneHeld)
		if strings.Contains(got, "stale") || strings.Contains(got, "ORPHANED") {
			t.Fatalf("inProgressSuffix(fresh row, laneHeld=%v) = %q, want the age alone: nothing here has aged out", laneHeld, got)
		}
	}
}

// TestCapacityForReadsAbandonmentOffTheLane carries the rule through to the
// verdict `lit next` actually acts on. An in-flight row in a lane another
// checkout holds is routed around whatever the row's own clock says — the
// holder is active in the lane, or has locked the worktree, and the derivation
// already folded both into Held. The same row in a lane nobody holds is served,
// orphan annotation or not: whoever started it no longer holds a claim there,
// and that is the whole of what "abandoned" means to routing.
func TestCapacityForReadsAbandonmentOffTheLane(t *testing.T) {
	h := newReadyTestHarness(t)
	ticket := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "In flight", Topic: "next", IssueType: "task", Priority: 0})
	h.transition(ticket.ID, model.Start{Assignee: "tester"})

	rows, _ := h.gather()
	quiet := rowByID(t, rows, ticket.ID)
	if ClassifyReadiness(quiet.Annotations).IsOrphaned() {
		t.Fatalf("fixture %q is orphaned; the pair below needs one row inside the orphan window and one past it", ticket.ID)
	}
	orphan(t, rows, ticket.ID)
	orphaned := rowByID(t, rows, ticket.ID)

	for _, row := range []annotation.AnnotatedIssue{quiet, orphaned} {
		if got := capacityFor(row, heldBy(otherAttribution), selfAttribution); got != routeAround {
			t.Fatalf("capacityFor(in-flight row, orphaned=%v, lane held by another) = %v, want %v — `lit next` would offer somebody's work in flight", ClassifyReadiness(row.Annotations).IsOrphaned(), got, routeAround)
		}
		if got := capacityFor(row, claims.Unclaimed{}, selfAttribution); got != serveWork {
			t.Fatalf("capacityFor(in-flight row, orphaned=%v, lane unheld) = %v, want %v — nobody holds the lane, so the row is abandoned on that fact alone", ClassifyReadiness(row.Annotations).IsOrphaned(), got, serveWork)
		}
	}
}
