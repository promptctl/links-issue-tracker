package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A leaf two parents claim is a shape only restored data holds, and this engine
// takes no restore, so the second edge is written into the engine directly. The
// detach rule is judged on the parent edges the removal leaves: one of two goes,
// and the last one stays.
func TestRemovingOneOfTwoParentEdgesKeepsALeafInside(t *testing.T) {
	ctx := context.Background()
	e, err := New("ws", storage.SystemClock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("E1"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E1) error = %v", err)
	}
	second, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("E2"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E2) error = %v", err)
	}
	child, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("C"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", ParentID: first.ID})
	if err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}
	e.relations = append(e.relations, model.Relation{SrcID: child.ID, DstID: second.ID, Type: model.RelParentChild})

	if err := e.RemoveRelation(ctx, child.ID, second.ID, model.RelParentChild); err != nil {
		t.Fatalf("RemoveRelation(%s -> %s) error = %v, want the second parent edge removed", child.ID, second.ID, err)
	}
	err = e.RemoveRelation(ctx, child.ID, first.ID, model.RelParentChild)
	var refused model.ValidationError
	if !errors.As(err, &refused) {
		t.Fatalf("RemoveRelation(%s -> %s) error = %v, want a refusal: it is the leaf's last parent", child.ID, first.ID, err)
	}
	if !e.hasRelation(child.ID, first.ID, model.RelParentChild) {
		t.Errorf("the refused removal dropped %s -> %s", child.ID, first.ID)
	}
}

// ParentByChild answers over a leaf two parents claim with the lower parent id,
// whichever edge was written first — the rule the Dolt engine's MIN applies.
// One child gains the lower edge second and the other the higher, so neither
// first-written nor last-written wins can pass.
func TestParentByChildTakesTheLowerOfTwoParents(t *testing.T) {
	ctx := context.Background()
	e, err := New("ws", storage.SystemClock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("E1"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E1) error = %v", err)
	}
	second, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("E2"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E2) error = %v", err)
	}
	higher, lower := max(first.ID, second.ID), min(first.ID, second.ID)
	for _, order := range [][2]model.ID{{higher, lower}, {lower, higher}} {
		child, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("C"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", ParentID: order[0]})
		if err != nil {
			t.Fatalf("CreateIssue(C under %s) error = %v", order[0], err)
		}
		e.relations = append(e.relations, model.Relation{SrcID: child.ID, DstID: order[1], Type: model.RelParentChild})

		got, err := e.ParentByChild(ctx)
		if err != nil {
			t.Fatalf("ParentByChild() error = %v", err)
		}
		if got[child.ID] != lower {
			t.Errorf("ParentByChild()[%s] with edges written %v = %q, want %q, the lower of its parents", child.ID, order, got[child.ID], lower)
		}
	}
}

// Siblings compare on their own keys in this engine too, so a child two parents
// claim lists by its key among the children of either, in Children and Siblings
// alike, where the Dolt engine's TestChildrenCompareOnTheirOwnKeys pins the same
// order.
func TestChildrenCompareOnTheirOwnKeys(t *testing.T) {
	ctx := context.Background()
	e, err := New("ws", storage.SystemClock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	mustIssue := func(in storage.CreateIssueInput) model.Issue {
		t.Helper()
		in.Topic, in.Prefix = "core", "conf"
		issue, err := e.CreateIssue(ctx, in)
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", in.Title, err)
		}
		return issue
	}
	lower := mustIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("one"), Lane: storage.SharedLane, IssueType: model.TypeEpic})
	upper := mustIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("two"), Lane: storage.SharedLane, IssueType: model.TypeEpic})
	if upper.ID < lower.ID {
		lower, upper = upper, lower
	}
	if _, err := e.RankToTop(ctx, upper.ID); err != nil {
		t.Fatalf("RankToTop(%s) error = %v", upper.ID, err)
	}
	child := mustIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("held"), Lane: storage.SharedLane, ParentID: lower.ID})
	kept := mustIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("kept"), Lane: storage.SharedLane, ParentID: upper.ID})
	later := mustIssue(storage.CreateIssueInput{Title: storage.MustParseTitle("later"), Lane: storage.SharedLane, ParentID: upper.ID})
	e.relations = append(e.relations, model.Relation{SrcID: child.ID, DstID: upper.ID, Type: model.RelParentChild})

	relations, err := e.GetRelationsByIDs(ctx, []model.ID{upper.ID})
	if err != nil {
		t.Fatalf("GetRelationsByIDs(%s) error = %v", upper.ID, err)
	}
	children := relations[upper.ID].Children
	if len(children) != 3 || children[0].ID != child.ID || children[1].ID != kept.ID || children[2].ID != later.ID {
		t.Errorf("Children of %s = %+v, want %s, %s, %s", upper.ID, children, child.ID, kept.ID, later.ID)
	}
	adjacency, err := e.GetIssueAdjacency(ctx, kept.ID)
	if err != nil {
		t.Fatalf("GetIssueAdjacency(%s) error = %v", kept.ID, err)
	}
	siblings := adjacency.Siblings
	if len(siblings) != 2 || siblings[0].ID != child.ID || siblings[1].ID != later.ID {
		t.Errorf("Siblings of %s = %+v, want %s before %s", kept.ID, siblings, child.ID, later.ID)
	}
}

// A title no parser admits reaches this engine only by being written into it
// directly, as restored data would hold one. A write of another field beside
// it lands, restating it is no change, and moving it to a new break is refused.
func TestAWriteBesideAStoredBreakTitleLands(t *testing.T) {
	ctx := context.Background()
	e, err := New("ws", storage.SystemClock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	epic, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: storage.MustParseTitle("E"), Lane: storage.SharedLane, Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E) error = %v", err)
	}
	rec, err := e.mustRecord(epic.ID)
	if err != nil {
		t.Fatalf("mustRecord error = %v", err)
	}
	rec.title = "two\nlines"
	description, restated, moved := "described", rec.title, "other\nlines"
	if _, err := e.Apply(ctx, epic.ID, storage.Change{Actor: "ada", Fields: storage.UpdateIssueInput{Description: &description, Title: &restated}}); err != nil {
		t.Fatalf("Apply(description, the stored title restated) error = %v", err)
	}
	_, err = e.Apply(ctx, epic.ID, storage.Change{Actor: "ada", Fields: storage.UpdateIssueInput{Title: &moved}})
	var refused model.ValidationError
	if !errors.As(err, &refused) {
		t.Errorf("Apply(title moved to another break) error = %v, want a ValidationError", err)
	}
	if got, _ := e.GetIssue(ctx, epic.ID); got.Title != "two\nlines" || got.Description != "described" {
		t.Errorf("title %q, description %q; want the stored title and the description", got.Title, got.Description)
	}
}
