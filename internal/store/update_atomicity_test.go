package store

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// doltCommitCount returns the number of commits in the working branch's history.
// A combined transition+field ApplyUpdate must add exactly one — proof the two
// writes share a single Dolt commit rather than landing as two.
func doltCommitCount(t *testing.T, ctx context.Context, st *Store) int {
	t.Helper()
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log()`).Scan(&count); err != nil {
		t.Fatalf("count dolt commits: %v", err)
	}
	return count
}

// TestApplyUpdateTransitionAndFieldsCommitAsOneUnit pins the atomicity end
// state: an update carrying both a status change and a field edit lands as ONE
// Dolt commit with both halves applied — not two commits that could tear apart.
func TestApplyUpdateTransitionAndFieldsCommitAsOneUnit(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	created, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Original",
		Topic:     "atomicity",
		IssueType: "task",
		Priority:  model.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	before := doltCommitCount(t, ctx, st)

	updated, err := st.ApplyUpdate(ctx, created.ID, ApplyUpdateInput{
		TargetStatus: "closed",
		TransitionBy: "tester",
		Fields: UpdateIssueInput{
			Title:    ptr("Renamed"),
			Priority: ptr(model.PriorityUrgent),
			By:       "tester",
		},
	})
	if err != nil {
		t.Fatalf("ApplyUpdate() error = %v", err)
	}

	// Both halves must be visible on the returned issue and on a fresh read.
	if updated.State() != model.StateClosed {
		t.Fatalf("ApplyUpdate() state = %q, want %q", updated.State(), model.StateClosed)
	}
	if updated.Title != "Renamed" {
		t.Fatalf("ApplyUpdate() title = %q, want %q", updated.Title, "Renamed")
	}
	if updated.Priority != model.PriorityUrgent {
		t.Fatalf("ApplyUpdate() priority = %d, want %d", updated.Priority, model.PriorityUrgent)
	}

	after := doltCommitCount(t, ctx, st)
	if got := after - before; got != 1 {
		t.Fatalf("combined transition+field update added %d Dolt commits, want 1 (the two writes must share one commit)", got)
	}
}

// TestApplyUpdateRejectedFieldWriteLeavesNoTransition is the regression guard
// for the ticket's exact hazard: a status transition paired with a field edit
// that fails validation must leave the issue WHOLLY untouched. Before the
// plan/apply split the transition committed first, so an invalid field left a
// status-moved-but-fields-unwritten row and an audit event for a "failed"
// command. Field validation now runs before any write, so the transition never
// lands — the torn state is unrepresentable.
func TestApplyUpdateRejectedFieldWriteLeavesNoTransition(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	created, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix:    "test",
		Title:     "Original",
		Topic:     "atomicity",
		IssueType: "task",
		Priority:  model.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}

	before, err := st.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(before) error = %v", err)
	}

	// A valid transition (open -> closed) paired with an empty title, which
	// planFieldUpdate rejects.
	_, err = st.ApplyUpdate(ctx, created.ID, ApplyUpdateInput{
		TargetStatus: "closed",
		TransitionBy: "tester",
		Fields: UpdateIssueInput{
			Title: ptr(""),
			By:    "tester",
		},
	})
	if err == nil {
		t.Fatalf("ApplyUpdate() error = nil, want title-validation rejection")
	}

	after, err := st.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(after) error = %v", err)
	}

	// The transition must not have applied: the issue is still open.
	if after.Issue.State() != model.StateOpen {
		t.Fatalf("ApplyUpdate() left state = %q after a rejected field write; want %q (transition must roll back with the field write)", after.Issue.State(), model.StateOpen)
	}
	if after.Issue.Title != "Original" {
		t.Fatalf("ApplyUpdate() title = %q after rejection, want unchanged %q", after.Issue.Title, "Original")
	}
	// No audit event for the half-applied command — neither a transition event
	// nor a field event.
	if added := len(after.Events) - len(before.Events); added != 0 {
		t.Fatalf("ApplyUpdate() recorded %d events on a rejected update, want 0", added)
	}
}
