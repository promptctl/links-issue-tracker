package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// assertBatchLeftNoScratch fails unless the session is back on master and no
// batch branch is left: a batch that ended, however it ended, leaves only the
// branch it started from.
func assertBatchLeftNoScratch(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	if branch, err := activeBranch(ctx, st.db); err != nil || branch != "master" {
		t.Fatalf("active branch = %q (%v); want master, the branch the batch started from", branch, err)
	}
	if left := branchesUnder(t, ctx, st, batchScratchPrefix); len(left) != 0 {
		t.Fatalf("batch branches left = %v; want none", left)
	}
}

// createTwo is a batch body making two tasks under epic, each its own write.
func createTwo(st *Store, epic string) func(ctx context.Context, result *storage.BulkApplyResult) error {
	return func(ctx context.Context, result *storage.BulkApplyResult) error {
		for _, title := range []string{"first", "second"} {
			issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", ParentID: epic})
			if err != nil {
				return err
			}
			result.Created = append(result.Created, storage.NewIDMapping("", issue.ID))
		}
		return nil
	}
}

// failOnFire returns a commit hook that fails the nth commit with err and lets
// every other commit through.
func failOnFire(n int, err error) func() error {
	fires := 0
	return func() error {
		fires++
		if fires == n {
			return err
		}
		return nil
	}
}

func gcContention() error {
	return transientGCContentionError{err: errors.New("simulated: this connection was established when this server performed an online garbage collection. please reconnect.")}
}

func countTasks(t *testing.T, ctx context.Context, st *Store) int {
	t.Helper()
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{IssueTypes: []model.IssueType{model.TypeTask}})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	return len(issues)
}

// A landed batch moves the branch it started from by exactly its own writes,
// one commit each, and leaves no scratch branch: the one it wrote on is
// recorded as finished.
func TestALandedBatchMovesItsBranchByItsWritesAndLeavesNoScratch(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	result, landing, err := withBatch(ctx, st, createTwo(st, epic))
	if err != nil {
		t.Fatalf("withBatch() error = %v", err)
	}
	if len(result.Created) != 2 || !landing.Landed {
		t.Fatalf("result = %+v, landing %+v; want both creates, landed", result, landing)
	}
	landed, err := countCommitsInRange(ctx, st.db, landing.From, headCommit(t, ctx, st))
	if err != nil {
		t.Fatal(err)
	}
	if landed != 2 {
		t.Fatalf("commits above the head the batch started from = %d; want 2, one per write", landed)
	}
	assertBatchLeftNoScratch(t, ctx, st)
	deletes, looked := st.ScratchDeletes()
	if !looked || len(deletes) != 1 || deletes[0].Cause != storage.ScratchFinished || !strings.HasPrefix(deletes[0].Branch, batchScratchPrefix) || deletes[0].Err != nil {
		t.Fatalf("ScratchDeletes() = %+v, looked %v; want the batch's own branch, finished", deletes, looked)
	}
}

// A write inside a batch that rotates the connection resumes on the batch's
// branch, so the batch lands whole, and a batch refused after that rotation
// still writes nothing: the rotated write never reached the branch the batch
// started from.
func TestARotationInsideABatchResumesOnItsBranch(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := map[bool]string{false: "lands", true: "refused"}[refuse]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, epic := openBatchStore(t, ctx)
			head := headCommit(t, ctx, st)
			// Fire 1 is the stranded-writes commit at the batch's start, 2 the
			// first create's, 3 the second's.
			st.commitWorkingSetHookForTest = failOnFire(3, gcContention())
			body := createTwo(st, epic)
			_, _, err := withBatch(ctx, st, func(ctx context.Context, result *storage.BulkApplyResult) error {
				if err := body(ctx, result); err != nil {
					return err
				}
				if branch, err := activeBranch(ctx, st.db); err != nil || !strings.HasPrefix(branch, batchScratchPrefix) {
					t.Errorf("active branch after the rotated write = %q (%v); want the batch's branch", branch, err)
				}
				if refuse {
					return errors.New("injected: refused")
				}
				return nil
			})
			st.commitWorkingSetHookForTest = nil
			assertBatchLeftNoScratch(t, ctx, st)
			if refuse {
				if err == nil {
					t.Fatal("withBatch() succeeded; want the refusal")
				}
				if got := headCommit(t, ctx, st); got != head {
					t.Fatalf("head after the refused batch = %s; want %s", got, head)
				}
				if n := countTasks(t, ctx, st); n != 0 {
					t.Fatalf("tasks after the refused batch = %d; want none", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("withBatch() error = %v; want the rotated write retried on the batch's branch", err)
			}
			if n := countTasks(t, ctx, st); n != 2 {
				t.Fatalf("tasks after the batch = %d; want 2", n)
			}
		})
	}
}

