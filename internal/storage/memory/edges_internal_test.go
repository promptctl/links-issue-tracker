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
	first, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: "E1", Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E1) error = %v", err)
	}
	second, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: "E2", Topic: "core", Prefix: "conf", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(E2) error = %v", err)
	}
	child, err := e.CreateIssue(ctx, storage.CreateIssueInput{Title: "C", Topic: "core", Prefix: "conf", ParentID: first.ID})
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
