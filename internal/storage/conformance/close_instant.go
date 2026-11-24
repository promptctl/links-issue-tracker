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
// same instant, in UTC, read back off the stored row and history rather than
// off the value Apply returned. The clock reads a non-UTC zone, so a close
// time that skipped the clock's normalization shows here. A re-close changes
// nothing, so it keeps the close time the first close recorded.
func aCloseIsStampedWithItsWritesInstant(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(earlierInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})

	clk.Pin(laterInstant.In(time.FixedZone("UTC-5", -5*60*60)))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Close{Outcome: model.Wontfix{}}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply close error = %v", err)
	}
	closed := mustGet(t, ctx, st, issue.ID)
	assertClosedAt(t, "after close", closed, laterInstant)
	assertUTCInstant(t, "UpdatedAt after close", closed.UpdatedAt, laterInstant)
	events, err := st.ListEvents(ctx, issue.ID)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("ListEvents(%s) after a close is empty; want the close event", issue.ID)
	}
	last := events[len(events)-1]
	if last.Action != string(model.ActionClose) {
		t.Errorf("last event = %s, want %s", last.Action, model.ActionClose)
	}
	assertUTCInstant(t, "close event CreatedAt", last.CreatedAt, laterInstant)

	clk.Pin(laterInstant.Add(time.Hour))
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply re-close error = %v", err)
	}
	assertClosedAt(t, "after re-close", mustGet(t, ctx, st, issue.ID), laterInstant)
}

func assertClosedAt(t *testing.T, when string, issue model.Issue, want time.Time) {
	t.Helper()
	got := issue.ClosedAtValue()
	if got == nil {
		t.Errorf("ClosedAt %s = nil, want %v", when, want)
		return
	}
	assertUTCInstant(t, "ClosedAt "+when, *got, want)
}

// assertUTCInstant pins both halves of a stored instant: when it is, and that
// it is spelled in UTC, which time.Equal alone would not notice.
func assertUTCInstant(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("%s = %v, want %v in UTC", what, got, want)
	}
}
