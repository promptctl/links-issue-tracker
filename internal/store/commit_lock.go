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
// is never released, which defer prevents for panics, PID-liveness reclaims
// for killed processes, and age-based reclaim backstops where PIDs prove
// nothing — with a holder-side mtime heartbeat keeping live holds out of the
// age window.

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
)

// commitLockStaleAfter and commitLockHeartbeatEvery govern age-based lock
// reclaim from its two sides: a contender may reclaim a lock file untouched
// for commitLockStaleAfter, and every live holder re-touches its file each
// commitLockHeartbeatEvery so a working hold — however long — never ages into
// that window. The 10x gap is the safety margin: a live holder must miss ten
// consecutive beats before the age arm can misread it as gone. Package vars
// rather than consts only so tests can compress the timeline (the
// commitLockPIDRunning seam's pattern); production code never assigns them.
var (
	commitLockStaleAfter     = 10 * time.Minute
	commitLockHeartbeatEvery = time.Minute
)

// commitLockTouch is the heartbeat's one effect, swappable so tests can pin
// the release ordering contract deterministically — release joins any
// in-flight beat, and the lock file outlives every beat — instead of betting
// on straggler timing. Production never assigns it.
// [LAW:effects-at-boundaries]
var commitLockTouch = os.Chtimes

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
	hold, err := tryAcquireFileLock(lockPath)
	for errors.Is(err, os.ErrExist) && hold == nil {
		if staleErr := removeStaleCommitLock(lockPath, commitLockStaleAfter); staleErr != nil {
			processCommitMutex.Unlock()
			return nil, fmt.Errorf("acquire commit lock: %w", staleErr)
		}
		if waitErr := waitWithContext(ctx, transientRetryBaseDelay); waitErr != nil {
			processCommitMutex.Unlock()
			return nil, waitErr
		}
		hold, err = tryAcquireFileLock(lockPath)
	}
	if err != nil {
		processCommitMutex.Unlock()
		return nil, fmt.Errorf("acquire commit lock: %w", err)
	}
	if hold == nil {
		processCommitMutex.Unlock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("acquire commit lock: lock not acquired")
	}
	stopHeartbeat := startCommitLockHeartbeat(*hold)
	return func() {
		// [LAW:no-ambient-temporal-coupling] Stop-then-remove: stop blocks
		// until the refresher has fully exited, so no straggler touch can land
		// on the path after this Remove.
		stopHeartbeat()
		releaseCommitLockFile(*hold)
		processCommitMutex.Unlock()
	}, nil
}

// commitLockHold is proof that this process minted the lock file at path: the
// identity of the file it created, captured from the open descriptor before
// anyone could swap the path. Both writers that mutate the lock file — the
// heartbeat's touch and release's remove — take a hold and act only while the
// path still resolves to that same file, so a holder that was reclaimed from
// (a frozen process past the age window, or either of the pre-existing steal
// vectors removeStaleCommitLock documents) can neither freshen nor delete its
// successor's live lock. Identity is compared with os.SameFile — inode on
// unix, file index on Windows — so the fence needs no lock-file format change
// and stays byte-compatible with binaries that predate it.
//
// [LAW:types-are-the-program] the token is minted at creation and consumed by
// both writers, so "touch or remove a lock this process does not own" stops
// being a call any caller can express. The residual stat→act window is
// microseconds and cannot be closed inside a create-file protocol at all;
// closing it entirely is the flock migration tracked in
// links-snapshots-3dtv.2.
type commitLockHold struct {
	path     string
	ownerPID int
	identity os.FileInfo
}

// ownsFile reports whether the lock file at path is still the one this hold
// minted. A definitive answer — the file is gone, a different file sits
// there, or someone else's PID is written in it — comes back as false with no
// error; an indeterminate one carries the read failure so the caller reports
// it instead of guessing an owner. [LAW:no-silent-failure]
// [LAW:parse-dont-validate] the two outcomes are distinct values, never
// collapsed onto one "not ours" that hides I/O trouble.
//
// Identity and recorded PID are both required because each covers the other's
// blind spot. os.SameFile compares inode (unix) / file index (windows), and
// filesystems that allocate the lowest free inode — ext4 among them — hand a
// successor's create the very inode this hold's file just freed, so identity
// alone can read a thief's lock as ours. The recorded PID closes that: a live
// successor on this host cannot share our PID. Conversely a successor on
// ANOTHER host can coincidentally match our PID number, which is where
// identity carries the check. Both failing at once needs inode reuse and a
// PID collision across hosts in the same instant.
func (h commitLockHold) ownsFile() (bool, error) {
	current, err := os.Stat(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(h.identity, current) {
		return false, nil
	}
	// [LAW:single-enforcer] readCommitLockOwnerPID is the one parser of this
	// file's contents; the fence reads through it rather than growing a second.
	pid, hasOwnerPID, err := readCommitLockOwnerPID(h.path)
	if err != nil {
		return false, err
	}
	return hasOwnerPID && pid == h.ownerPID, nil
}

// releaseCommitLockFile removes the lock file only while it is still the file
// this hold minted. A lock reclaimed out from under a live holder belongs to
// whoever holds it now: removing it would hand a third process an instant
// O_EXCL acquire beside the current holder, turning one steal into two live
// writers. The mutation is already over by the time release runs, so every
// outcome here is a diagnostic rather than a failure — but a detected steal is
// exactly the evidence this epic exists to surface, so it is loud.
// [LAW:no-silent-failure] the previous `_ = os.Remove(...)` also discarded
// genuine removal errors; they now say so.
func releaseCommitLockFile(hold commitLockHold) {
	owns, err := hold.ownsFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: could not confirm ownership of commit lock %s at release; leaving it for age-based reclaim: %v\n", hold.path, err)
		return
	}
	if !owns {
		fmt.Fprintf(os.Stderr, "lit: commit lock %s was reclaimed by another process while this one still held it — another lit process may have been writing this workspace concurrently; leaving the current holder's lock in place\n", hold.path)
		return
	}
	if err := os.Remove(hold.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "lit: could not remove commit lock %s; a contender will reclaim it by age: %v\n", hold.path, err)
	}
}

