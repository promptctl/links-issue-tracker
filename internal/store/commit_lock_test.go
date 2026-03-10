package store

import (
	"context"
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
