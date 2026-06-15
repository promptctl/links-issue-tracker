package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// [LAW:single-enforcer] All commit-lock acquisition, transient-retry, and
// commitWorkingSet sequencing live here so writer serialization is enforced
// at exactly one boundary.
//
// Deadlock impossibility: This system has exactly one lock type (file-based
// commit lock). Single-resource systems cannot deadlock by lock-ordering. The
// processCommitMutex serializes in-process acquisition, and O_CREATE|O_EXCL
// serializes cross-process acquisition. Deadlock is only possible if the lock
// is never released, which defer prevents for panics and PID-liveness
// reclaims for killed processes.

// ErrTransientGCContention marks a failure caused by concurrent Dolt online
// garbage collection — either the manifest going read-only mid-run or the
// active connection being invalidated ("please reconnect"). Both are
// recoverable by backing off, rotating the poisoned connection, and retrying.
var ErrTransientGCContention = errors.New("transient online-gc contention")
var processCommitMutex sync.Mutex
var commitLockPIDRunning = isCommitLockPIDRunning

const (
	transientRetryMaxAttempts = 12
	transientRetryBaseDelay   = 50 * time.Millisecond
	transientRetryMaxDelay    = 1 * time.Second
	commitLockStaleAfter      = 10 * time.Minute
)

type retryOperation func(context.Context) error
type retryDelayFunc func(attempt int) time.Duration
type retrySleepFunc func(context.Context, time.Duration) error

// connectionRotator rotates a poisoned SQL connection between retry attempts.
// Online GC invalidates the connection that observed it, so the next attempt
// must run on a fresh handle. [LAW:effects-at-boundaries] The retry loop stays
// pure; the reconnect effect is injected here.
type connectionRotator func() error

type commitLockContextKey struct{}

// withMutation runs a mutation under a held commit lock. It begins a tx,
// invokes fn, commits the tx, and runs the working-set commit via
// commitWorkingSet (re-entrant: the lock is already held, so acquireCommitLock
// short-circuits). The lock is acquired and released exactly once.
//
// [LAW:dataflow-not-control-flow] Every mutation runs the same sequence;
// per-site variability is carried in `message` and `fn`, not in branches.
// [LAW:single-enforcer] Lock acquisition, tx lifecycle, and transient-retry
// are all owned at their respective single boundaries; withMutation composes
// them rather than duplicating any of them.
func (s *Store) withMutation(ctx context.Context, message string, fn func(ctx context.Context, tx *sql.Tx) error) error {
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin %s tx: %w", message, err)
		}
		defer tx.Rollback()
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s tx: %w", message, err)
		}
		return s.commitWorkingSet(ctx, message)
	})
}

// retryTransientGCContention runs operation, and on a transient online-GC
// contention failure backs off, rotates the (poisoned) connection, and retries.
// The rotate-between-attempts step is load-bearing: the GC reset invalidates the
// connection that observed it, so re-running on the same handle would fail
// identically — only a fresh connection can make progress. [LAW:single-enforcer]
// All GC-contention recovery lives here; callers supply the rotate effect.
func retryTransientGCContention(ctx context.Context, operation retryOperation, rotate connectionRotator, delayForAttempt retryDelayFunc, sleep retrySleepFunc) error {
	var lastErr error
	for attempt := 1; attempt <= transientRetryMaxAttempts; attempt++ {
		err := classifyTransientGCError(operation(ctx))
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrTransientGCContention) || attempt == transientRetryMaxAttempts {
			break
		}
		if waitErr := sleep(ctx, delayForAttempt(attempt)); waitErr != nil {
			return waitErr
		}
		if rotateErr := rotate(); rotateErr != nil {
			return rotateErr
		}
	}
	return lastErr
}

func transientRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := transientRetryBaseDelay << (attempt - 1)
	if delay > transientRetryMaxDelay {
		delay = transientRetryMaxDelay
	}
	return delay
}

func waitWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Store) commitWorkingSet(ctx context.Context, message string) error {
	// [LAW:single-enforcer] commitWorkingSet is the single mutation boundary that owns transient commit retry behavior.
	// [LAW:one-source-of-truth] A process-shared commit lock at this boundary is the canonical writer serialization mechanism.
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.commitWorkingSetOnce(ctx, message)
		}, s.reconnect, transientRetryDelay, waitWithContext)
	})
}

// commitWorkingSetOnce is the single function that hands a commit message to
// Dolt, so it owns what a valid commit message looks like: trimmed and never
// empty. Routing normalization through here means every caller (commitWorkingSet
// and withMutation) gets the same message shape with no per-callsite repetition.
// [LAW:single-enforcer] One trim+default rule for Dolt commit messages.
func (s *Store) commitWorkingSetOnce(ctx context.Context, message string) error {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		trimmed = "links mutation"
	}
	var commitHash string
	err := s.db.QueryRowContext(ctx, `CALL DOLT_COMMIT('-Am', ?)`, trimmed).Scan(&commitHash)
	if err == nil {
		return nil
	}
	normalized := strings.ToLower(err.Error())
	if strings.Contains(normalized, "nothing to commit") {
		return nil
	}
	return wrapCommitWorkingSetError(err)
}

func (s *Store) withCommitLock(ctx context.Context, operation retryOperation) error {
	lockedCtx, release, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return operation(lockedCtx)
}

func (s *Store) acquireCommitLock(ctx context.Context) (context.Context, func(), error) {
	if alreadyLocked, _ := ctx.Value(commitLockContextKey{}).(bool); alreadyLocked {
		return ctx, func() {}, nil
	}
	release, err := acquireCommitLockAtPath(ctx, s.commitLockPath)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, commitLockContextKey{}, true), release, nil
}

