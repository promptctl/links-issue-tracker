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
	assertCloseStampedAt(t, ctx, st, issue.ID, laterInstant)

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

// aStampPastHistoryIsTheNextInstantInUTC drives the stamp's other branch: the
// clock reads behind the history the write is planned against, so the write
// takes the nanosecond after the history's newest instant, in UTC, though the
// clock reads a non-UTC zone.
func aStampPastHistoryIsTheNextInstantInUTC(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(laterInstant)
	issue := startedTicket(t, ctx, st)

	newest := newestEventAt(t, ctx, st, issue.ID)
	clk.Pin(earlierInstant.In(time.FixedZone("UTC+9", 9*60*60)))
	assertCloseStampedAt(t, ctx, st, issue.ID, newest.Add(time.Nanosecond))
}

// aStampPastAnImportedOffsetIsInUTC is the same branch over history that
// arrived spelled in another zone: an import keeps each event's offset, so
// the newest instant a write is stamped past carries it, and the stamp still
// lands in UTC. Only an engine that can be replaced from an export can hold
// such history, so the case asks for the capability.
func aStampPastAnImportedOffsetIsInUTC(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	importer, ok := st.(storage.Importer)
	if !ok {
		t.Skip("the engine cannot be replaced from an export, so it holds no history spelled in another zone")
	}
	clk.Pin(laterInstant)
	issue := startedTicket(t, ctx, st)
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export error = %v", err)
	}
	east := time.FixedZone("UTC+2", 2*60*60)
	for i := range export.Events {
		export.Events[i].CreatedAt = export.Events[i].CreatedAt.In(east)
	}
	if _, err := importer.ReplaceFromExport(ctx, export); err != nil {
		t.Fatalf("ReplaceFromExport error = %v", err)
	}

	newest := newestEventAt(t, ctx, st, issue.ID)
	clk.Pin(earlierInstant)
	assertCloseStampedAt(t, ctx, st, issue.ID, newest.Add(time.Nanosecond))
}

// startedTicket files a ticket under an epic and starts it, all at the clock's
// pinned instant, and returns the ticket.
func startedTicket(t *testing.T, ctx context.Context, st storage.Store) model.Issue {
	t.Helper()
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, Topic: "core", IssueType: model.TypeEpic})
	issue := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: storage.MustParseTitle("work"), Lane: storage.SharedLane, Topic: "core", ParentID: epic.ID})
	if _, err := st.Apply(ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "ada"}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply start error = %v", err)
	}
	return issue
}

// newestEventAt is the newest instant in id's history, which must have one.
func newestEventAt(t *testing.T, ctx context.Context, st storage.Store, id model.ID) time.Time {
	t.Helper()
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	newest := model.NewestInstant(events)
	if newest.IsZero() {
		t.Fatalf("ListEvents(%s) is empty; want the start's history", id)
	}
	return newest
}

// assertCloseStampedAt closes id and holds the stored close to want, in UTC:
// its close time, its update time, and its event's.
func assertCloseStampedAt(t *testing.T, ctx context.Context, st storage.Store, id model.ID, want time.Time) {
	t.Helper()
	if _, err := st.Apply(ctx, id, storage.Change{Action: model.Close{Outcome: model.Wontfix{}}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply close error = %v", err)
	}
	closed := mustGet(t, ctx, st, id)
	assertClosedAt(t, "after close", closed, want)
	assertUTCInstant(t, "UpdatedAt after close", closed.UpdatedAt, want)
	events, err := st.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("ListEvents(%s) after a close is empty; want the close event", id)
	}
	last := events[len(events)-1]
	if last.Action != string(model.ActionClose) {
		t.Fatalf("last event = %s, want %s", last.Action, model.ActionClose)
	}
	assertUTCInstant(t, "close event CreatedAt", last.CreatedAt, want)
}
