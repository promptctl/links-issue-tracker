package storagetest

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// RequireLaneFamiliesMatchTheBacklog reads every issue's lane through
// LaneFamily and holds it to what the whole store says of that lane: the same
// standing, the same progress with the same ticket active. Reaching every lane
// is part of it, so a lane no family landed in fails too.
func RequireLaneFamiliesMatchTheBacklog(t testing.TB, ctx context.Context, st storage.Store) {
	t.Helper()
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListIssues error = %v", err)
	}
	parents, err := st.ParentByChild(ctx)
	if err != nil {
		t.Fatalf("ParentByChild error = %v", err)
	}
	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents error = %v", err)
	}
	evidence, err := claims.NewEvidence(issues, parents, events)
	if err != nil {
		t.Fatalf("NewEvidence error = %v", err)
	}
	fresh := claims.Freshness{Now: time.Now(), Window: time.Hour}
	var local claims.LocalCheckouts
	standings := claims.Derive(evidence, fresh, local)

	reached := map[model.LaneID]bool{}
	for _, issue := range issues {
		family, err := st.LaneFamily(ctx, issue.ID)
		if err != nil {
			t.Fatalf("LaneFamily(%s) error = %v", issue.ID, err)
		}
		lane, err := claims.NewLane(issue.ID, family)
		if err != nil {
			t.Fatalf("NewLane(%s) error = %v", issue.ID, err)
		}
		reached[lane.ID()] = true
		if got, want := lane.Standing(fresh, local), standings.Of(lane.ID()); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the standing of lane %v read from its family = %+v, want the whole store's %+v", issue.ID, lane.ID(), got, want)
		}
		if got, want := lane.ProgressWith(issue), evidence.LaneProgressWith(issue); !sameProgress(got, want) {
			t.Errorf("%s: the progress of its lane read from its family = %+v, want the whole store's %+v", issue.ID, got, want)
		}
		if got, want := lane.Progress(), evidence.LaneProgress(lane.ID()); !sameProgress(got, want) {
			t.Errorf("%s: the progress of lane %v read from its family = %+v, want the whole store's %+v", issue.ID, lane.ID(), got, want)
		}
	}
	if len(reached) != len(evidence.Lanes()) {
		t.Errorf("the families reached %d lanes, want the store's %d: %v", len(reached), len(evidence.Lanes()), evidence.Lanes())
	}
}

func sameProgress(a, b claims.LaneProgress) bool {
	return a.Progress == b.Progress && activeID(a) == activeID(b)
}

func activeID(progress claims.LaneProgress) string {
	if progress.Active == nil {
		return ""
	}
	return progress.Active.ID
}
