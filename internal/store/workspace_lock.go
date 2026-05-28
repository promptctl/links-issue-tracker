package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// [LAW:single-enforcer] Workspace-exclusivity lock acquisition lives here so
// the contract — "no Store may be open while the Dolt directory is rotated by
// lit snapshots restore" — is enforced at exactly one boundary. Store.Open /
// Store.OpenForRead acquire shared holds; LockWorkspaceExclusive is the only
// way to take an exclusive hold and is reserved for callers that swap the
// Dolt directory wholesale.
//
// [LAW:dataflow-not-control-flow] Variability between shared and exclusive
// modes lives in the (exclusive, maxAttempts, delay) arguments threaded into
// acquireWorkspaceLock; the acquisition sequence (OpenFile, tryLockFile,
// retry-or-error) is the same every call.
//
// [LAW:locality-or-seam] The lock primitive is platform-specific (POSIX
// flock(2) vs. Win32 LockFileEx) and lives in workspace_lock_posix.go and
// workspace_lock_windows.go behind a typed seam. Adding a new platform is
// a parallel-but-isolated change — no edit to this file or to callers.
// The seam shape: tryLockFile(file, exclusive) returns nil on success,
// errLockWouldBlock on contention, or another error on real failure;
// unlockFile(file) releases the hold.

// ErrWorkspaceBusy is the sentinel every workspace-lock contention error
// wraps. Callers detect contention with errors.Is(err, ErrWorkspaceBusy)
// regardless of the specific operator-facing message attached.
//
// [LAW:one-source-of-truth] One sentinel for "lock is held by someone else";
// the wrapping messages differ to give context-appropriate guidance, but the
// programmatic discriminator is uniform.
var ErrWorkspaceBusy = errors.New("workspace busy")

// errLockWouldBlock is the internal seam between the platform-neutral
// acquisition loop and the platform-specific tryLockFile. The loop converts
// it into ErrWorkspaceBusy after retries are exhausted; any other error from
// tryLockFile is a real failure surfaced immediately.
var errLockWouldBlock = errors.New("lock would block")

const (
	// ~5s wall-clock cap: 100 attempts with 99 inter-attempt sleeps of 50ms
	// (the loop skips the sleep after the final attempt because there's
	// nothing to wait for).
	workspaceSharedRetryAttempts = 100
	workspaceSharedRetryDelay    = 50 * time.Millisecond
)

// WorkspaceLockPath returns the workspace-exclusivity lock path for a Dolt
// root directory. Sits at <dirname(databasePath)>/.links-workspace.lock — the
// same sibling-of-dolt-dir position as the commit lock — so lit snapshots
// restore (which renames the Dolt directory) does not move the lock file out
// from under concurrent acquirers.
//
// [LAW:one-source-of-truth] One naming convention for the workspace-busy lock;
// any callsite that needs the path reads it from this function.
func WorkspaceLockPath(databasePath string) string {
	cleaned := filepath.Clean(databasePath)
	return filepath.Join(filepath.Dir(cleaned), ".links-workspace.lock")
}

// acquireWorkspaceShared takes a shared hold on the workspace lock for the
// lifetime of a Store. Released when the returned func is called. Retries
// briefly (~5s) when an exclusive holder is active so a casual concurrent
// lit snapshots restore does not paper-cut every reader; surfaces a clear
// "workspace busy" error after the budget elapses.
func acquireWorkspaceShared(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, false, workspaceSharedRetryAttempts, workspaceSharedRetryDelay)
	if errors.Is(err, ErrWorkspaceBusy) {
		// [LAW:no-silent-fallbacks] Wrap the original error (which may
		// itself be an errors.Join containing close-side diagnostics
		// from joinWithClose) instead of replacing with a fresh sentinel.
		// errors.Is(err, ErrWorkspaceBusy) continues to detect contention;
		// any additional diagnostics survive.
		return nil, fmt.Errorf("lit snapshots restore is rotating the Dolt directory; retry after it completes: %w", err)
	}
	return release, err
}

// LockWorkspaceExclusive takes an exclusive hold for the duration of an
// operation that swaps the Dolt directory wholesale (i.e. lit snapshots
// restore). Refuses immediately on contention with any shared holder — the
// operator chose to run restore knowing the workspace is shared, so waiting
// would hide the conflict instead of surfacing it.
//
// [LAW:single-enforcer] Exported so the snapshots-restore command can take the
// hold without reconstructing the lock path; no other code should call this.
func LockWorkspaceExclusive(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, true, 1, 0)
	if errors.Is(err, ErrWorkspaceBusy) {
		return nil, fmt.Errorf("another lit process is using this workspace; close other lit commands and retry: %w", err)
	}
	return release, err
}

func acquireWorkspaceLock(ctx context.Context, doltRootDir string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, error) {
	lockPath := WorkspaceLockPath(doltRootDir)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("ensure workspace lock dir: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = tryLockFile(file, exclusive)
		if err == nil {
			fd := file
			// [LAW:no-silent-fallbacks] Both unlock and close failures
			// matter (FD leak; lock stuck held) so the release contract
			// surfaces them jointly via errors.Join instead of picking one.
			return func() error {
				var unlockErr error
				if e := unlockFile(fd); e != nil {
					unlockErr = fmt.Errorf("release workspace lock: %w", e)
				}
				var closeErr error
				if e := fd.Close(); e != nil {
					closeErr = fmt.Errorf("close workspace lock fd: %w", e)
				}
				return errors.Join(unlockErr, closeErr)
			}, nil
		}
		if !errors.Is(err, errLockWouldBlock) {
			return nil, joinWithClose(fmt.Errorf("lock workspace: %w", err), file)
		}
		if attempt+1 == maxAttempts {
			break
		}
		if waitErr := waitWithContext(ctx, delay); waitErr != nil {
			return nil, joinWithClose(waitErr, file)
		}
	}
	return nil, joinWithClose(ErrWorkspaceBusy, file)
}

// joinWithClose closes the lock file and returns the primary error joined
// with any close error. Used on every failure path inside
// acquireWorkspaceLock so an FD leak / close-time error stays observable
// alongside the failure that triggered the release.
//
// [LAW:no-silent-fallbacks] A leaked FD or a close error is real signal —
// silently dropping it (`_ = file.Close()`) hid debugging information on
// the exact paths that are hardest to diagnose.
func joinWithClose(primary error, file *os.File) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(primary, fmt.Errorf("close workspace lock fd: %w", closeErr))
	}
	return primary
}
