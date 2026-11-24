package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// aCloseIsStampedWithItsWritesInstant pins that a close records one instant:
// the write's. closed_at, updated_at and the close event's created_at are the
// same instant, read back off the stored row and history rather than off the
// value Apply returned. A re-close changes nothing, so it keeps the close time
// the first close recorded.
func aCloseIsStampedWithItsWritesInstant(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})

	clk.Pin(laterInstant)
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Close{Outcome: model.Wontfix{}}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply close error = %v", err)
	}
	closed := mustGet(t, ctx, st, issue.ID)
	assertClosedAt(t, "after close", closed, laterInstant)
	if !closed.UpdatedAt.Equal(laterInstant) {
		t.Errorf("UpdatedAt after close = %v, want %v", closed.UpdatedAt, laterInstant)
	}
	events, err := st.ListEvents(ctx, issue.ID)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	last := events[len(events)-1]
	if last.Action != string(model.ActionClose) || !last.CreatedAt.Equal(laterInstant) {
		t.Errorf("last event = %s at %v, want %s at %v", last.Action, last.CreatedAt, model.ActionClose, laterInstant)
	}

	clk.Pin(laterInstant.Add(time.Hour))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply re-close error = %v", err)
	}
	assertClosedAt(t, "after re-close", mustGet(t, ctx, st, issue.ID), laterInstant)
}

func assertClosedAt(t *testing.T, when string, issue model.Issue, want time.Time) {
	t.Helper()
	if got := issue.ClosedAtValue(); got == nil || !got.Equal(want) {
		t.Errorf("ClosedAt %s = %v, want %v", when, got, want)
	}
}