// startCommitLockHeartbeat refreshes the held lock file's mtime every
// commitLockHeartbeatEvery until the returned stop is called; stop blocks
// until the refresher has exited. It keeps removeStaleCommitLock's age arm
// truthful: the holder re-stamps the mtime for as long as it is alive and
// scheduled, so an age past commitLockStaleAfter means no live holder is
// working (links-snapshots-3dtv.1: without this, any legitimate hold longer
// than the threshold — a non-reflink `lit snapshots new` on a big store — was
// stolen mid-operation by the next contender, and two writers interleaved).
// [FRAMING:representation] the heartbeat is the machine redrawing the
// liveness map on a cadence the hold owns; "operations finish inside the
// staleness window" was a timing bet, and this replaces it.
// [LAW:no-ambient-temporal-coupling]
//
// What it does NOT cover, stated plainly because the age arm is only one of
// two reclaim arms: the dead-PID arm never consults the mtime, so on a shared
// filesystem — where a contender probes a foreign host's PID against its own
// process table and reads ESRCH — a fresh, actively beating holder is still
// reclaimed instantly. Nor does beating survive a forward wall-clock step
// larger than the window, since ModTime ages against a clock the beat cannot
// hold still. Both are pre-existing holes in a PID-file protocol rather than
// regressions here, both are tracked in links-snapshots-3dtv.2, and until
// that lands the ownership fence below is what keeps them from compounding: a
// holder reclaimed by either vector stops beating and says so, instead of
// silently freshening the thief's lock.
//
// Two outcomes per beat, neither of which may fail the hold — the mutation
// the lock protects is the point, and aborting it over a liveness refresh
// would invert priorities (the dbsnapshot residue-collector precedent for
// diagnostics that never fail the host op). A touch that errors warns once
// per hold that age protection is degraded and keeps beating, because a
// transient filesystem error should not permanently disarm the heartbeat. A
// beat that finds the path no longer bound to its own file has been reclaimed
// from: it warns and exits, since every further beat would land on the
// current holder's lock and, by holding a dead successor's lock inside the
// freshness window, could lock the workspace out permanently.
// [LAW:no-silent-failure]
func startCommitLockHeartbeat(hold commitLockHold) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(commitLockHeartbeatEvery)
		defer ticker.Stop()
		warned := false
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				owns, err := hold.ownsFile()
				if err != nil {
					if !warned {
						warned = true
						fmt.Fprintf(os.Stderr, "lit: commit-lock heartbeat could not check %s; a hold longer than %s may be reclaimed as stale by a contender: %v\n", hold.path, commitLockStaleAfter, err)
					}
					continue
				}
				if !owns {
					fmt.Fprintf(os.Stderr, "lit: commit lock %s was reclaimed by another process while this one still held it — another lit process may be writing this workspace concurrently; this hold stops refreshing it\n", hold.path)
					return
				}
				now := time.Now()
				if err := commitLockTouch(hold.path, now, now); err != nil && !warned {
					warned = true
					fmt.Fprintf(os.Stderr, "lit: commit-lock heartbeat could not refresh %s; a hold longer than %s may be reclaimed as stale by a contender: %v\n", hold.path, commitLockStaleAfter, err)
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// tryAcquireFileLock creates the lock file exclusively and returns the hold
// that proves this process minted it, or nil with the reason it could not.
// The identity comes from the open descriptor rather than a second os.Stat of
// the path, so it names the file this call created even if another process
// replaces the path an instant later. [LAW:one-source-of-truth] acquisition
// and the ownership proof are minted together; there is no window where a
// caller holds one without the other.
func tryAcquireFileLock(path string) (*commitLockHold, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	ownerPID := os.Getpid()
	if _, err := fmt.Fprintf(file, "%d\n", ownerPID); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return nil, closeErr
	}
	return &commitLockHold{path: path, ownerPID: ownerPID, identity: identity}, nil
}

// removeStaleCommitLock reclaims a lock whose holder is gone by either
// signal: the recorded owner PID is dead, or the file has sat untouched for
// staleAfter.
//
// The age arm is truthful, and the holder-side heartbeat is what makes it so:
// every live holder re-touches its file at commitLockHeartbeatEvery, so an
// age past staleAfter means no live holder is working — dead, frozen
// mid-hold, or a pre-heartbeat lit, whose exposure is exactly the old
// behavior. Age stays sufficient on its own rather than requiring a dead PID
// first, which was considered and rejected: gating it would leave a foreign
// host's dead holder unreclaimable forever whenever its recorded PID aliases
// a live local process.
//
// The owner arm is NOT truthful on a shared filesystem, and the heartbeat
// does not help it: commitLockPIDRunning probes a recorded PID against THIS
// host's process table, so a lock held and actively beaten by another host
// reads as dead-owner and is reclaimed seconds after it was taken, mtime
// untouched by the decision. Age is the only arm that means anything
// cross-host. Fixing the owner arm needs the lock file to record which host
// minted it (or the flock protocol that makes liveness a kernel fact) — a
// protocol change tracked in links-snapshots-3dtv.2, deliberately not folded
// into the heartbeat. Until then commitLockHold's ownership fence bounds the
// damage: the reclaimed-from holder stops beating and says so, rather than
// silently freshening and later deleting the new holder's lock.
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
