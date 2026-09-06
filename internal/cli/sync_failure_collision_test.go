package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func collidingIssue(t *testing.T, id, title, description string, born time.Time) model.Issue {
	t.Helper()
	issue := model.Issue{ID: id, IssueType: "task", Title: title, Description: description, CreatedAt: born, UpdatedAt: born}
	hydrated, err := model.HydrateStatus(issue, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus(%s): %v", id, err)
	}
	return hydrated
}

func collisionFailure(t *testing.T) SyncFailure {
	t.Helper()
	return SyncFailure{
		Class:  syncFailureIDCollision,
		Remote: "origin",
		Branch: "master",
		Ahead:  3,
		Behind: 2,
		Collisions: []merge.Collision{{
			IssueID: "links-epic-a1b2.16",
			Ours: collidingIssue(t, "links-epic-a1b2.16", "Wire the release watchdog",
				"alarm when a pending release sits past its window",
				time.Date(2026, 8, 27, 9, 14, 2, 118_000_000, time.UTC)),
			Theirs: collidingIssue(t, "links-epic-a1b2.16", "Adaptive id length for large backlogs",
				"hash ids grow a character past 4k issues",
				time.Date(2026, 8, 27, 16, 40, 55, 907_000_000, time.UTC)),
		}},
	}
}

// TestSyncFailureBlockIDCollisionNamesBothTickets is the ticket's operator-visible
// report. The merge was refused, so this block is the only place the other side's
// ticket appears anywhere on this machine — if it does not carry both tickets
// whole, the report loses the same work the fusion used to lose.
func TestSyncFailureBlockIDCollisionNamesBothTickets(t *testing.T) {
	t.Parallel()
	block := collisionFailure(t).blockString()
	assertContractElements(t, block, "lit new")

	for _, want := range []string{
		"WHAT COLLIDED",
		"links-epic-a1b2.16",
		// Both jobs, title AND body — a report naming only the survivor would be
		// the silent-loser defect wearing a warning label.
		"Wire the release watchdog",
		"alarm when a pending release sits past its window",
		"Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues",
		// The sides say which store each came from, and the birth certificates are
		// the evidence that these are two tickets rather than one that diverged.
		"yours  (local)",
		"theirs (remote)",
		"2026-08-27T09:14:02.118Z",
		"2026-08-27T16:40:55.907Z",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("id-collision block missing %q:\n%s", want, block)
		}
	}
}

// TestSyncFailureIDCollisionEscalatesAsBlocked pins that a collision never reads
// as a routine, age-scaled divergence. Everything about the backlog still looks
// well-formed, which is exactly why the severity must come from the class: two
// tickets under one id collide identically on the first retry and the hundredth.
func TestSyncFailureIDCollisionEscalatesAsBlocked(t *testing.T) {
	t.Parallel()
	fresh := collisionFailure(t)
	fresh.Age = time.Minute
	block := fresh.blockString()
	if !strings.Contains(block, "ESCALATION — BLOCKED") {
		t.Errorf("a minute-old collision escalated as routine; severity must come from the class:\n%s", block)
	}
	if strings.Contains(block, "still within the window where a divergence is routine") {
		t.Errorf("collision block used the aged routine framing:\n%s", block)
	}
}

// TestSyncFailureIDCollisionNamesNoFalseRemedy is the honesty guard. Closing or
// deleting the losing row does NOT free the id — both are soft states and the row
// still exports, so it still collides — and lit has no re-id operation. A block
// that offered one of those as the fix would send an operator to do work that
// cannot resolve this. [LAW:no-silent-failure]
func TestSyncFailureIDCollisionNamesNoFalseRemedy(t *testing.T) {
	t.Parallel()
	block := collisionFailure(t).blockString()
	for _, forbidden := range []string{"lit close", "lit sync reconcile take", "lit sync reconcile combine"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("id-collision block offers %q, which does not free a colliding id:\n%s", forbidden, block)
		}
	}
	if !strings.Contains(block, "not yet a lit operation") {
		t.Errorf("block must say plainly that retiring the duplicate id is not automated:\n%s", block)
	}
}

