package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// A lane read through LaneFamily says what the whole store says of it. The
// store is built around the readings a narrower read would get wrong: a hold
// resting entirely on a `done` against a closed ticket, the newest work in a
// lane sitting on an archived and a deleted ticket, a sibling in another lane
// holding the newest event of all, a nested epic filed in a lane, and two
// tickets in progress in one lane, which the claim line names one of.
func aLaneFamilyReadsWhatTheBacklogReads(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	child := func(title, lane string) model.Issue {
		return mustCreate(t, ctx, st, storage.CreateIssueInput{Title: title, Topic: "core", ParentID: epic.ID, Lane: lane})
	}
	finished, waiting := child("finished", "done-lane"), child("waiting", "done-lane")
	archived, deleted := child("archived", "retired-lane"), child("deleted", "retired-lane")
	first, second, _ := child("first", "busy-lane"), child("second", "busy-lane"), child("default lane", "")
	nested := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "nested", Topic: "core", IssueType: model.TypeEpic, ParentID: epic.ID, Lane: "retired-lane"})
	inNested := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "in nested", Topic: "core", ParentID: nested.ID})
	alone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	outside := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "outside", Topic: "core", ParentID: alone.ID})

	apply := func(checkout, id string, actions ...model.Action) {
		t.Helper()
		st.AttributeTo(checkout)
		for _, action := range actions {
			if _, err := st.Apply(ctx, id, storage.Change{Action: action, Actor: "ada"}); err != nil {
				t.Fatalf("Apply(%s, %T) error = %v", id, action, err)
			}
		}
	}
	start := model.Start{Assignee: "ada"}
	apply("checkout-a", finished.ID, start, model.Done{})
	apply("checkout-b", archived.ID, start, model.Archive{})
	apply("checkout-b", deleted.ID, start, model.Delete{})
	apply("checkout-b", inNested.ID, start)
	apply("checkout-c", outside.ID, start)
	apply("checkout-c", first.ID, start)
	apply("checkout-c", second.ID, start)

	storagetest.RequireLaneFamiliesMatchTheBacklog(t, ctx, st)

	// The comparison above would pass on two reads that both lost the hold, so
	// the one reading the ticket exists to keep is pinned outright.
	family, err := st.LaneFamily(ctx, waiting.ID)
	if err != nil {
		t.Fatalf("LaneFamily(%s) error = %v", waiting.ID, err)
	}
	lane, err := claims.NewLane(waiting.ID, family)
	if err != nil {
		t.Fatalf("NewLane(%s) error = %v", waiting.ID, err)
	}
	standing := lane.Standing(claims.Freshness{Now: time.Now(), Window: time.Hour}, claims.LocalCheckouts{})
	if held, ok := standing.(claims.Held); !ok || held.By.Stream() != "checkout-a" {
		t.Errorf("the standing of %s's lane = %+v, want it held by checkout-a on its `done` against the closed %s", waiting.ID, standing, finished.ID)
	}
}

// LaneFamily reads model.LaneFamily's set, every retention state included, and
// names the parent of everything in it.
func aLaneFamilyIsTheIssueAndWhatIsFiledBesideIt(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	epic := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "epic", Topic: "core", IssueType: model.TypeEpic})
	subject := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "subject", Topic: "core", ParentID: epic.ID})
	sibling := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "sibling", Topic: "core", ParentID: epic.ID, Lane: "another"})
	gone := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "gone", Topic: "core", ParentID: epic.ID})
	if _, err := st.Apply(ctx, gone.ID, storage.Change{Action: model.Delete{}, Actor: "ada"}); err != nil {
		t.Fatalf("Apply(%s, Delete) error = %v", gone.ID, err)
	}
	other := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "other epic", Topic: "core", IssueType: model.TypeEpic})
	stranger := mustCreate(t, ctx, st, storage.CreateIssueInput{Title: "stranger", Topic: "core", ParentID: other.ID})

	family, err := st.LaneFamily(ctx, subject.ID)
	if err != nil {
		t.Fatalf("LaneFamily error = %v", err)
	}
	got := make([]string, 0, len(family.Issues))
	for _, issue := range family.Issues {
		got = append(got, issue.ID)
	}
	slices.Sort(got)
	want := []string{subject.ID, sibling.ID, gone.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("family issues = %v, want %v", got, want)
	}
	if family.ParentByChild[subject.ID] != epic.ID || family.ParentTypes[epic.ID] != model.TypeEpic {
		t.Errorf("family parents = %v of types %v, want %s under the epic %s", family.ParentByChild, family.ParentTypes, subject.ID, epic.ID)
	}
	if slices.ContainsFunc(family.Events, func(event model.IssueEvent) bool { return event.IssueID == stranger.ID || event.IssueID == epic.ID }) {
		t.Errorf("family events = %+v, want none outside the family", family.Events)
	}
	if !slices.ContainsFunc(family.Events, func(event model.IssueEvent) bool { return event.IssueID == gone.ID }) {
		t.Errorf("family events = %+v, want the deleted %s's history", family.Events, gone.ID)
	}

	top, err := st.LaneFamily(ctx, other.ID)
	if err != nil {
		t.Fatalf("LaneFamily(%s) error = %v", other.ID, err)
	}
	if len(top.Issues) != 1 || top.Issues[0].ID != other.ID || len(top.ParentByChild) != 0 {
		t.Errorf("a parentless issue's family = %+v, want the issue alone", top)
	}

	missing, err := st.LaneFamily(ctx, "no-such-issue")
	if err != nil {
		t.Fatalf("LaneFamily(no-such-issue) error = %v", err)
	}
	if _, err := claims.NewLane("no-such-issue", missing); err == nil {
		t.Errorf("NewLane on the family of an id no issue has = nil error, want a refusal: family %+v", missing)
	}
}
