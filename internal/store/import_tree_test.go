package store

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestImportTreeCreatesEpicWithChildAndDep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []storage.ImportTreeSpec{
		{LocalID: "e1", Title: "Epic", IssueType: "epic", Topic: "tree", Priority: 0},
		{LocalID: "t1", Title: "First", IssueType: "task", Topic: "tree", Priority: 0, Parent: "e1"},
		{LocalID: "t2", Title: "Second", IssueType: "task", Topic: "tree", Priority: 0, Parent: "e1", DependsOn: []string{"t1"}},
	}
	result, err := st.ImportTree(ctx, "test", specs)
	if err != nil {
		t.Fatalf("ImportTree() error = %v", err)
	}
	if len(result.Created) != 3 {
		t.Fatalf("Created = %v, want 3 entries", result.Created)
	}
	t1 := createdIDByRef(t, result.Created, "t1")
	t2 := createdIDByRef(t, result.Created, "t2")
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
	if detail.Parent == nil || detail.Parent.ID != createdIDByRef(t, result.Created, "e1") {
		t.Fatalf("t2.Parent = %#v, want epic e1", detail.Parent)
	}
}

func TestImportTreeRejectsCycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []storage.ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "task", Topic: "x", Priority: 0, DependsOn: []string{"b"}},
		{LocalID: "b", Title: "B", IssueType: "task", Topic: "x", Priority: 0, DependsOn: []string{"a"}},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("ImportTree(cycle) error = %v, want cycle error", err)
	}
}

func TestImportTreeRejectsMissingReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []storage.ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "task", Topic: "x", Priority: 0, Parent: "ghost"},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "missing parent") {
		t.Fatalf("ImportTree(missing parent) error = %v, want missing-parent error", err)
	}
}

func TestImportTreeRejectsInvalidType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	specs := []storage.ImportTreeSpec{
		{LocalID: "a", Title: "A", IssueType: "ghost", Topic: "x", Priority: 0},
	}
	if _, err := st.ImportTree(ctx, "test", specs); err == nil || !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("ImportTree(bad type) error = %v, want invalid-type error", err)
	}
}

// The valid flat form (local_id + parent + depends_on) must survive the strict
// parse and import unchanged: same wiring the in-memory spec form produces.
func TestParseImportTreeSpecsValidFlatFormImports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	flat := []byte(`[
		{"local_id":"e1","title":"Epic","type":"epic","topic":"tree","priority":0},
		{"local_id":"t1","title":"First","type":"task","topic":"tree","priority":0,"parent":"e1"},
		{"local_id":"t2","title":"Second","type":"task","topic":"tree","priority":0,"parent":"e1","depends_on":["t1"]}
	]`)
	specs, err := storage.ParseImportTreeSpecs(flat)
	if err != nil {
		t.Fatalf("ParseImportTreeSpecs(flat) error = %v", err)
	}
	result, err := st.ImportTree(ctx, "test", specs)
	if err != nil {
		t.Fatalf("ImportTree() error = %v", err)
	}
	if len(result.Created) != 3 {
		t.Fatalf("Created = %v, want 3 entries", result.Created)
	}
	detail, err := st.GetIssueDetail(ctx, createdIDByRef(t, result.Created, "t2"))
	if err != nil {
		t.Fatalf("GetIssueDetail(t2) error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != createdIDByRef(t, result.Created, "e1") {
		t.Fatalf("t2.Parent = %#v, want epic e1", detail.Parent)
	}
	foundDep := false
	for _, d := range detail.DependsOn {
		if d.ID == createdIDByRef(t, result.Created, "t1") {
			foundDep = true
		}
	}
	if !foundDep {
		t.Fatalf("t2.DependsOn missing t1: %#v", detail.DependsOn)
	}
}