// TestCollisionLinesRenderEmptyDescriptionExplicitly keeps an absent body from
// rendering as a blank the reader has to interpret — the same rule
// writeProseSection follows on the prose surface.
func TestCollisionLinesRenderEmptyDescriptionExplicitly(t *testing.T) {
	t.Parallel()
	failure := collisionFailure(t)
	failure.Collisions[0].Theirs = collidingIssue(t, "links-epic-a1b2.16", "no body", "  \n ",
		time.Date(2026, 8, 27, 16, 40, 55, 0, time.UTC))
	if !strings.Contains(failure.blockString(), "(no description)") {
		t.Errorf("an empty description rendered as a blank instead of an explicit marker:\n%s", failure.blockString())
	}
}

// TestInlineReceiveSurfacesIDCollision covers the path a collision is actually
// hit on: the inline auto-reconcile that runs after nearly every command. Both
// halves of surfaceInlineOutcome are asserted — the block it prints and the owner
// event it notifies on — because a collision that produced neither would be the
// "committed autonomously with no signal at all" failure the refusal exists to
// end. [LAW:no-silent-failure]
func TestInlineReceiveSurfacesIDCollision(t *testing.T) {
	t.Parallel()
	now := time.Now()
	outcome := syncReceiveOutcome{
		remote: "origin", branch: "master", ahead: 2, behind: 3,
		oldestDivergedUnix: now.Add(-time.Hour).Unix(),
		reconcile: &reconcileOutcome{
			state:      storage.SyncReconcileIDCollision,
			collisions: collisionFailure(t).Collisions,
		},
	}
	if outcome.settledCleanly() {
		t.Fatal("a refused collision reported as settled; the divergence episode would be cleared")
	}
	failure, ok := outcome.inlineSyncFailure(now)
	if !ok {
		t.Fatal("the inline reconcile stayed silent on an id collision")
	}
	if failure.Class != syncFailureIDCollision {
		t.Fatalf("inline failure class = %q, want %q", failure.Class, syncFailureIDCollision)
	}
	// The other side's ticket reaches the operator only through this block.
	block := failure.blockString()
	for _, want := range []string{"WHAT COLLIDED", "Wire the release watchdog", "Adaptive id length for large backlogs"} {
		if !strings.Contains(block, want) {
			t.Errorf("inline collision block missing %q:\n%s", want, block)
		}
	}
	ev, ok := ownerNotifyEventForFailure(failure)
	if !ok {
		t.Fatal("an id collision produced no owner notification")
	}
	if ev.Kind != ownerNotifyKind(syncFailureIDCollision) {
		t.Fatalf("owner notify kind = %q, want %q", ev.Kind, syncFailureIDCollision)
	}
}

// TestSyncPullSurfacesIDCollisionAsSyncFailure pins the explicit-pull surface. A
// pull state this mapping does not know falls through to the outcome printer under
// a success exit, so an unmapped collision would read as a clean pull.
func TestSyncPullSurfacesIDCollisionAsSyncFailure(t *testing.T) {
	t.Parallel()
	result := storage.SyncPullResult{
		State:      storage.SyncPullIDCollision,
		Ahead:      2,
		Behind:     3,
		Collisions: collisionFailure(t).Collisions,
	}
	failure, held := syncFailureFromPull("origin", "master", result, time.Now())
	if !held {
		t.Fatal("lit sync pull rendered an id collision as an ordinary outcome")
	}
	if failure.Failure.Class != syncFailureIDCollision {
		t.Fatalf("pull failure class = %q, want %q", failure.Failure.Class, syncFailureIDCollision)
	}
	if len(failure.Failure.Collisions) != 1 {
		t.Fatalf("pull failure carried %d collision(s); the block would render an empty section", len(failure.Failure.Collisions))
	}
	if block := failure.Failure.blockString(); !strings.Contains(block, "Adaptive id length for large backlogs") {
		t.Fatalf("pull collision block lost the other side's ticket:\n%s", block)
	}
}
