package conformance

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// aCloseRecordsTheInstantItsWriteIsStamped closes a ticket on a clock behind
// its own history, so the write's stamp steps past that history rather than
// taking the clock's reading. The close time, the row's updated_at and the
// close event's created_at are one instant all the same: the stamp.
func aCloseRecordsTheInstantItsWriteIsStamped(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(laterInstant)
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core"})

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
	closing := detail.Events[len(detail.Events)-1]
	if closing.Action != string(model.ActionDone) {
		t.Fatalf("newest event = %q, want the done that closed the ticket", closing.Action)
	}
	if !closedAt.After(laterInstant) {
		t.Errorf("ClosedAt = %v, want past the history's %v rather than the clock's %v", closedAt, laterInstant, earlierInstant)
	}
	if !closedAt.Equal(detail.Issue.UpdatedAt) || !closedAt.Equal(closing.CreatedAt) {
		t.Errorf("closed_at %v, updated_at %v, close event created_at %v: want one instant", closedAt, detail.Issue.UpdatedAt, closing.CreatedAt)
	}
}
