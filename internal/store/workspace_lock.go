package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/filelock"
)

// [LAW:single-enforcer] Workspace-exclusivity lock acquisition lives here so
// the contract — "nobody may be reading the Dolt directory while it is
// displaced or rotated wholesale" — is enforced at exactly one boundary.
// Shared holds mark directory readers: everything that reads the directory's
// files, whether through an engine (store opens, raw dumps) or a plain file
// walk (the snapshot copy), via acquireWorkspaceShared in-package or
// LockWorkspaceShared outside it. The exclusive hold marks directory
// rotators — operations that displace, swap, or rebuild the directory itself
// — and LockWorkspaceExclusive is the only way to take one.
//
// [LAW:one-source-of-truth] Neither mode's caller roster is listed here: the
// callers of those two functions ARE the roster, and a prose copy of it
// drifts (a three-name list written in PR #379 was wrong before the PR
// merged — it missed lifeboat recover's heal).
//
// [LAW:dataflow-not-control-flow] Variability between shared and exclusive
// modes lives in the (exclusive, maxAttempts, delay) arguments threaded into
// acquireWorkspaceLock; the acquisition sequence (OpenFile, tryLockFile,
// retry-or-error) is the same every call.
//
// [LAW:locality-or-seam] The lock primitive (POSIX flock(2) vs. Win32
// LockFileEx) and its acquisition loop live in internal/filelock behind a
// typed seam shared with every other flock-backed coordination point (e.g.
// dbsnapshot's snapshot-producer beacon). This file keeps only the lock
// *meanings*: which path, which mode, which retry budget, and which
// operator guidance wraps contention.

// ErrWorkspaceBusy is the sentinel every workspace-lock contention error
// wraps. Callers detect contention with errors.Is(err, ErrWorkspaceBusy)
// regardless of the specific operator-facing message attached.
//
// [LAW:one-source-of-truth] One sentinel for "lock is held by someone else";
// the wrapping messages differ to give context-appropriate guidance, but the
// programmatic discriminator is uniform. filelock reports contention as a
// value, and acquireStoreLock is the one boundary that stamps it with this
// domain meaning.
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

// acquireWorkspaceShared takes a shared hold on the workspace lock for the
// lifetime of a Store. Released when the returned func is called. Retries
// briefly (~5s) when an exclusive holder is active so a casual concurrent
// lit snapshots restore does not paper-cut every reader; surfaces a clear
// "workspace busy" error after the budget elapses.
func acquireWorkspaceShared(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, false, workspaceSharedRetryAttempts, workspaceSharedRetryDelay)
	if errors.Is(err, ErrWorkspaceBusy) {
		// [LAW:no-silent-failure] Wrap the original error (which may
		// itself be an errors.Join containing close-side diagnostics
		// from joinWithClose) instead of replacing with a fresh sentinel.
		// errors.Is(err, ErrWorkspaceBusy) continues to detect contention;
		// any additional diagnostics survive.
		return nil, fmt.Errorf("a lit operation is rebuilding this workspace's Dolt directory (e.g. snapshots restore, an init backlog adopt, or lifeboat recover); retry after it completes: %w", err)
	}
	return release, err
}

// LockWorkspaceShared takes the same shared hold every Store open acquires,
// for a caller that reads the Dolt directory's files without opening a Store
// — i.e. the `lit snapshots new` copy. The hold coexists with other shared
// holders (ordinary readers stay unblocked) and contends with the exclusive
// hold of every directory rotator, so a file walk can never observe a
// directory mid-displacement. Shares acquireWorkspaceShared's brief retry so
// a transient rotation is waited out rather than paper-cutting the caller.
//
// [LAW:single-enforcer] Reader-vs-rotator exclusion has exactly one boundary
// — this lock, in shared mode. Before this export, the snapshot copy ran
// under only the commit lock (a writer-vs-writer gate on a different file),
// which an adopt's exclusive hold never contends with — the torn-snapshot
// race of links-sync-pgct.14.
func LockWorkspaceShared(ctx context.Context, doltRootDir string) (func() error, error) {
	return acquireWorkspaceShared(ctx, doltRootDir)
}

// LockWorkspaceExclusive takes an exclusive hold for the duration of an
// operation that displaces, swaps, or rebuilds the Dolt directory wholesale.
// Refuses immediately on contention with any shared holder — a rotation was
// requested knowing the workspace may be in use, so waiting would hide the
// conflict instead of surfacing it.
//
// [LAW:single-enforcer] Exported so CLI-layer rotators can take the hold
// without reconstructing the lock path. A caller that does not rotate the
// directory itself has no business with this mode — readers take the shared
// hold.
func LockWorkspaceExclusive(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, true, 1, 0)
	if errors.Is(err, ErrWorkspaceBusy) {
		return nil, fmt.Errorf("another lit process is using this workspace; close other lit commands and retry: %w", err)
	}
	return release, err
}

