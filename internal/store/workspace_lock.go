package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
// modes lives in the (lockType, maxAttempts, delay) arguments threaded into
// acquireWorkspaceLock; the acquisition sequence (OpenFile, Flock|LOCK_NB,
// retry-or-error) is the same every call.
//
// Lock primitive: POSIX flock(2) via syscall.Flock. Multiple shared holders
// coexist (LOCK_SH on independent file descriptors), an exclusive holder
// blocks every other holder, and LOCK_NB makes contention observable instead
// of latent. Cross-process semantics are enforced by the kernel; per-Store
// FDs make the same semantics hold inside one process for free.

// ErrWorkspaceBusy is the sentinel every workspace-lock contention error
// wraps. Callers detect contention with errors.Is(err, ErrWorkspaceBusy)
// regardless of the specific operator-facing message attached.
//
// [LAW:one-source-of-truth] One sentinel for "lock is held by someone else";
// the wrapping messages differ to give context-appropriate guidance, but the
// programmatic discriminator is uniform.
var ErrWorkspaceBusy = errors.New("workspace busy")

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

// acquireWorkspaceShared takes a shared (LOCK_SH) hold on the workspace lock
// for the lifetime of a Store. Released when the returned func is called.
// Retries briefly (~5s) when an exclusive holder is active so a casual
// concurrent lit snapshots restore does not paper-cut every reader; surfaces
// a clear "workspace busy" error after the budget elapses.
func acquireWorkspaceShared(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, syscall.LOCK_SH, workspaceSharedRetryAttempts, workspaceSharedRetryDelay)
	if errors.Is(err, ErrWorkspaceBusy) {
		return nil, fmt.Errorf("%w: lit snapshots restore is rotating the Dolt directory; retry after it completes", ErrWorkspaceBusy)
	}
	return release, err
}

// LockWorkspaceExclusive takes an exclusive (LOCK_EX) hold for the duration of
// an operation that swaps the Dolt directory wholesale (i.e. lit snapshots
// restore). Refuses immediately on contention with any shared holder — the
// operator chose to run restore knowing the workspace is shared, so waiting
// would hide the conflict instead of surfacing it.
//
// [LAW:single-enforcer] Exported so the snapshots-restore command can take the
// hold without reconstructing the lock path; no other code should call this.
func LockWorkspaceExclusive(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, syscall.LOCK_EX, 1, 0)
	if errors.Is(err, ErrWorkspaceBusy) {
		return nil, fmt.Errorf("%w: another lit process is using this workspace; close other lit commands and retry", ErrWorkspaceBusy)
	}
	return release, err
}

func acquireWorkspaceLock(ctx context.Context, doltRootDir string, lockType, maxAttempts int, delay time.Duration) (func() error, error) {
	lockPath := WorkspaceLockPath(doltRootDir)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("ensure workspace lock dir: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err = syscall.Flock(int(file.Fd()), lockType|syscall.LOCK_NB)
		if err == nil {
			fd := file
			// [LAW:no-silent-fallbacks] Both unlock and close failures
			// matter (FD leak; lock stuck held) so the release contract
			// surfaces them jointly via errors.Join instead of picking one.
			return func() error {
				var flockErr error
				if e := syscall.Flock(int(fd.Fd()), syscall.LOCK_UN); e != nil {
					flockErr = fmt.Errorf("release workspace lock: %w", e)
				}
				var closeErr error
				if e := fd.Close(); e != nil {
					closeErr = fmt.Errorf("close workspace lock fd: %w", e)
				}
				return errors.Join(flockErr, closeErr)
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, joinWithClose(fmt.Errorf("flock workspace lock: %w", err), file)
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
