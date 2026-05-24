package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkspaceLockSharedHoldersCoexist pins the contract that multiple Stores
// open against the same workspace each take their own shared (LOCK_SH) hold and
// none of them block another. Without this invariant, two concurrent readers
// (e.g. agent A running lit ls while agent B runs lit show) would serialize on
// startup or, worse, the second would error with workspace-busy.
func TestWorkspaceLockSharedHoldersCoexist(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	defer first.Close()

	// Second Open against the same workspace must succeed without waiting on
	// the first's shared hold to release.
	done := make(chan error, 1)
	go func() {
		s, err := Open(ctx, doltRoot, "test-workspace-id")
		if err != nil {
			done <- err
			return
		}
		_ = s.Close()
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Open() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Open() did not complete within 10s — shared holds must coexist")
	}
}

// TestWorkspaceLockExclusiveRefusesWhileSharedHeld pins the headline acceptance
// criterion: while a Store holds a shared workspace lock, an attempt to take
// the exclusive hold (i.e. lit snapshots restore) refuses immediately with a
// "workspace busy" error — not a query error, not a silent corruption, not a
// hang.
//
// [LAW:single-enforcer] One exclusive holder at a time; this test pins that
// the refusal contract is owned by LockWorkspaceExclusive.
func TestWorkspaceLockExclusiveRefusesWhileSharedHeld(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	release, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err == nil {
		_ = release()
		t.Fatal("LockWorkspaceExclusive succeeded while a Store held a shared hold; expected refusal")
	}
	if !strings.Contains(err.Error(), "workspace busy") {
		t.Fatalf("error %q must name the workspace-busy condition so the operator knows what to do", err.Error())
	}
}

// TestWorkspaceLockSharedRefusesWhileExclusiveHeld pins the reverse direction:
// while a holder has the exclusive lock (i.e. mid-restore), an attempt to open
// a Store must not succeed. The shared-side acquisition retries briefly and
// then refuses with a workspace-busy error naming the likely cause.
func TestWorkspaceLockSharedRefusesWhileExclusiveHeld(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Bootstrap the database so Open's only failure mode here is the lock.
	bootstrap, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("bootstrap Open() error = %v", err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatalf("bootstrap Close() error = %v", err)
	}

	release, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive() error = %v", err)
	}
	defer release()

	// Constrain the shared-side wait so this test stays bounded.
	shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = acquireWorkspaceShared(shortCtx, doltRoot)
	if err == nil {
		t.Fatal("acquireWorkspaceShared succeeded while exclusive held; expected refusal or context deadline")
	}
	// Either of two errors is acceptable: the context deadline fires before
	// the retry budget, or the retry budget elapses and the shared helper
	// returns its workspace-busy message. Both are observable refusals.
	if !strings.Contains(err.Error(), "workspace busy") && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("unexpected error from acquireWorkspaceShared: %v", err)
	}
}

// TestWorkspaceLockExclusiveReleasedAfterClose pins that Close releases the
// shared hold, so a subsequent restore can take the exclusive hold without
// any explicit quiesce step beyond closing the Store.
func TestWorkspaceLockExclusiveReleasedAfterClose(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	release, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive after Close error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release exclusive error = %v", err)
	}
}

// TestWorkspaceBusyErrorsWrapSentinel pins the contract that every
// workspace-busy error from the public helpers wraps ErrWorkspaceBusy so
// callers can detect contention with errors.Is — independent of the
// operator-facing message attached to the specific failure mode.
//
// [LAW:one-source-of-truth] One sentinel; the wrapping message varies for
// context, but the discriminator is uniform.
func TestWorkspaceBusyErrorsWrapSentinel(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	bootstrap, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("bootstrap Open() error = %v", err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatalf("bootstrap Close() error = %v", err)
	}

	// Hold exclusive to force a workspace-busy refusal from acquireWorkspaceShared.
	exclusive, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive() error = %v", err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, sharedErr := acquireWorkspaceShared(shortCtx, doltRoot)
	if sharedErr == nil {
		t.Fatal("acquireWorkspaceShared succeeded under exclusive hold; expected refusal")
	}
	if !errors.Is(sharedErr, ErrWorkspaceBusy) && !strings.Contains(sharedErr.Error(), "deadline exceeded") {
		t.Fatalf("acquireWorkspaceShared error %v does not wrap ErrWorkspaceBusy (and isn't a context deadline)", sharedErr)
	}
	_ = exclusive()

	// Now hold shared and verify LockWorkspaceExclusive's refusal wraps the sentinel.
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open under no contention error = %v", err)
	}
	defer st.Close()
	_, exclErr := LockWorkspaceExclusive(ctx, doltRoot)
	if exclErr == nil {
		t.Fatal("LockWorkspaceExclusive succeeded while shared held; expected refusal")
	}
	if !errors.Is(exclErr, ErrWorkspaceBusy) {
		t.Fatalf("LockWorkspaceExclusive error %v does not wrap ErrWorkspaceBusy", exclErr)
	}
}

