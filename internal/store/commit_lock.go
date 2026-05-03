package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// [LAW:single-enforcer] All commit-lock acquisition, transient-retry, and
// commitWorkingSet sequencing live here so writer serialization is enforced
// at exactly one boundary.

var ErrTransientManifestReadOnly = errors.New("transient manifest read-only")
var processCommitMutex sync.Mutex
var commitLockPIDRunning = isCommitLockPIDRunning

const (
	transientManifestRetryMaxAttempts = 12
	transientManifestRetryBaseDelay   = 50 * time.Millisecond
	transientManifestRetryMaxDelay    = 1 * time.Second
	commitLockStaleAfter              = 10 * time.Minute
)

type retryOperation func(context.Context) error
type retryDelayFunc func(attempt int) time.Duration
type retrySleepFunc func(context.Context, time.Duration) error
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

func retryTransientManifestReadOnly(ctx context.Context, operation retryOperation, delayForAttempt retryDelayFunc, sleep retrySleepFunc) error {
	var lastErr error
	for attempt := 1; attempt <= transientManifestRetryMaxAttempts; attempt++ {
		err := classifyTransientManifestError(operation(ctx))
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrTransientManifestReadOnly) || attempt == transientManifestRetryMaxAttempts {
			break
		}
		if waitErr := sleep(ctx, delayForAttempt(attempt)); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

func transientManifestRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := transientManifestRetryBaseDelay << (attempt - 1)
	if delay > transientManifestRetryMaxDelay {
		delay = transientManifestRetryMaxDelay
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
		return retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
			return s.commitWorkingSetOnce(ctx, message)
		}, transientManifestRetryDelay, waitWithContext)
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

	processCommitMutex.Lock()
	locked, err := tryAcquireFileLock(s.commitLockPath)
	for errors.Is(err, os.ErrExist) && !locked {
		if staleErr := removeStaleCommitLock(s.commitLockPath, commitLockStaleAfter); staleErr != nil {
			processCommitMutex.Unlock()
			return ctx, nil, fmt.Errorf("acquire commit lock: %w", staleErr)
		}
		if waitErr := waitWithContext(ctx, transientManifestRetryBaseDelay); waitErr != nil {
			processCommitMutex.Unlock()
			return ctx, nil, waitErr
		}
		locked, err = tryAcquireFileLock(s.commitLockPath)
	}
	if err != nil {
		processCommitMutex.Unlock()
		return ctx, nil, fmt.Errorf("acquire commit lock: %w", err)
	}
	if !locked {
		processCommitMutex.Unlock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctx, nil, ctxErr
		}
		return ctx, nil, errors.New("acquire commit lock: lock not acquired")
	}

	release := func() {
		_ = os.Remove(s.commitLockPath)
		processCommitMutex.Unlock()
	}
	return context.WithValue(ctx, commitLockContextKey{}, true), release, nil
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

type transientManifestReadOnlyError struct {
	err error
}

func (e transientManifestReadOnlyError) Error() string {
	return e.err.Error()
}

func (e transientManifestReadOnlyError) Unwrap() error {
	return e.err
}

func (e transientManifestReadOnlyError) Is(target error) bool {
	return target == ErrTransientManifestReadOnly
}

func wrapCommitWorkingSetError(err error) error {
	wrapped := fmt.Errorf("dolt commit working set: %w", err)
	if !isManifestReadOnlyCommitError(err) {
		return wrapped
	}
	// [LAW:one-source-of-truth] Store commit wrapping is the canonical transient classifier for manifest read-only failures.
	return transientManifestReadOnlyError{err: wrapped}
}

func classifyTransientManifestError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTransientManifestReadOnly) {
		return err
	}
	if !isManifestReadOnlyCommitError(err) {
		return err
	}
	return transientManifestReadOnlyError{err: err}
}

func isManifestReadOnlyCommitError(err error) bool {
	if err == nil {
		return false
	}
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "cannot update manifest") && strings.Contains(normalized, "read only")
}