// A landing that meets GC contention is retried as every write is, and its
// rotation resumes on the branch it lands on — here not the branch a fresh
// connection opens on — so the batch lands there.
func TestABatchLandingRetriesPastGCContention(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	if err := st.checkoutNewAt(ctx, "work", headCommit(t, ctx, st)); err != nil {
		t.Fatal(err)
	}
	st.landHookForTest = failOnFire(1, gcContention())
	result, landing, err := withBatch(ctx, st, createTwo(st, epic))
	st.landHookForTest = nil
	if err != nil {
		t.Fatalf("withBatch() error = %v; want the contended landing retried", err)
	}
	head := landing.From
	if len(result.Created) != 2 || countTasks(t, ctx, st) != 2 {
		t.Fatalf("result = %+v; want both creates landed", result)
	}
	if branch, err := activeBranch(ctx, st.db); err != nil || branch != "work" {
		t.Fatalf("active branch = %q (%v); want work, the branch the batch started from", branch, err)
	}
	if master, err := commitHashOfRef(ctx, st.db, "master"); err != nil || master != head {
		t.Fatalf("master = %s (%v); want %s: the landing belongs on work", master, err, head)
	}
}

// A batch that cannot land writes nothing, returns the zero result beside an
// error naming the landing, and its observers hear nothing.
func TestABatchThatCannotLandWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	var heard []string
	st.ObserveChanges(func(_ storage.Change, issue model.Issue) { heard = append(heard, issue.ID) })
	st.landHookForTest = func() error { return errors.New("injected: cannot land") }
	result, landing, err := withBatch(ctx, st, createTwo(st, epic))
	st.landHookForTest = nil
	if landing.Landed {
		t.Errorf("landing = %+v; want not landed", landing)
	}
	if err == nil || !strings.Contains(err.Error(), "land") || !strings.Contains(err.Error(), "injected: cannot land") {
		t.Fatalf("withBatch() error = %v; want the landing's failure", err)
	}
	var refused *storage.RefusedBatchError
	if errors.As(err, &refused) {
		t.Errorf("error = %v; want a landing failure, not a refusal", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("result = %+v; want the zero result", result)
	}
	if len(heard) != 0 {
		t.Errorf("observers heard %v; want nothing", heard)
	}
	if got := headCommit(t, ctx, st); got != head {
		t.Fatalf("head = %s; want %s, the head before the batch", got, head)
	}
	assertBatchLeftNoScratch(t, ctx, st)
}

// A batch does not start on a scratch branch, a batch's or any other run's: a
// scratch branch is deleted when its run ends, and the batch's sweep would
// delete another batch's live branch.
func TestABatchRefusesToStartOnAScratchBranch(t *testing.T) {
	for _, prefix := range scratchPrefixes {
		t.Run(prefix, func(t *testing.T) {
			ctx := context.Background()
			st, epic := openBatchStore(t, ctx)
			head := headCommit(t, ctx, st)
			outer := prefix + "outer"
			if err := st.checkoutNewAt(ctx, outer, head); err != nil {
				t.Fatal(err)
			}
			_, landing, err := withBatch(ctx, st, createTwo(st, epic))
			if err == nil || !strings.Contains(err.Error(), outer) || landing.Landed {
				t.Fatalf("withBatch() = %+v, %v; want a refusal naming %s", landing, err, outer)
			}
			if left := branchesUnder(t, ctx, st, prefix); left["outer"] == "" {
				t.Fatalf("branches under %s = %v; want %s untouched", prefix, left, outer)
			}
		})
	}
}