// LockCommitPath acquires the writer-exclusion commit lock at lockPath without
// requiring an open Store. Callers outside the Store (e.g. `lit snapshots
// new`/`restore`, which must operate without a Dolt SQL connection) use this
// to quiesce concurrent mutations for the duration of a filesystem operation.
// Returns a release function that the caller must defer.
//
// [LAW:single-enforcer] Routes through the same acquireCommitLockAtPath
// primitive Store uses, so writer serialization stays at one boundary.
func LockCommitPath(ctx context.Context, lockPath string) (func(), error) {
	return acquireCommitLockAtPath(ctx, lockPath)
}

// CommitLockPath returns the conventional commit-lock path for a workspace's
// Dolt root directory. The lock sits one level above the dolt directory (i.e.
// in the workspace storage dir) so that `lit snapshots restore` — which
// rotates the dolt directory itself — does not move the lock file out from
// under concurrent acquirers. Exposed so callers outside the Store don't
// reconstruct the path independently.
//
// [LAW:one-source-of-truth] The lock-file naming convention lives here; if it
// ever changes, Store and external callers move together.
func CommitLockPath(databasePath string) string {
	return commitLockPathForDolt(databasePath)
}

func commitLockPathForDolt(databasePath string) string {
	cleaned := filepath.Clean(databasePath)
	return filepath.Join(filepath.Dir(cleaned), ".links-commit.lock")
}

func acquireCommitLockAtPath(ctx context.Context, lockPath string) (func(), error) {
	processCommitMutex.Lock()
	locked, err := tryAcquireFileLock(lockPath)
	for errors.Is(err, os.ErrExist) && !locked {
		if staleErr := removeStaleCommitLock(lockPath, commitLockStaleAfter); staleErr != nil {
			processCommitMutex.Unlock()
			return nil, fmt.Errorf("acquire commit lock: %w", staleErr)
		}
		if waitErr := waitWithContext(ctx, transientRetryBaseDelay); waitErr != nil {
			processCommitMutex.Unlock()
			return nil, waitErr
		}
		locked, err = tryAcquireFileLock(lockPath)
	}
	if err != nil {
		processCommitMutex.Unlock()
		return nil, fmt.Errorf("acquire commit lock: %w", err)
	}
	if !locked {
		processCommitMutex.Unlock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("acquire commit lock: lock not acquired")
	}
	return func() {
		_ = os.Remove(lockPath)
		processCommitMutex.Unlock()
	}, nil
}

func tryAcquireFileLock(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, err
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return false, closeErr
	}
	return true, nil
}

func removeStaleCommitLock(path string, staleAfter time.Duration) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	isStaleByAge := time.Since(info.ModTime()) > staleAfter
	isStaleByOwner, err := commitLockOwnedByDeadProcess(path)
	if err != nil {
		return err
	}
	if !isStaleByAge && !isStaleByOwner {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func commitLockOwnedByDeadProcess(path string) (bool, error) {
	// [LAW:single-enforcer] Commit-lock owner liveness classification is centralized here to keep stale-lock handling deterministic.
	pid, hasOwnerPID, err := readCommitLockOwnerPID(path)
	if err != nil {
		return false, err
	}
	if !hasOwnerPID {
		return false, nil
	}
	running, err := commitLockPIDRunning(pid)
	if err != nil {
		return false, err
	}
	return !running, nil
}

func readCommitLockOwnerPID(path string) (int, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pidText := strings.TrimSpace(string(content))
	if pidText == "" {
		return 0, false, nil
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false, nil
	}
	return pid, true, nil
}

func isCommitLockPIDRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	// Unknown probe errors are treated as running to avoid removing an active lock.
	return true, nil
}

type transientGCContentionError struct {
	err error
}

func (e transientGCContentionError) Error() string {
	return e.err.Error()
}

func (e transientGCContentionError) Unwrap() error {
	return e.err
}

func (e transientGCContentionError) Is(target error) bool {
	return target == ErrTransientGCContention
}

func wrapCommitWorkingSetError(err error) error {
	wrapped := fmt.Errorf("dolt commit working set: %w", err)
	if !isTransientGCContentionError(err) {
		return wrapped
	}
	// [LAW:one-source-of-truth] Store commit wrapping is the canonical transient classifier for online-GC contention failures.
	return transientGCContentionError{err: wrapped}
}

func classifyTransientGCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTransientGCContention) {
		return err
	}
	if !isTransientGCContentionError(err) {
		return err
	}
	return transientGCContentionError{err: err}
}

// isTransientGCContentionError is the single predicate deciding whether a raw
// Dolt error is recoverable online-GC contention. The two shapes are distinct
// symptoms of the same cause, kept as single-purpose predicates and composed
// here. [LAW:decomposition] [LAW:single-enforcer]
func isTransientGCContentionError(err error) bool {
	return isManifestReadOnlyError(err) || isOnlineGCResetError(err)
}

func isManifestReadOnlyError(err error) bool {
	if err == nil {
		return false
	}
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "cannot update manifest") && strings.Contains(normalized, "read only")
}

// isOnlineGCResetError matches Dolt's online-GC connection invalidation
// (ErrServerPerformedGC). It requires the GC-specific phrase so the unrelated
// cluster-role transition error — which also says "please reconnect" — is not
// misclassified as transient. [FRAMING:representation]
func isOnlineGCResetError(err error) bool {
	if err == nil {
		return false
	}
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "online garbage collection") && strings.Contains(normalized, "reconnect")
}
