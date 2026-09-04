package store

import (
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestBulkApplyCreatesEpicWithChildAndDep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := func(s string) *string { return &s }
	specs := []storage.BulkIssueSpec{
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
	t1 := createdIDByRef(t, result.Created, "t1")
	t2 := createdIDByRef(t, result.Created, "t2")
	detail, err := st.GetIssueDetail(ctx, t2)
	if err != nil {
		t.Fatalf("GetIssueDetail(t2) error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != createdIDByRef(t, result.Created, "e1") {
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
	specs := []storage.BulkIssueSpec{
		{Title: title("First"), IssueType: issueType, Topic: topic},
		{Title: title("Second"), IssueType: issueType, Topic: topic},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	var first, second model.Issue
	for _, m := range result.Created {
		issue, err := st.GetIssue(ctx, m.ID)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", m.Ref, err)
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
	specs := []storage.BulkIssueSpec{
		{Title: title("Loose"), IssueType: title("task"), Topic: title("bulk")},
	}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("Created = %v, want 1 entry", result.Created)
	}
	if m := result.Created[0]; m.Ref != m.ID {
		t.Fatalf("Created[0] = %+v, want self-referenced (no local_id given)", m)
	}
}

// createdIDByRef resolves one create's real id from a result's ordered
// report, failing the test when the ref is not there.
func createdIDByRef(t *testing.T, created []storage.IDMapping, ref string) string {
	t.Helper()
	for _, m := range created {
		if m.Ref == ref {
			return m.ID
		}
	}
	t.Fatalf("created report %+v has no %q entry", created, ref)
	return ""
}

func TestBulkApplyUpdatesExistingIssueByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "Before", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	newTitle := "After"
	specs := []storage.BulkIssueSpec{
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
	existing, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "Old", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := func(s string) *string { return &s }
	newTitle := "Updated"
	specs := []storage.BulkIssueSpec{
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
	specs := []storage.BulkIssueSpec{
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
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	specs := []storage.BulkIssueSpec{{ID: created.ID}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "no fields to update") {
		t.Fatalf("BulkApply(empty update) error = %v, want no-fields error", err)
	}
}

func TestBulkApplyRejectsUpdateWithTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	topic := "other"
	specs := []storage.BulkIssueSpec{{ID: created.ID, Topic: &topic}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("BulkApply(update sets topic) error = %v, want immutable error", err)
	}
}

func TestBulkApplyRejectsUpdateWithParentOrDependsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "renamed"
	specs := []storage.BulkIssueSpec{{ID: created.ID, Title: &title, Parent: "somewhere"}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "lit parent set") {
		t.Fatalf("BulkApply(update sets parent) error = %v, want parent-set pointer error", err)
	}
}

func TestBulkApplyRejectsInvalidTypeOrPriorityOnUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	badType := "ghost"
	if _, err := st.BulkApply(ctx, "test", "agent", []storage.BulkIssueSpec{{ID: created.ID, IssueType: &badType}}); err == nil || !strings.Contains(err.Error(), "invalid type") {
		t.Fatalf("BulkApply(update invalid type) error = %v, want invalid-type error", err)
	}
	badPriority := 7
	if _, err := st.BulkApply(ctx, "test", "agent", []storage.BulkIssueSpec{{ID: created.ID, Priority: &badPriority}}); err == nil || !strings.Contains(err.Error(), "invalid priority") {
		t.Fatalf("BulkApply(update invalid priority) error = %v, want invalid-priority error", err)
	}
}

func TestBulkApplyRejectsMissingCreateFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	title := "no topic"
	specs := []storage.BulkIssueSpec{{Title: &title}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "missing topic") {
		t.Fatalf("BulkApply(missing topic) error = %v, want missing-topic error", err)
	}
}

func TestBulkApplyRejectsDuplicateID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	t1, t2 := "one", "two"
	specs := []storage.BulkIssueSpec{{ID: created.ID, Title: &t1}, {ID: created.ID, Title: &t2}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("BulkApply(duplicate id) error = %v, want duplicate-id error", err)
	}
}

func TestBulkApplyRejectsIDAndLocalIDTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "X", Topic: "bulk", IssueType: "task", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "y"
	specs := []storage.BulkIssueSpec{{ID: created.ID, LocalID: "x1", Title: &title}}
	if _, err := st.BulkApply(ctx, "test", "agent", specs); err == nil || !strings.Contains(err.Error(), "local_id") {
		t.Fatalf("BulkApply(id+local_id) error = %v, want local_id error", err)
	}
}

func TestBulkApplyCreateChildOfExistingIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Title: "Epic", Topic: "bulk", IssueType: "epic", Prefix: "test"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	title := "Child"
	issueType := "task"
	topic := "bulk"
	specs := []storage.BulkIssueSpec{{Title: &title, IssueType: &issueType, Topic: &topic, Parent: epic.ID}}
	result, err := st.BulkApply(ctx, "test", "agent", specs)
	if err != nil {
		t.Fatalf("BulkApply() error = %v", err)
	}
	detail, err := st.GetIssueDetail(ctx, result.Created[0].ID)
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
	specs := []storage.BulkIssueSpec{
		{LocalID: "a", Title: &title, IssueType: &issueType, Topic: &topic},
		{LocalID: "b", Title: &badTitle, IssueType: &issueType, Topic: &topic, Parent: "ghost-does-not-exist"},
	}
	_, err := st.BulkApply(ctx, "test", "agent", specs)
	if err == nil {
		t.Fatalf("BulkApply() error = nil, want failure on doc b's missing parent")
	}
	// Default ListIssues excludes deleted issues, so doc a's create surviving
	// here means the rollback failed to remove it.
	list, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	for _, issue := range list {
		if issue.Title == title {
			t.Fatalf("doc a's create was not rolled back: %#v", issue)
		}
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