// A batch whose caller is cancelled while it lands lands whole: the reset
// that moves its branch runs to the end, so a batch never reports nothing
// landed beside a branch that moved.
func TestABatchCancelledWhileLandingLandsWhole(t *testing.T) {
	st, epic := openBatchStore(t, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.landHookForTest = func() error {
		cancel()
		return nil
	}
	result, landing, err := withBatch(ctx, st, createTwo(st, epic))
	st.landHookForTest = nil
	if err != nil || !landing.Landed || len(result.Created) != 2 {
		t.Fatalf("withBatch() = %+v, %+v, %v; want both creates landed", result, landing, err)
	}
	if n := countTasks(t, context.Background(), st); n != 2 {
		t.Fatalf("tasks after the batch = %d; want 2", n)
	}
	assertBatchLeftNoScratch(t, context.Background(), st)
}

// Creating a batch's branch that meets GC contention is retried as every write
// is, so the batch lands whole.
func TestABatchRetriesCreatingItsBranchPastGCContention(t *testing.T) {
	ctx := context.Background()
	st, epic := openBatchStore(t, ctx)
	st.createScratchHookForTest = failOnFire(1, gcContention())
	result, landing, err := withBatch(ctx, st, createTwo(st, epic))
	st.createScratchHookForTest = nil
	if err != nil || !landing.Landed || len(result.Created) != 2 {
		t.Fatalf("withBatch() = %+v, %+v, %v; want the contended creation retried and the batch landed", result, landing, err)
	}
	if n := countTasks(t, ctx, st); n != 2 {
		t.Fatalf("tasks after the batch = %d; want 2", n)
	}
	assertBatchLeftNoScratch(t, ctx, st)
}

// A fresh connection resumes on the branch the session was last switched to,
// and before any switch stays on the branch it opened on.
func TestAReconnectResumesTheCheckedOutBranch(t *testing.T) {
	ctx := context.Background()
	st, _ := openBatchStore(t, ctx)
	if err := st.reconnect(ctx); err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	if branch, err := activeBranch(ctx, st.db); err != nil || branch != "master" {
		t.Fatalf("active branch after a reconnect before any switch = %q (%v); want master", branch, err)
	}
	if err := st.checkoutNewAt(ctx, "elsewhere", headCommit(t, ctx, st)); err != nil {
		t.Fatal(err)
	}
	if err := st.reconnect(ctx); err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	if branch, err := activeBranch(ctx, st.db); err != nil || branch != "elsewhere" {
		t.Fatalf("active branch after a reconnect = %q (%v); want elsewhere, the branch last switched to", branch, err)
	}
}

// A scratch run whose return to its home branch fails recovers by one
// reconnect, which resumes on home — here not the branch a fresh connection
// opens on — and deletes its branches from there.
func TestLeavingScratchRecoversAFailedReturnByOneReconnect(t *testing.T) {
	ctx := context.Background()
	st, _ := openBatchStore(t, ctx)
	head := headCommit(t, ctx, st)
	if err := execProcedureDiscard(ctx, st.db, "DOLT_BRANCH", "home", head); err != nil {
		t.Fatal(err)
	}
	scratch := batchScratchPrefix + "stranded"
	if err := st.checkoutNewAt(ctx, scratch, head); err != nil {
		t.Fatal(err)
	}
	// A closed connection fails the switch back.
	_ = st.db.Close()
	if err := st.leaveScratch(ctx, "home", []string{scratch}); err != nil {
		t.Fatalf("leaveScratch() error = %v; want the failed return recovered", err)
	}
	if branch, err := activeBranch(ctx, st.db); err != nil || branch != "home" {
		t.Fatalf("active branch = %q (%v); want home", branch, err)
	}
	// The recovered run deletes its own branch, so no later sweep reads it as
	// one a killed run left.
	if left := branchesUnder(t, ctx, st, batchScratchPrefix); len(left) != 0 {
		t.Fatalf("batch branches left = %v; want none", left)
	}
	deletes, looked := st.ScratchDeletes()
	if !looked || len(deletes) != 1 || deletes[0].Branch != scratch || deletes[0].Cause != storage.ScratchFinished || deletes[0].Err != nil {
		t.Fatalf("ScratchDeletes() = %+v, looked %v; want %s, finished", deletes, looked, scratch)
	}
}
