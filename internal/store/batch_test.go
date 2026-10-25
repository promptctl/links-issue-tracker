package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// openBatchStore opens a migrated store holding one epic, the parent the
// batches below create under.
func openBatchStore(t *testing.T, ctx context.Context) (*Store, string) {
	t.Helper()
	st, err := Open(ctx, testEnv(), migratedDoltDir(t), "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "home", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	return st, epic.ID
}

// refusedFile is a bulk file whose first document lands and whose second is
// refused at its write: it names a parent no issue answers to.
func refusedFile(epic string) []storage.BulkIssueSpec {
	title, topic, taskType := "lands", "topic", "task"
	return []storage.BulkIssueSpec{
		{Title: &title, Topic: &topic, IssueType: &taskType, Parent: &epic},
		{Title: &title, Topic: &topic, IssueType: &taskType, Parent: strptr("nowhere-1")},
	}
}

// A refused file leaves the branch on the commit it started from: none of the
// commits its writes made outlives the refusal.
func TestARefusedImportLeavesTheBranchWhereItStarted(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	if _, err := st.BulkApply(ctx, "test", "ada", refusedFile(epic)); err == nil {
		t.Fatal("BulkApply() succeeded; want the missing parent refused")
	}
	if got := headCommit(t, ctx, st); got != head {
		t.Fatalf("head after the refusal = %s; want %s, the head before the file", got, head)
	}
	assertWorkingSetClean(t, ctx, st)
}

// A refused file's error is the refusal, classified as the refusal, and it
// records the commit the batch reset to with no reset error, so a refusal at
// a write reads apart from one the gate made before writing anything.
func TestARefusedImportRecordsTheCommitItResetTo(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	_, err := st.BulkApply(ctx, "test", "ada", refusedFile(epic))
	var refused *RefusedBatchError
	if !errors.As(err, &refused) {
		t.Fatalf("BulkApply() error = %v; want a *RefusedBatchError", err)
	}
	if notFound := (storage.NotFoundError{}); !errors.As(err, &notFound) {
		t.Errorf("BulkApply() error = %v; want it still classified as the missing parent's not-found", err)
	}
	if err.Error() != refused.Cause.Error() {
		t.Errorf("error text = %q; want the refusal's own %q", err.Error(), refused.Cause.Error())
	}
	facts, _ := refused.EventRecord()
	if facts["import.reset_to"] != head || facts["import.reset_error"] != "" {
		t.Errorf("EventRecord() = %v; want import.reset_to %s and an empty import.reset_error", facts, head)
	}
}

// A batch refused because its caller gave up still resets: the cancellation
// that refused it cannot also stop its reset.
func TestABatchRefusedByCancellationStillResets(t *testing.T) {
	st, epic := openBatchStore(t, context.Background())
	head := headCommit(t, context.Background(), st)
	ctx, cancel := context.WithCancel(context.Background())
	err := st.withBatch(ctx, func(ctx context.Context) error {
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "lands", Topic: "topic", IssueType: "task", ParentID: epic}); err != nil {
			t.Fatalf("CreateIssue() error = %v", err)
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("withBatch() error = %v; want the cancellation", err)
	}
	if got := headCommit(t, context.Background(), st); got != head {
		t.Fatalf("head after the cancelled batch = %s; want %s, the head before it", got, head)
	}
}

// A write a failed commit left staged is not the batch's to discard: the
// batch commits it first, under the stranded-writes message, and records it,
// so the refusal's reset keeps it.
func TestARefusedImportKeepsAStrandedWrite(t *testing.T) {
	ctx := context.Background()
	root := migratedDoltDir(t)
	st, err := Open(ctx, testEnv(), root, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "home", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	strandWrite(t, ctx, root, epic.ID)

	st = openSyncOrFatal(t, ctx, root)
	defer func() { _ = st.Close() }()
	if _, err := st.BulkApply(ctx, "test", "ada", refusedFile(epic.ID)); err == nil {
		t.Fatal("BulkApply() succeeded; want the missing parent refused")
	}
	if issue := getIssueOrFatal(t, ctx, st, epic.ID); issue.Title != strandedTitle {
		t.Fatalf("epic title after the refusal = %q; want the stranded %q", issue.Title, strandedTitle)
	}
	assertStrandedWritesVersioned(t, ctx, st)
	assertWorkingSetClean(t, ctx, st)
}

// A refused batch whose reset fails too says so, and does not report the file
// as writing nothing: the result is what stays written, the error names every
// write in it, and the event carries the commit the reset could not reach.
func TestABatchWhoseResetFailsNamesWhatStaysWritten(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	result := storage.BulkApplyResult{Created: []storage.IDMapping{storage.NewIDMapping("", "test-kept")}, Updated: []string{epic}}
	var heard []string
	st.ObserveChanges(func(_ storage.Change, issue model.Issue) { heard = append(heard, issue.ID) })
	err := st.withBatch(ctx, func(ctx context.Context) error {
		// A change the batch made: its write stands once the reset fails, so
		// its observers are owed it.
		st.notify(ctx, storage.Change{}, model.Issue{ID: epic})
		// A closed connection fails every statement after it, the reset's
		// included; what the close itself reports is the pool's, not the
		// batch's, and this test does not pin it.
		_ = st.db.Close()
		return errors.New("injected: refused")
	})
	if len(heard) != 1 || heard[0] != epic {
		t.Errorf("observers heard %v; want the change whose write stands after the failed reset, %s", heard, epic)
	}
	written, err := refusedBatch(result, err)
	var resetFailed *RefusedBatchError
	if !errors.As(err, &resetFailed) || resetFailed.ResetErr == nil {
		t.Fatalf("error = %v; want a *RefusedBatchError whose reset failed", err)
	}
	if resetFailed.Head != head {
		t.Errorf("Head = %s; want %s, the head before the batch", resetFailed.Head, head)
	}
	if len(written.Created) != 1 || len(written.Updated) != 1 {
		t.Errorf("result = %+v; want what stays written, not the zero result", written)
	}
	for _, want := range []string{"injected: refused", head, "1 creates [test-kept]", "1 updates [" + epic + "]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err.Error(), want)
		}
	}
	facts, _ := resetFailed.EventRecord()
	if facts["import.reset_to"] != head || facts["import.reset_error"] == "" {
		t.Errorf("EventRecord() = %v; want import.reset_to %s and the reset's error", facts, head)
	}
}
