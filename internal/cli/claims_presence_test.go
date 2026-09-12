package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The surfaces links-claims-2wk2 was reported on, held to one rule: a word that
// asserts the holder is GONE may only be printed where this machine has not
// seen the holder's worktree. The clock is identical in every case below —
// these tests move the worktree and nothing else.
//
// The report was a lane whose worktree sat locked, on an open PR, with its
// session running, while `lit next` called it abandoned and `lit backlog`
// printed ORPHANED beside it. Both surfaces read a six-hour clock and neither
// read the enumeration the derivation had already performed.

// TestInFlightStateNeverCallsAVisibleHolderAbandoned covers `lit next`'s
// takeover sentence — the line an agent acts on before any claim line below it
// has been read.
func TestInFlightStateNeverCallsAVisibleHolderAbandoned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder claims.Presence
		// abandoned is whether the sentence may assert the holder left.
		abandoned bool
	}{
		{"a holder this machine cannot check keeps the original word", claims.Unprovable, true},
		{"a holder proven gone is genuinely abandoned", claims.Gone, true},
		{"a present worktree is not an abandoned one", claims.Present, false},
		{"a locked worktree is the furthest thing from an abandoned one", claims.Locked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := inFlightState(tc.holder)
			if strings.Contains(got, "abandoned") != tc.abandoned {
				t.Fatalf("inFlightState(%v) = %q, want abandoned=%v — the word claims the holder left, and this machine %s",
					tc.holder, got, tc.abandoned, map[bool]string{true: "cannot say otherwise", false: "can see them"}[tc.abandoned])
			}
			if got == "" {
				t.Fatalf("inFlightState(%v) = %q, want a clause: the sentence interpolates this and would read %q",
					tc.holder, got, "X is in progress and  — run ...")
			}
		})
	}
}

// TestInProgressSuffixWithdrawsORPHANEDForAVisibleHolder covers `lit backlog`'s
// row suffix. ORPHANED is the loudest thing the backlog says about a row and
// the one an agent scans for, so it carries the same rule as the sentence.
func TestInProgressSuffixWithdrawsORPHANEDForAVisibleHolder(t *testing.T) {
	orphaned := annotation.AnnotatedIssue{
		Annotations: []annotation.Annotation{{
			Kind:    annotation.Orphaned,
			Message: "in_progress for 100h0m0s with no update",
		}},
	}

	for _, tc := range []struct {
		name    string
		holder  claims.Presence
		orphan  bool
		wantSay string
	}{
		{"unprovable holder keeps ORPHANED", claims.Unprovable, true, "(ORPHANED)"},
		{"proven-gone holder keeps ORPHANED", claims.Gone, true, "(ORPHANED)"},
		{"present worktree reports staleness instead", claims.Present, true, "(stale, worktree present)"},
		{"locked worktree reports the lock", claims.Locked, true, "(stale, worktree locked)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := inProgressSuffix(orphaned, tc.holder)
			if !strings.Contains(got, tc.wantSay) {
				t.Fatalf("inProgressSuffix(orphaned, %v) = %q, want it to contain %q", tc.holder, got, tc.wantSay)
			}
			if tc.wantSay != "(ORPHANED)" && strings.Contains(got, "ORPHANED") {
				t.Fatalf("inProgressSuffix(orphaned, %v) = %q, want no %q — the row would contradict the claim line printed directly beneath it, which names a live worktree",
					tc.holder, got, "ORPHANED")
			}
		})
	}
}

// TestInProgressSuffixSaysNothingWithoutTheAnnotation pins that presence only
// ever WITHDRAWS a word. A row inside its window is not stale under any
// worktree state, and a suffix inventing "(stale, worktree present)" there
// would be a second staleness verdict beside the annotation's.
func TestInProgressSuffixSaysNothingWithoutTheAnnotation(t *testing.T) {
	for _, holder := range []claims.Presence{claims.Unprovable, claims.Gone, claims.Present, claims.Locked} {
		got := inProgressSuffix(annotation.AnnotatedIssue{}, holder)
		if strings.Contains(got, "stale") || strings.Contains(got, "ORPHANED") {
			t.Fatalf("inProgressSuffix(fresh row, %v) = %q, want the age alone: nothing here has aged out", holder, got)
		}
	}
}

// TestRelationOfLetsOnlyALockSustainAnExpiredClaim is the routing half, and the
// precedence it pins is the whole design in one table: a lock outranks the
// clock, and nothing else does.
//
// Getting this wrong in either direction has a name. Too permissive is the
// reported bug — a live session's lane offered as free work. Too strict would
// be worse and quieter: a worktree outlives its session routinely, so letting
// mere presence sustain a claim would strand every uncleaned tree's lane
// forever, with the age-out that exists to release it never firing.
func TestRelationOfLetsOnlyALockSustainAnExpiredClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder claims.Presence
		want   laneRelation
	}{
		{"unprovable: the clock is all we have, so the lane is available", claims.Unprovable, laneStaleForeign},
		{"gone: available, and this is the path the void filter already owns", claims.Gone, laneStaleForeign},
		{"present: available — a tree outliving its session is ordinary", claims.Present, laneStaleForeign},
		{"locked: the holder said do-not-disturb, so the hold stands", claims.Locked, laneHeldForeign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standing := claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: tc.holder}
			if got := relationOf(standing, selfAttribution); got != tc.want {
				t.Fatalf("relationOf(stale held by another, holder %v) = %v, want %v", tc.holder, got, tc.want)
			}
		})
	}
}

// TestCapacityForRoutesAroundALockedLane carries the relation through to the
// verdict `lit next` actually acts on. relationOf answering laneHeldForeign is
// only half the fix; if capacityFor still served the row, the sentence above it
// would be the only thing that changed and the pick would be the same one.
func TestCapacityForRoutesAroundALockedLane(t *testing.T) {
	h := newReadyTestHarness(t)
	ticket := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "In flight", Topic: "next", IssueType: "task", Priority: 0})
	h.transition(ticket.ID, model.Start{Assignee: "tester"})

	rows, _ := h.gather()
	orphan(t, rows, ticket.ID)
	row := rowByID(t, rows, ticket.ID)

	locked := claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: claims.Locked}
	if got := capacityFor(row, locked, selfAttribution); got != routeAround {
		t.Fatalf("capacityFor(orphaned row, locked foreign holder) = %v, want %v — `lit next` would offer a lane whose holder marked it do-not-disturb", got, routeAround)
	}

	// The control: the same row, the same expired clock, an unlocked worktree.
	// It stays takeable, which is what keeps this a fix and not a freeze.
	present := claims.Stale{Tenure: claims.Tenure{By: otherAttribution}, Holder: claims.Present}
	if got := capacityFor(row, present, selfAttribution); got != takeoverWork {
		t.Fatalf("capacityFor(orphaned row, present foreign holder) = %v, want %v — presence changes what the lane is called, never who may take it", got, takeoverWork)
	}
}
