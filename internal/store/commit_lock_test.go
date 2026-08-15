package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveStaleCommitLockRemovesDeadOwnerImmediately(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("pid probe = %d, want 12345", pid)
		}
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockKeepsFreshLiveOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("42\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should remain for live owner, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockKeepsFreshMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		t.Fatalf("commitLockPIDRunning should not be called for malformed lock content")
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("fresh malformed lock should remain, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockRemovesStaleMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}

	if err := removeStaleCommitLock(lockPath, time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale malformed lock should be removed, stat err = %v", err)
	}
}

func TestAcquireCommitLockReclaimsDeadOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	s := &Store{commitLockPath: lockPath}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lockedCtx, release, err := s.acquireCommitLock(ctx)
	if err != nil {
		t.Fatalf("acquireCommitLock() error = %v", err)
	}
	if lockedCtx.Value(commitLockContextKey{}) != true {
		t.Fatalf("acquireCommitLock() did not set commit lock context value")
	}
	release()

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
	}
}

// TestWithMutationResumesAtVersioningAfterStagedCommit pins withMutation's
// phase resume: when the staging transaction has committed and only the
// versioning step (DOLT_COMMIT) fails transiently, the retry must re-run
// versioning alone. Re-running the mutation body against its own staged
// writes would double-apply it — for CreateIssue, the in-tx collision check
// would find the staged row and mint a SECOND issue under a higher nonce —
// so the discriminating assertion is that exactly one issue exists afterward.
func TestWithMutationResumesAtVersioningAfterStagedCommit(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	fires := 0
	st.commitWorkingSetHookForTest = func() error {
		fires++
		if fires == 1 {
			return transientGCContentionError{err: errors.New("simulated: this connection was established when this server performed an online garbage collection. please reconnect.")}
		}
		return nil
	}

	created, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "survives a versioning retry", Topic: "sync"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if created.ID == "" {
		t.Fatalf("CreateIssue() returned an empty id")
	}
	if fires < 2 {
		t.Fatalf("versioning step ran %d time(s); want the transient failure to have forced a retry", fires)
	}
	st.commitWorkingSetHookForTest = nil

	count, err := st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("LocalIssueCount() = %d, want exactly 1 — more means the retry re-ran the staged mutation body", count)
	}
}

// TestCommitWorkingSetOnceRendersStamp pins the commit-stamp flag rendering the
// reconcile's provenance replay depends on: a non-default Author lands verbatim
// as the commit's committer/email (not the session identity), a fixed past Date
// lands to the second, and AllowEmpty lands a commit even on a clean working
// set. The replay tests cannot see a dropped --author on their own — the
// original and the replayed commit share the session identity there — so this
// test stamps an identity the session does not have.
func TestCommitWorkingSetOnceRendersStamp(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	date := time.Date(2025, 3, 7, 9, 30, 45, 0, time.UTC)
	stamp := commitStamp{
		Message:    "stamped provenance probe",
		Date:       date,
		Author:     "prov-author <prov@example.test>",
		AllowEmpty: true,
	}
	if err := st.commitWorkingSetOnce(ctx, stamp); err != nil {
		t.Fatalf("commitWorkingSetOnce(stamp) error = %v", err)
	}

	var committer, email, message string
	var got time.Time
	err := st.db.QueryRowContext(ctx, `SELECT committer, email, date, message FROM dolt_log('HEAD') LIMIT 1`).Scan(&committer, &email, &got, &message)
	if err != nil {
		t.Fatalf("read stamped head: %v", err)
	}
	if committer != "prov-author" || email != "prov@example.test" {
		t.Fatalf("stamped author = %s <%s>, want prov-author <prov@example.test>", committer, email)
	}
	if !got.UTC().Equal(date) {
		t.Fatalf("stamped date = %s, want %s", got.UTC(), date)
	}
	if message != "stamped provenance probe" {
		t.Fatalf("stamped message = %q, want %q", message, stamp.Message)
	}
}
