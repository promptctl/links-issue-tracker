package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// aCloseRecordsTheInstantItsWriteIsStamped closes a ticket on a clock behind
// its own history, so the write's stamp steps past that history rather than
// taking the clock's reading. The close time, the row's updated_at and the
// close event's created_at are one instant all the same, the stamp, and each
// reads back in UTC, the contract's one form.
func aCloseRecordsTheInstantItsWriteIsStamped(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(laterInstant)
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})

	clk.Pin(earlierInstant)
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply done error = %v", err)
	}
	detail, err := st.GetIssueDetail(ctx, issue.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	closedAt := detail.Issue.ClosedAtValue()
	if closedAt == nil {
		t.Fatalf("ClosedAt after done = nil, want the write's stamp")
	}
	var closing []model.IssueEvent
	for _, event := range detail.Events {
		if event.Action == string(model.ActionDone) {
			closing = append(closing, event)
		}
	}
	if len(closing) != 1 {
		t.Fatalf("history holds %d done events, want the one that closed the ticket: %+v", len(closing), detail.Events)
	}
	if !closedAt.After(laterInstant) {
		t.Errorf("ClosedAt = %v, want past the history's %v rather than the clock's %v", closedAt, laterInstant, earlierInstant)
	}
	for _, read := range []struct {
		name string
		at   time.Time
	}{
		{"closed_at", *closedAt},
		{"updated_at", detail.Issue.UpdatedAt},
		{"the close event's created_at", closing[0].CreatedAt},
	} {
		if !read.at.Equal(*closedAt) {
			t.Errorf("%s = %v, want %v: a close records one instant", read.name, read.at, *closedAt)
		}
		if read.at.Location() != time.UTC {
			t.Errorf("%s = %v in %v, want UTC", read.name, read.at, read.at.Location())
		}
	}
}
