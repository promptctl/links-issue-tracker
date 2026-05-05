package store

import (
	"context"
	"strings"
	"testing"
)

func TestImportTreeCreatesEpicWithChildAndDep(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []ImportTreeSpec{
		{LocalID: "e1", Title: "Epic", IssueType: "epic", Topic: "tree", Priority: 2},
		{LocalID: "t1", Title: "First", IssueType: "task", Topic: "tree", Priority: 2, Parent: "e1"},
		{LocalID: "t2", Title: "Second", IssueType: "task", Topic: "tree", Priority: 2, Parent: "e1", DependsOn: []string{"t1"}},
	}
	result, err := st.ImportTree(ctx, "test", specs)
	if err != nil {
		t.Fatalf("ImportTree() error = %v", err)
	}
	if len(result.IDMap) != 3 {
		t.Fatalf("IDMap = %v, want 3 entries", result.IDMap)
	}
	t2 := result.IDMap["t2"]
	t1 := result.IDMap["t1"]
	if t2 == "" || t1 == "" {
		t.Fatalf("missing id mapping: %#v", result.IDMap)
	}
	detail, err := st.GetIssueDetail(ctx, t2)
	if err != nil {
		t.Fatalf("GetIssueDetail(t2) error = %v", err)
	}
	foundDep := false
	for _, d := range detail.DependsOn {
		if d.ID == t1 {
			foundDep = true
		}
	}
	if !foundDep {
		t.Fatalf("t2.DependsOn missing t1: %#v", detail.DependsOn)
	}
	if detail.Parent == nil || detail.Parent.ID != result.IDMap["e1"] {
		t.Fatalf("t2.Parent = %#v, want epic e1", detail.Parent)
	}
}

func TestImportTreeRejectsCycle(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "task", Topic: "x", Priority: 2, DependsOn: []string{"b"}},
		{LocalID: "b", Title: "B", IssueType: "task", Topic: "x", Priority: 2, DependsOn: []string{"a"}},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ImportTree(cycle) error = %v, want cycle error", err)
	}
}

func TestImportTreeRejectsMissingReference(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "task", Topic: "x", Priority: 2, Parent: "ghost"},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "missing parent") {
		t.Fatalf("ImportTree(missing parent) error = %v, want missing-parent error", err)
	}
}

func TestImportTreeRejectsInvalidType(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "ghost", Topic: "x", Priority: 2},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("ImportTree(bad type) error = %v, want invalid-type error", err)
	}
}
