package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// assertCommitLockFree proves the commit flock at lockPath is not held: a
// non-blocking exclusive probe must acquire. The lock FILE persisting is the
// normal flock shape — absence of a kernel hold, not absence of the path, is
// what "released" means. [LAW:one-source-of-truth] one probe helper, so every
// release assertion means the same thing.
func assertCommitLockFree(t *testing.T, lockPath string) {
	t.Helper()
	release, err := acquireStoreLock(context.Background(), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("commit lock still held: probe error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("probe release error = %v", err)
	}
}

// TestPanicDuringMutationReleasesLock verifies that withMutation's deferred
// rollback and lock release fire even when the mutation function panics.
// Without defer, a panic would leave the flock held for the Store's lifetime,
// blocking all future mutations.
func TestPanicDuringMutationReleasesLock(t *testing.T) {
	t.Parallel()
	st := openIssueStore(t, context.Background())

	// withMutation panics inside the mutation fn.
	func() {
		defer func() {
			_ = recover()
		}()
		_ = st.withMutation(context.Background(), "panic-test", func(ctx context.Context, tx *sql.Tx) error {
			panic("simulated mutation panic")
		})
	}()

	assertCommitLockFree(t, st.commitLockPath)

	// A subsequent mutation must succeed, proving the lock was released.
	_, err := st.CreateIssue(context.Background(), CreateIssueInput{Prefix: "test",
		Title:     "Post-panic issue",
		Topic:     "crash",
		IssueType: "task",
		Priority:  0,
	})
	if err != nil {
		t.Fatalf("CreateIssue after panic error = %v", err)
	}
}

// TestPanicDuringWithCommitLockReleasesLock verifies that withCommitLock's
// deferred release fires even when the enclosed operation panics.
func TestPanicDuringWithCommitLockReleasesLock(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	func() {
		defer func() {
			_ = recover()
		}()
		_ = s.withCommitLock(context.Background(), func(ctx context.Context) error {
			panic("simulated operation panic")
		})
	}()

	assertCommitLockFree(t, lockPath)
}

// Live-holder non-eviction and dead-residue tolerance are covered by
// TestAcquireCommitLockNeverEvictsLiveHolderByAge and
// TestAcquireCommitLockIgnoresDeadResidue in commit_lock_test.go.
// [LAW:one-source-of-truth] one canonical assertion per behavior.

// TestWithMutationCommitWorkingSetReentrantPath verifies that withMutation's
// post-tx call to commitWorkingSet re-enters withCommitLock and short-circuits
// correctly because the context already carries the lock marker. CreateIssue
// only succeeds end-to-end when the re-entrant path completes without
// deadlocking or attempting to take the file lock a second time.
func TestWithMutationCommitWorkingSetReentrantPath(t *testing.T) {
	t.Parallel()
	st := openIssueStore(t, context.Background())

	// CreateIssue goes through withMutation, which:
	// 1. acquires commit lock
	// 2. begins tx, runs fn, commits tx
	// 3. calls commitWorkingSet (which re-enters withCommitLock — short-circuits)
	// If any step fails, CreateIssue returns an error.
	issue, err := st.CreateIssue(context.Background(), CreateIssueInput{Prefix: "test",
		Title:     "Commit path exercise",
		Topic:     "crash",
		IssueType: "task",
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if issue.ID == "" {
		t.Fatal("CreateIssue() returned empty ID")
	}

	// Verify the issue is readable.
	got, err := st.GetIssue(context.Background(), issue.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != issue.Title {
		t.Fatalf("GetIssue() title = %q, want %q", got.Title, issue.Title)
	}

	// Lock must not be held.
	assertCommitLockFree(t, st.commitLockPath)
}

// TestReentrantWithCommitLockShortCircuits verifies that calling withCommitLock
// from within a held lock is a no-op acquisition (the context already carries
// the lock marker and the release is a no-op).
func TestReentrantWithCommitLockShortCircuits(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	err := s.withCommitLock(context.Background(), func(ctx context.Context) error {
		// Nested call should short-circuit: no deadlock, no second acquisition.
		return s.withCommitLock(ctx, func(ctx context.Context) error {
			// Verify the context still carries the marker.
			if ctx.Value(commitLockContextKey{}) != true {
				return errors.New("nested context missing commit lock marker")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("nested withCommitLock() error = %v", err)
	}

	// The outer release must have freed the hold.
	assertCommitLockFree(t, lockPath)
}

// TestAcquireCommitLockContextCancellation verifies that a cancelled context
// prevents lock acquisition rather than blocking out the full retry budget
// against a live holder.
func TestAcquireCommitLockContextCancellation(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	// Hold the lock with a live in-test holder.
	release, err := acquireStoreLock(context.Background(), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("holder acquisition error = %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Fatalf("holder release error = %v", err)
		}
	}()

	// Cancel the context immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = s.acquireCommitLock(ctx)
	if err == nil {
		t.Fatal("acquireCommitLock() with cancelled context succeeded, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireCommitLock() error = %v, want context.Canceled", err)
	}
}
