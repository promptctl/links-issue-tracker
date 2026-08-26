package store

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func TestBulkApplyCreatesEpicWithChildAndDep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := func(s string) *string { return &s }
	specs := []BulkIssueSpec{
		{LocalID: "e1", Title: title("Epic"), IssueType: title("epic"), Topic: title("bulk")},
		{LocalID: "t1", Title: title("First"), IssueType: title("task"), Topic: title("bulk"), Parent: "e1"},
		{LocalID: "t2", Title: title("Second"), IssueType: title("task"), Topic: title("bulk"), Parent: "e1", DependsOn: []string{"t1"}},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	if len(result.Created) != 3 {
		t.Fatalf("Created = %v, want 3 entries", result.Created)
	}
	t1, t2 := result.Created["t1"], result.Created["t2"]
	if t1 == "" || t2 == "" {
		t.Fatalf("missing created mapping: %#v", result.Created)
	}
	detail, err := st.GetIssueDetail(ctx, t2)
	if err != nil {
		t.Fatalf("GetIssueDetail(t2) error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != result.Created["e1"] {
		t.Fatalf("t2.Parent = %#v, want epic e1", detail.Parent)
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
}

// TestBulkApplyCreatesLandInFileOrder pins the batch half of the placement
// contract: a multi-document import with no placement named anywhere lands in
// the order the file lists, because every creation surface reaches the default
// by saying nothing. ImportTree gets the same treatment from the same zero
// value.
func TestBulkApplyCreatesLandInFileOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := func(s string) *string { return &s }
	issueType := title("task")
	topic := title("bulk")
	specs := []BulkIssueSpec{
		{Title: title("First"), IssueType: issueType, Topic: topic},
		{Title: title("Second"), IssueType: issueType, Topic: topic},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	var first, second model.Issue
	for ref, real := range result.Created {
		issue, err := st.GetIssue(ctx, real)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", ref, err)
		}
		if issue.Title == "First" {
			first = issue
		} else {
			second = issue
		}
	}
	if first.Rank == "" || second.Rank == "" {
		t.Fatalf("missing rank: first=%#v second=%#v", first, second)
	}
	if !(first.Rank < second.Rank) {
		t.Fatalf("first.Rank = %q, second.Rank = %q; want first < second (the default appends each create after the one before it, so the batch keeps file order)", first.Rank, second.Rank)
	}
}

func TestBulkApplyCreateWithoutLocalIDIsReportedByRealID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := func(s string) *string { return &s }
	specs := []BulkIssueSpec{
		{Title: title("Loose"), IssueType: title("task"), Topic: title("bulk")},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("Created = %v, want 1 entry", result.Created)
	}
	for ref, real := range result.Created {
		if ref != real {
			t.Fatalf("Created[%q] = %q, want self-keyed (no local_id given)", ref, real)
		}
	}
}

func TestBulkApplyUpdatesExistingIssueByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Before", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	newTitle := "After"
	specs := []BulkIssueSpec{
		{ID: created.ID, Title: &newTitle},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0] != created.ID {
		t.Fatalf("Updated = %v, want [%s]", result.Updated, created.ID)
	}
	issue, err := st.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.Title != "After" {
		t.Fatalf("issue.Title = %q, want %q", issue.Title, "After")
	}
}

func TestBulkApplyMixedCreateAndUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	existing, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Old", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := func(s string) *string { return &s }
	newTitle := "Updated"
	specs := []BulkIssueSpec{
		{LocalID: "new1", Title: title("Fresh"), IssueType: title("task"), Topic: title("bulk")},
		{ID: existing.ID, Title: &newTitle},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	if len(result.Created) != 1 || len(result.Updated) != 1 {
		t.Fatalf("Created = %v, Updated = %v, want 1 each", result.Created, result.Updated)
	}
	issue, err := st.GetIssue(ctx, existing.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.Title != "Updated" {
		t.Fatalf("issue.Title = %q, want %q", issue.Title, "Updated")
	}
}

func TestBulkApplyRejectsUnknownID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := "New title"
	specs := []BulkIssueSpec{
		{ID: "ghost-1", Title: &title},
	}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("BulkApply(unknown id) error = %v, want not-found error", err)
	}
}

func TestBulkApplyRejectsUpdateWithNoFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	specs := []BulkIssueSpec{{ID: created.ID}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "no fields to update") {
		t.Fatalf("BulkApply(empty update) error = %v, want no-fields error", err)
	}
}

func TestBulkApplyRejectsUpdateWithTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	topic := "other"
	specs := []BulkIssueSpec{{ID: created.ID, Topic: &topic}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("BulkApply(update sets topic) error = %v, want immutable error", err)
	}
}

