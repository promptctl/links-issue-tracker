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

	"github.com/promptctl/links-issue-tracker/internal/filelock"
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
	// transientRetryMaxAttempts/transientRetryBaseDelay/transientRetryMaxDelay
	// bound the total wait (~25s: five uncapped doublings then 25 more attempts
	// at the 1s cap) for a transient online-GC contention to clear. Sized to
	// match the engine-write lock's ~30s budget for "how long do we wait on a
	// co-resident holder of this store" (links-sync-pgct.11): the lock bounds
	// how long two engines can be concurrently OPEN, but this retry is what
	// absorbs the brief settle window right after one releases — under real
	// system load (a slower/contended CI runner, an earlier mirror's real
	// network push taking longer) that window is not always sub-second, and a
	// budget tuned only for a quick intra-process GC blip cut this retry off
	// before a legitimately-finishing prior holder released, escalating a
	// recoverable wait into a hard WorkspaceWriteBlockedError. A genuinely
	// wedged holder still surfaces as that same terminal error, just after the
	// longer budget elapses rather than hanging forever.
	transientRetryMaxAttempts = 30
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

// commitStamp is everything a mutation may declare about the Dolt commit it
// produces. The zero value beyond Message is the ordinary mutation: stamped
// now, authored by the session identity, skipped when the working set already
// equals HEAD. The reconcile's provenance replay sets the rest — a folded
// commit lands under its ORIGINAL message, timestamp, and author, and the
// settling marker commit lands even when it changes nothing.
// [LAW:types-are-the-program] the stamp is a value crossing the one commit
// boundary; zero means exactly today's behavior, so no caller changes meaning
// by ignoring it. [LAW:dataflow-not-control-flow] per-commit variability is
// data through one pipeline, never a second commit path.
type commitStamp struct {
	Message string
	// Date, when non-zero, becomes the commit timestamp via --date. Dolt's
	// date parsing is second-granular (RFC3339 without fractional seconds), so
	// sub-second precision truncates.
	Date time.Time
	// Author, when non-empty, is passed as --author "Name <email>", replacing
	// the session identity Dolt would otherwise stamp.
	Author string
	// AllowEmpty lands a commit even when the working set equals HEAD — for
	// marker commits whose existence, not diff, is the point.
	AllowEmpty bool
}

// withMutation runs a mutation under a held commit lock with the ordinary
// message-only stamp. It is the high-traffic spelling of withStampedMutation;
// the reconcile's provenance replay is the one caller that needs the full
// stamp.
func (s *Store) withMutation(ctx context.Context, message string, fn func(ctx context.Context, tx *sql.Tx) error) error {
	return s.withStampedMutation(ctx, commitStamp{Message: message}, fn)
}

// withStampedMutation runs a mutation under a held commit lock. It begins a
// tx, invokes fn, commits the tx, and runs the working-set commit — all as ONE
// retried unit (re-entrant: the lock is already held, so acquireCommitLock
// short-circuits). The lock is acquired and released exactly once.
//
// [LAW:dataflow-not-control-flow] Every mutation runs the same sequence;
// per-site variability is carried in `stamp` and `fn`, not in branches.
// [LAW:single-enforcer] Lock acquisition, tx lifecycle, and transient-retry
// are all owned at their respective single boundaries; withStampedMutation
// composes them rather than duplicating any of them.
//
// The whole BeginTx→fn→tx.Commit→commitWorkingSetOnce sequence is inside
// retryTransientGCContention, not just the final DOLT_COMMIT step
// (links-sync-pgct.11): tx.Commit() — the plain SQL transaction commit that
// lands fn's writes into Dolt's working set, distinct from the later DOLT_COMMIT
// that versions them — touches the same manifest and is just as exposed to
// transient online-GC contention.
//
// The unit is two phases with an owned resume point, not one blind re-run:
// staging (BeginTx→fn→tx.Commit) and versioning (DOLT_COMMIT). While staging
// fails, retrying it whole is safe — fn only writes through the tx it's given,
// and the deferred Rollback discards the failed attempt. But once tx.Commit
// has SUCCEEDED, fn's writes are durably staged in the working set (they
// survive a connection rotation), so a retry provoked by a transient failure
// in the versioning step must resume AT versioning: re-running fn would apply
// the mutation a second time on top of its own staged writes (CreateIssue's
// collision check, for one, would mint a duplicate issue under a higher
// nonce). The staged flag is the phase marker each attempt resumes from.
// [LAW:no-ambient-temporal-coupling] the phase is explicit owned state, not an
// assumption about which step happened to fail.
func (s *Store) withStampedMutation(ctx context.Context, stamp commitStamp, fn func(ctx context.Context, tx *sql.Tx) error) error {
	staged := false
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			if !staged {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin %s tx: %w", stamp.Message, err)
				}
				defer tx.Rollback()
				if err := fn(ctx, tx); err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return fmt.Errorf("commit %s tx: %w", stamp.Message, err)
				}
				staged = true
			}
			return s.commitWorkingSetOnce(ctx, stamp)
		}, s.reconnect, transientRetryDelay, waitWithContext)
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
	return exhaustedContentionError(lastErr)
}

