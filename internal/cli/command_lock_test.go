package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func TestRemoveStaleCommandLockFileRemovesDeadOwnerImmediately(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-command.lock")
	if err := os.WriteFile(lockPath, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commandLockPIDRunning
	commandLockPIDRunning = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("pid probe = %d, want 12345", pid)
		}
		return false, nil
	}
	t.Cleanup(func() { commandLockPIDRunning = originalProbe })

	if err := removeStaleCommandLockFile(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommandLockFile() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists, stat err = %v", err)
	}
}

func TestRemoveStaleCommandLockFileKeepsFreshLiveOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-command.lock")
	if err := os.WriteFile(lockPath, []byte("42\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commandLockPIDRunning
	commandLockPIDRunning = func(pid int) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { commandLockPIDRunning = originalProbe })

	if err := removeStaleCommandLockFile(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommandLockFile() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should remain for live owner, stat err = %v", err)
	}
}

func TestRemoveStaleCommandLockFileKeepsFreshMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-command.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commandLockPIDRunning
	commandLockPIDRunning = func(pid int) (bool, error) {
		t.Fatalf("commandLockPIDRunning should not be called for malformed lock content")
		return false, nil
	}
	t.Cleanup(func() { commandLockPIDRunning = originalProbe })

	if err := removeStaleCommandLockFile(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommandLockFile() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("fresh malformed lock should remain, stat err = %v", err)
	}
}

func TestRemoveStaleCommandLockFileRemovesStaleMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-command.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}

	if err := removeStaleCommandLockFile(lockPath, time.Minute); err != nil {
		t.Fatalf("removeStaleCommandLockFile() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale malformed lock should be removed, stat err = %v", err)
	}
}

func TestAcquireWorkspaceCommandLockReclaimsDeadOwner(t *testing.T) {
	databasePath := t.TempDir()
	lockPath := filepath.Join(databasePath, ".links-command.lock")
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commandLockPIDRunning
	commandLockPIDRunning = func(pid int) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { commandLockPIDRunning = originalProbe })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := acquireWorkspaceCommandLock(ctx, databasePath)
	if err != nil {
		t.Fatalf("acquireWorkspaceCommandLock() error = %v", err)
	}
	release()

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
	}
}

func TestRunWithWorkspaceBlocksOnWorkspaceCommandLock(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")

	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	lockCtx, cancelLock := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelLock()
	release, err := acquireWorkspaceCommandLock(lockCtx, ws.DatabasePath)
	if err != nil {
		t.Fatalf("acquireWorkspaceCommandLock() error = %v", err)
	}
	defer release()

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	commandCtx, cancelCommand := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCommand()

	called := false
	err = runWithWorkspace(commandCtx, []string{"sync", "status"}, false, func(workspace.Info) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("runWithWorkspace() error = nil, want lock wait failure")
	}
	if commandCtx.Err() == nil {
		t.Fatalf("runWithWorkspace() error = %v, want context timeout", err)
	}
	if called {
		t.Fatal("runWithWorkspace() entered command body while lock was held")
	}
}