func TestBulkApplyRejectsUpdateWithParentOrDependsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "renamed"
	specs := []BulkIssueSpec{{ID: created.ID, Title: &title, Parent: "somewhere"}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "lit parent set") {
		t.Fatalf("BulkApply(update sets parent) error = %v, want parent-set pointer error", err)
	}
}

func TestBulkApplyRejectsInvalidTypeOrPriorityOnUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	badType := "ghost"
	if _, err := st.BulkApply(ctx, "test", "agent", []BulkIssueSpec{{ID: created.ID, IssueType: &badType}}); err == nil || !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("BulkApply(update invalid type) error = %v, want invalid-type error", err)
	}
	badPriority := 7
	if _, err := st.BulkApply(ctx, "test", "agent", []BulkIssueSpec{{ID: created.ID, Priority: &badPriority}}); err == nil || !strings.Contains(err.Error(), "invalid priority") {
		t.Fatalf("BulkApply(update invalid priority) error = %v, want invalid-priority error", err)
	}
}

func TestBulkApplyRejectsMissingCreateFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := "no topic"
	specs := []BulkIssueSpec{{Title: &title}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "missing topic") {
		t.Fatalf("BulkApply(missing topic) error = %v, want missing-topic error", err)
	}
}

func TestBulkApplyRejectsDuplicateID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	t1, t2 := "one", "two"
	specs := []BulkIssueSpec{{ID: created.ID, Title: &t1}, {ID: created.ID, Title: &t2}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("BulkApply(duplicate id) error = %v, want duplicate-id error", err)
	}
}

func TestBulkApplyRejectsIDAndLocalIDTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "y"
	specs := []BulkIssueSpec{{ID: created.ID, LocalID: "x1", Title: &title}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "local_id") {
		t.Fatalf("BulkApply(id+local_id) error = %v, want local_id error", err)
	}
}

func TestBulkApplyCreateChildOfExistingIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Epic", Topic: "bulk", IssueType: "epic", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "Child"
	issueType := "task"
	topic := "bulk"
	specs := []BulkIssueSpec{{Title: &title, IssueType: &issueType, Topic: &topic, Parent: epic.ID}}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	var childID string
	for _, real := range result.Created {
		childID = real
	}
	detail, err := st.GetIssueDetail(ctx, childID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != epic.ID {
		t.Fatalf("child.Parent = %#v, want existing epic %s", detail.Parent, epic.ID)
	}
}

func TestBulkApplyRollsBackCreatesOnLaterFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := "will roll back"
	issueType := "task"
	topic := "bulk"
	badTitle := "b"
	// doc b's parent matches no local_id in the batch and no real issue, so
	// it passes validateBulkSpecs (which cannot check external existence)
	// and fails inside CreateIssue — after doc a has already been created.
	specs := []BulkIssueSpec{
		{LocalID: "a", Title: &title, IssueType: &issueType, Topic: &topic},
		{LocalID: "b", Title: &badTitle, IssueType: &issueType, Topic: &topic, Parent: "ghost-does-not-exist"},
	}
	_, err := st.BulkApply(ctx, "test", "agent", specs)
	if err == nil {
		t.Fatalf("BulkApply() error = nil, want failure on doc b's missing parent")
	}
	// Default ListIssues excludes deleted issues, so doc a's create surviving
	// here means the rollback failed to remove it.
	list, err := st.ListIssues(ctx, ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	for _, issue := range list {
		if issue.Title == title {
			t.Fatalf("doc a's create was not rolled back: %#v", issue)
		}
	}
}

func TestParseBulkSpecsRejectsUnknownField(t *testing.T) {
	t.Parallel()
	doc := []byte("title: X\ntopic: bulk\ntype: task\nchildren: [a, b]\n")
	if _, err := ParseBulkSpecs(doc); err == nil || !strings.Contains(err.Error(), "children") {
		t.Fatalf("ParseBulkSpecs(unknown field) error = %v, want error naming \"children\"", err)
	}
}

func TestParseBulkSpecsMultiDocument(t *testing.T) {
	t.Parallel()
	doc := []byte("title: A\ntopic: bulk\ntype: task\n---\ntitle: B\ntopic: bulk\ntype: task\n")
	specs, err := ParseBulkSpecs(doc)
	if err != nil {
		t.Fatalf("ParseBulkSpecs() error = %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
}

func TestBulkApplyRejectsEmptyInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	if _, err := st.BulkApply(ctx, "test", "agent", nil); err == nil || !strings.Contains(err.Error(), "no issues in input") {
		t.Fatalf("BulkApply(empty) error = %v, want no-issues error", err)
	}
}