// WorkspaceWriteBlockedError is the terminal, cross-process refusal a mutation
// hits when another process holds the embedded Dolt store open for writing: the
// manifest stayed read-only across the entire transient-retry budget, which no
// amount of in-process reconnecting can clear — only the foreign holder releasing
// does. It is the exhausted counterpart to the recoverable ErrTransientGCContention
// (intra-process, clears on retry). [FRAMING:representation] "database is read
// only" maps a lock-holder situation onto a permission one; this type carries the
// holder truth so the CLI renders "another lit process holds this workspace"
// instead of the raw backend line. The backend error is preserved as the cause
// for diagnosis, never dropped. [LAW:no-silent-failure]
type WorkspaceWriteBlockedError struct {
	Cause error
}

func (e WorkspaceWriteBlockedError) Error() string {
	// The holder sentence is the headline; the backend string is demoted to a
	// parenthetical for diagnosis, mirroring the sync-failure contract so the raw
	// "read only" line can never read as the whole (misleading) message.
	return fmt.Sprintf(
		"another lit process is holding this workspace open for writing; the store stayed read-only across every retry, so this write could not proceed (backend detail: %v)",
		e.Cause)
}

// Unwrap preserves the backend cause chain so errors.Is/As still see the
// underlying transient classification for diagnosis, while errors.As at the
// surface recognizes this terminal type first.
func (e WorkspaceWriteBlockedError) Unwrap() error { return e.Cause }

// exhaustedContentionError promotes a manifest-read-only that survived the full
// retry budget into the terminal WorkspaceWriteBlockedError. The discriminator is
// exhaustion: an intra-process online-GC hiccup clears within the budget (each
// attempt rotates the poisoned connection), so a manifest STILL read-only after
// every rotation is a foreign writer holding the store — not a transient this
// process can clear. A persistent GC-reset or any non-manifest error passes
// through unchanged, so only the genuinely cross-process case is reclassified.
// [LAW:types-are-the-program] the accept/reject decision lives in the type the
// surface dispatches on, not in message-string matching at the CLI.
func exhaustedContentionError(err error) error {
	if err != nil && isManifestReadOnlyError(err) {
		return WorkspaceWriteBlockedError{Cause: err}
	}
	return err
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

// waitWithContext delegates to filelock.SleepWithContext — one home for the
// context-aware inter-attempt sleep — under the historical local name the
// retry machinery passes around as a function value. [LAW:one-source-of-truth]
func waitWithContext(ctx context.Context, duration time.Duration) error {
	return filelock.SleepWithContext(ctx, duration)
}

func (s *Store) commitWorkingSet(ctx context.Context, message string) error {
	// [LAW:single-enforcer] commitWorkingSet is the single mutation boundary that owns transient commit retry behavior.
	// [LAW:one-source-of-truth] A process-shared commit lock at this boundary is the canonical writer serialization mechanism.
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientGCContention(ctx, func(ctx context.Context) error {
			return s.commitWorkingSetOnce(ctx, commitStamp{Message: message})
		}, s.reconnect, transientRetryDelay, waitWithContext)
	})
}

// commitWorkingSetOnce is the single function that hands a commit to Dolt, so
// it owns what a valid commit looks like: the message trimmed and never empty,
// and the stamp's optional date/author/allow-empty rendered as DOLT_COMMIT
// flags. A "nothing to commit" outcome is success-with-no-commit — the value
// diff is empty, so there is no change to version (and the reconcile's
// provenance replay leans on exactly this to drop folded commits whose
// projection changed nothing). [LAW:single-enforcer] One trim+default rule and
// one flag rendering for Dolt commits.
func (s *Store) commitWorkingSetOnce(ctx context.Context, stamp commitStamp) error {
	if s.commitWorkingSetHookForTest != nil {
		if err := s.commitWorkingSetHookForTest(); err != nil {
			return err
		}
	}
	trimmed := strings.TrimSpace(stamp.Message)
	if trimmed == "" {
		trimmed = "links mutation"
	}
	args := []any{"-Am", trimmed}
	if stamp.AllowEmpty {
		args = append(args, "--allow-empty")
	}
	if !stamp.Date.IsZero() {
		// RFC3339 in UTC is one of Dolt's supported date layouts; formatting in
		// UTC keeps the stored instant independent of this machine's zone.
		args = append(args, "--date", stamp.Date.UTC().Format(time.RFC3339))
	}
	if stamp.Author != "" {
		args = append(args, "--author", stamp.Author)
	}
	// [LAW:single-enforcer] buildProcedureCall owns the CALL-with-N-placeholders
	// spelling; this renders no second copy of it.
	var commitHash string
	err := s.db.QueryRowContext(ctx, buildProcedureCall("DOLT_COMMIT", len(args)), args...).Scan(&commitHash)
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
