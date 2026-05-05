package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/errgroup"
)

// TestPanicDuringMutationReleasesLock verifies that withMutation's deferred
// rollback and lock release fire even when the mutation function panics.
// Without defer, a panic would leave the lock file on disk, blocking all
// future mutations.
func TestPanicDuringMutationReleasesLock(t *testing.T) {
	st := openIssueStore(t, context.Background())
	lockPath := st.commitLockPath

	// withMutation panics inside the mutation fn.
	func() {
		defer func() {
			_ = recover()
		}()
		_ = st.withMutation(context.Background(), "panic-test", func(ctx context.Context, tx *sql.Tx) error {
			panic("simulated mutation panic")
		})
	}()

	// Lock file must not exist after the panic is recovered.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists after panic: stat err = %v", err)
	}

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
// defer release() fires even when the enclosed operation panics.
func TestPanicDuringWithCommitLockReleasesLock(t *testing.T) {
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

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists after withCommitLock panic: stat err = %v", err)
	}
}

// Stale-lock reclamation by dead PID is covered by
// TestAcquireCommitLockReclaimsDeadOwner in commit_lock_test.go.
// Stale-lock reclamation by age (malformed owner) is covered by
// TestRemoveStaleCommitLockRemovesStaleMalformedOwner in commit_lock_test.go.
// [LAW:one-source-of-truth] one canonical assertion per behavior.

// TestWithMutationCommitWorkingSetReentrantPath verifies that withMutation's
// post-tx call to commitWorkingSet re-enters withCommitLock and short-circuits
// correctly because the context already carries the lock marker. CreateIssue
// only succeeds end-to-end when the re-entrant path completes without
// deadlocking or attempting to take the file lock a second time.
func TestWithMutationCommitWorkingSetReentrantPath(t *testing.T) {
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
	if _, err := os.Stat(st.commitLockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file exists after CreateIssue: stat err = %v", err)
	}
}

// TestLockFileContainsCurrentPID verifies that tryAcquireFileLock writes the
// current process PID to the lock file.
func TestLockFileContainsCurrentPID(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")

	locked, err := tryAcquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("tryAcquireFileLock() error = %v", err)
	}
	if !locked {
		t.Fatal("tryAcquireFileLock() returned locked=false")
	}

	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lock) error = %v", err)
	}
	expected := fmt.Sprintf("%d\n", os.Getpid())
	if string(content) != expected {
		t.Fatalf("lock content = %q, want %q", string(content), expected)
	}
	_ = os.Remove(lockPath)
}

// TestTryAcquireFileLockIsAtomicUnderRace verifies true atomicity: when N
// goroutines race to acquire the same lock path concurrently, exactly one
// observes locked=true and every other observes os.ErrExist. A check-then-
// create implementation (stat + create as separate syscalls) would let two
// racers both pass the check and both create the file, which this test
// catches under -race.
// [LAW:single-enforcer] file lock acquisition is the single atomic boundary.
func TestTryAcquireFileLockIsAtomicUnderRace(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")

	const racers = 32
	var winners atomic.Int32
	var existsErrs atomic.Int32

	eg, _ := errgroup.WithContext(context.Background())
	start := make(chan struct{})
	for range racers {
		eg.Go(func() error {
			<-start
			locked, err := tryAcquireFileLock(lockPath)
			if locked && err == nil {
				winners.Add(1)
				return nil
			}
			if !locked && errors.Is(err, os.ErrExist) {
				existsErrs.Add(1)
				return nil
			}
			return fmt.Errorf("unexpected racer outcome: locked=%v err=%v", locked, err)
		})
	}
	close(start)
	if err := eg.Wait(); err != nil {
		t.Fatalf("racer goroutine error = %v", err)
	}

	if got := winners.Load(); got != 1 {
		t.Fatalf("winners = %d, want exactly 1 (atomicity broken)", got)
	}
	if got := existsErrs.Load(); got != racers-1 {
		t.Fatalf("os.ErrExist count = %d, want %d", got, racers-1)
	}
	_ = os.Remove(lockPath)
}

// TestReentrantWithCommitLockShortCircuits verifies that calling withCommitLock
// from within a held lock is a no-op acquisition (the context already carries
// the lock marker and the release is a no-op).
func TestReentrantWithCommitLockShortCircuits(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	err := s.withCommitLock(context.Background(), func(ctx context.Context) error {
		// Nested call should short-circuit: no deadlock, no second lock file.
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

	// Lock file must be cleaned up.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists after re-entrant withCommitLock: stat err = %v", err)
	}
}

// TestAcquireCommitLockContextCancellation verifies that a cancelled context
// prevents lock acquisition rather than blocking indefinitely.
func TestAcquireCommitLockContextCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	// Hold the lock externally with current PID (live owner).
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// Cancel the context immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.acquireCommitLock(ctx)
	if err == nil {
		t.Fatal("acquireCommitLock() with cancelled context succeeded, want error")
	}
}