// TestOpenForReadAcquiresLockBeforeStat pins the contract that OpenForRead
// takes the workspace shared lock BEFORE its database-exists stat — so a
// concurrent lit snapshots restore (which transiently renames the database
// dir away between rotate and install) cannot make OpenForRead return a
// false-negative "repository not initialized" error.
//
// The test simulates the transient state by:
//   1. Opening and closing a Store to bootstrap the workspace
//   2. Renaming the database dir away (mimicking restore's first rename)
//   3. Calling OpenForRead under a short context and asserting the error
//      shape: it must be a workspace-busy refusal (because the exclusive
//      lock is held), NOT a "not initialized" error.
func TestOpenForReadAcquiresLockBeforeStat(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	bootstrap, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("bootstrap Open() error = %v", err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatalf("bootstrap Close() error = %v", err)
	}

	// Hold the exclusive workspace lock — the shape lit snapshots restore
	// takes while it rotates the database directory.
	release, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockWorkspaceExclusive() error = %v", err)
	}
	defer release()

	// Rename the database directory away — mimics the transient
	// "directory absent" state between dbsnapshot.Restore's rotate-away
	// and install-snapshot calls. Without the fix, OpenForRead's stat
	// would observe ENOENT and return "repository not initialized".
	rotated := doltRoot + ".pre-restore-test"
	if err := os.Rename(doltRoot, rotated); err != nil {
		t.Fatalf("rotate dolt dir: %v", err)
	}
	defer func() { _ = os.Rename(rotated, doltRoot) }()

	shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = OpenForRead(shortCtx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("OpenForRead succeeded while exclusive workspace lock held; expected workspace-busy refusal")
	}
	if strings.Contains(err.Error(), "repository not initialized") {
		t.Fatalf("OpenForRead returned false-negative 'not initialized' instead of workspace-busy: %v", err)
	}
	if !strings.Contains(err.Error(), "workspace busy") && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("OpenForRead returned unexpected error shape (want workspace-busy or deadline): %v", err)
	}
}

// TestOpenSyncHoldsWorkspaceLock pins the contract that lit sync's long-lived
// Store also acquires the shared workspace lock — the same way Open and
// OpenForRead do. Without this, `lit sync ...` could hold an open Dolt
// connection while `lit snapshots restore` rotates the directory.
//
// [LAW:single-enforcer] All Store constructors that open long-lived Dolt
// connections route through the same workspace-lock contract; OpenSync is no
// exception even though it serves a different higher-level purpose.
func TestOpenSyncHoldsWorkspaceLock(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer st.Close()

	// While the sync Store is open, exclusive acquire must refuse.
	release, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err == nil {
		_ = release()
		t.Fatal("LockWorkspaceExclusive succeeded while OpenSync Store held shared; expected refusal")
	}
	if !strings.Contains(err.Error(), "workspace busy") {
		t.Fatalf("error %q must name workspace-busy", err.Error())
	}
}

// TestWorkspaceLockExclusiveSerializes pins that two concurrent exclusive
// acquisitions cannot both succeed — one wins, the other refuses immediately.
// Without this invariant, two concurrent lit snapshots restore commands could
// both rotate the database directory and lose data.
func TestWorkspaceLockExclusiveSerializes(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Initialize the parent dir by opening and closing a Store once. The lock
	// file lives in the workspace storage dir, so its parent must exist.
	bootstrap, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("bootstrap Open() error = %v", err)
	}
	if err := bootstrap.Close(); err != nil {
		t.Fatalf("bootstrap Close() error = %v", err)
	}

	first, err := LockWorkspaceExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("first exclusive acquire error = %v", err)
	}
	defer first()

	var (
		secondErr error
		wg        sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		release, err := LockWorkspaceExclusive(ctx, doltRoot)
		if err == nil {
			_ = release()
			secondErr = nil
			return
		}
		secondErr = err
	}()
	wg.Wait()
	if secondErr == nil {
		t.Fatal("second exclusive acquire succeeded while first held; expected refusal")
	}
	if !strings.Contains(secondErr.Error(), "workspace busy") {
		t.Fatalf("second error %q must name workspace-busy", secondErr.Error())
	}
}