// SyncPushLockPath returns the single-flight lock path for the background
// mirror, a sibling of the Dolt directory at
// <dirname(databasePath)>/.links-sync-push.lock — the same position as the
// commit and workspace locks, so it survives a lit snapshots restore that
// rotates the Dolt directory. [LAW:one-source-of-truth] One naming convention;
// every mirror reads the path from here.
func SyncPushLockPath(databasePath string) string {
	cleaned := filepath.Clean(databasePath)
	return filepath.Join(filepath.Dir(cleaned), ".links-sync-push.lock")
}

// TryAcquireSyncPushLock takes a non-blocking exclusive hold guaranteeing only
// one background mirror runs at a time. The second return value reports whether
// the hold was taken: false means another mirror already holds it, and the
// caller coalesces by doing nothing — that mirror pushes the current HEAD
// (which already includes this caller's commit) and re-checks freshness before
// it releases. [LAW:no-ambient-temporal-coupling] Single-flight is owned here,
// not by sleeps or in-flight flags scattered across the spawn path; it is also
// what keeps two sibling mirrors from opening a second embedded Dolt engine on
// the one path and colliding on online GC.
func TryAcquireSyncPushLock(databasePath string) (func() error, bool, error) {
	return filelock.Acquire(context.Background(), SyncPushLockPath(databasePath), true, 1, 0)
}

func acquireWorkspaceLock(ctx context.Context, doltRootDir string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, error) {
	return acquireStoreLock(ctx, WorkspaceLockPath(doltRootDir), exclusive, maxAttempts, delay)
}

// acquireStoreLock runs the shared filelock acquisition and stamps its
// contention outcome with the store's domain sentinel.
//
// [LAW:parse-dont-validate] filelock reports contention as a value (a healthy
// lock being held is not a failure of the primitive); this is the one
// boundary where that value becomes ErrWorkspaceBusy, so every store lock's
// contention carries the same errors.Is discriminator.
func acquireStoreLock(ctx context.Context, lockPath string, exclusive bool, maxAttempts int, delay time.Duration) (func() error, error) {
	release, acquired, err := filelock.Acquire(ctx, lockPath, exclusive, maxAttempts, delay)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrWorkspaceBusy
	}
	return release, nil
}

// EngineLockPath returns the read-write-engine-exclusivity lock path for a
// Dolt root directory, at <dirname(databasePath)>/.links-engine.lock — the
// same sibling-of-dolt-dir position as the other locks, so it survives a lit
// snapshots restore that rotates the Dolt directory.
//
// [LAW:one-source-of-truth] One naming convention for the engine lock; every
// caller reads the path from here.
func EngineLockPath(databasePath string) string {
	cleaned := filepath.Clean(databasePath)
	return filepath.Join(filepath.Dir(cleaned), ".links-engine.lock")
}

const (
	// engineWriteRetryDelay/engineWriteRetryAttempts bound the wait for an
	// earlier read-write embedded Dolt engine on the same path to close
	// before this one opens. Embedded Dolt permits only one read-write
	// engine per path at a time — a second concurrent open does not queue,
	// it fails outright with Dolt's raw "cannot update manifest: database is
	// read only" (see links-sync-pgct.11: a foreground command racing a
	// still-live on-change mirror hit exactly this). Waiting here turns that
	// hard failure into the beat it actually takes the other holder to
	// finish. ~30s wall-clock cap matches mirrorParentWaitTimeout's budget
	// for "how long do we wait on a co-resident holder of this store" —
	// long enough to outlast a real push, short enough that a genuinely
	// wedged holder still surfaces as a clear, actionable error rather than
	// hanging forever.
	engineWriteRetryDelay    = 100 * time.Millisecond
	engineWriteRetryAttempts = 300
)

// acquireEngineWriteLock takes an exclusive, blocking-with-backoff hold
// guaranteeing this process is the only one with a live read-write embedded
// Dolt engine open on doltRootDir. Store.Open and Store.OpenSync both acquire
// it before opening their connection and release it in Close; Store.OpenForRead
// does not participate — a read-only open does not conflict with a concurrent
// read-write engine, only two read-write engines conflict with each other.
// [LAW:single-enforcer] This is the one boundary every read-write engine open
// passes through, so "only one read-write engine per path" cannot be violated
// by a call site racing ahead of it — no call site decides this for itself.
func acquireEngineWriteLock(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireStoreLock(ctx, EngineLockPath(doltRootDir), true, engineWriteRetryAttempts, engineWriteRetryDelay)
	if errors.Is(err, ErrWorkspaceBusy) {
		// [LAW:no-silent-failure] Wrap rather than replace so errors.Is(err,
		// ErrWorkspaceBusy) still detects contention while the operator sees
		// which holder to blame instead of a bare sentinel string.
		return nil, fmt.Errorf("another lit process is holding this workspace's read-write engine open (a background sync mirror or another lit command still running); retry: %w", err)
	}
	return release, err
}

// The path-parametrized acquisition loop these wrappers share lives at
// filelock.Acquire; only the lock meanings (paths, modes, budgets, operator
// guidance) remain here.
