package store

import (
	"context"
	"errors"
	"testing"

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

// A refused file returns the zero result, since nothing it wrote stands, and
// its error is the refusal, classified as the refusal. It records the commit
// the file started from, so a refusal at a write reads apart from one the
// gate made before writing anything.
func TestARefusedImportRecordsTheCommitItStartedFrom(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	result, err := st.BulkApply(ctx, "test", "ada", refusedFile(epic))
	if len(result.Created) != 0 {
		t.Errorf("result = %+v; want the zero result: the create before the refusal never landed", result)
	}
	var refused *storage.RefusedBatchError
	if !errors.As(err, &refused) {
		t.Fatalf("BulkApply() error = %v; want a *storage.RefusedBatchError", err)
	}
	if notFound := (storage.NotFoundError{}); !errors.As(err, &notFound) {
		t.Errorf("BulkApply() error = %v; want it still classified as the missing parent's not-found", err)
	}
	if err.Error() != refused.Cause.Error() {
		t.Errorf("error text = %q; want the refusal's own %q", err.Error(), refused.Cause.Error())
	}
	facts, _ := refused.EventRecord()
	if facts["import.head"] != head {
		t.Errorf("EventRecord() = %v; want import.head %s", facts, head)
	}
}

// A store has looked for a staged write only once a batch ran, so its event
// tells "found none" from "never looked".
func TestABatchLooksForStrandedWrites(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	if commits, looked := st.StrandedWritesCommits(); looked {
		t.Fatalf("StrandedWritesCommits() = %v, looked; want no look before any batch", commits)
	}
	if _, err := st.BulkApply(ctx, "test", "ada", refusedFile(epic)); err == nil {
		t.Fatal("BulkApply() succeeded; want the missing parent refused")
	}
	if commits, looked := st.StrandedWritesCommits(); !looked || len(commits) != 0 {
		t.Fatalf("StrandedWritesCommits() = %v, looked %v; want a look that found none", commits, looked)
	}
}

// A batch refused because its caller gave up leaves the branch where it
// started, and the session on it: the cancellation that refused the batch
// cannot also stop its return from the scratch branch.
func TestABatchRefusedByCancellationLeavesTheBranchWhereItStarted(t *testing.T) {
	st, epic := openBatchStore(t, context.Background())
	head := headCommit(t, context.Background(), st)
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := withBatch(ctx, st, func(ctx context.Context, _ *storage.BulkApplyResult) error {
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
	assertBatchLeftNoScratch(t, context.Background(), st)
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

// A batch's start is retried past transient GC contention, as every write is:
// nothing is written yet, so a contended commit of the stranded writes does
// not refuse the file.
func TestABatchStartRetriesPastGCContention(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	fires := 0
	st.commitWorkingSetHookForTest = func() error {
		fires++
		if fires == 1 {
			return transientGCContentionError{err: errors.New("simulated: this connection was established when this server performed an online garbage collection. please reconnect.")}
		}
		return nil
	}
	title, topic, taskType := "lands", "topic", "task"
	result, err := st.BulkApply(ctx, "test", "ada", []storage.BulkIssueSpec{{Title: &title, Topic: &topic, IssueType: &taskType, Parent: &epic}})
	st.commitWorkingSetHookForTest = nil
	if err != nil {
		t.Fatalf("BulkApply() error = %v; want the contended start retried", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("result = %+v; want the one create", result)
	}
}
