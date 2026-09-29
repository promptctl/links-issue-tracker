package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/promptctl/primitives/filelock"
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
// drifts.
//
// [LAW:dataflow-not-control-flow] Variability between shared and exclusive
// modes lives in the (exclusive, maxAttempts, delay) arguments threaded into
// acquireWorkspaceLock; the acquisition sequence (filelock.Acquire's one
// open→try→retry loop) is the same every call.
//
// [LAW:locality-or-seam] The lock primitive (POSIX flock(2) vs. Win32
// LockFileEx) and its acquisition loop live in
// github.com/promptctl/primitives/filelock behind a typed seam shared with
// every other flock-backed coordination point (e.g. dbsnapshot's
// snapshot-producer beacon). This file keeps only the lock *meanings*:
// which path, which mode, which retry budget, and which operator guidance
// wraps contention. The lock discipline itself — the one primitive, the
// acquisition order, and where lock files live — is declared in this
// package's doc (doc.go); read it before adding a coordination point.

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

// WorkspaceLockPath returns the workspace-exclusivity lock path for a Dolt
// root directory. Sits at <dirname(databasePath)>/.links-workspace.lock — the
// same sibling-of-dolt-dir position as the commit lock — so lit snapshots
// restore (which renames the Dolt directory) does not move the lock file out
// from under concurrent acquirers.
//
// [LAW:one-source-of-truth] One naming convention for the workspace-busy lock;
// any callsite that needs the path reads it from this function.
func WorkspaceLockPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), ".links-workspace.lock")
}

// workspaceStorageDir is the lit-owned directory a workspace's locks and lock
// state sit in: the parent of the Dolt directory, which is what keeps them in
// place when `lit snapshots restore` rotates that directory out from under
// concurrent acquirers.
//
// [LAW:one-source-of-truth] Every *LockPath helper in this package reads this
// derivation from one place, so the position the ONE HOME rule names has
// exactly one definition.
func workspaceStorageDir(databasePath string) string {
	return filepath.Dir(filepath.Clean(databasePath))
}

// acquireWorkspaceShared takes a shared hold on the workspace lock for the
// lifetime of a Store. Released when the returned func is called. Waits out
// an exclusive holder for coResidentHolderWait so a casual concurrent
// lit snapshots restore — a rename — does not paper-cut every reader, and
// surfaces a "workspace busy" error naming the rotator that outlasts it.
func acquireWorkspaceShared(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, false, coResidentHolderWait)
	if errors.Is(err, ErrWorkspaceBusy) {
		// Wrap the sentinel so errors.Is(err, ErrWorkspaceBusy) detects
		// contention while the operator sees which holders to suspect.
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
// — this lock, in shared mode. The commit lock is a writer-vs-writer gate on a
// different file, which an adopt's exclusive hold never contends with, so a
// snapshot copy under only the commit lock can tear.
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
//
// Taking the hold forgets the received-refs record (received_refs.go): the
// record describes the directory about to be rotated away, and a rotation
// that cannot forget it does not start. The forgetting lives here, in the
// one act every rotator performs, so none of them can leave a record that
// leads the directory it now sits beside. [LAW:single-enforcer]
func LockWorkspaceExclusive(ctx context.Context, doltRootDir string) (func() error, error) {
	release, err := acquireWorkspaceLock(ctx, doltRootDir, true, 0)
	if errors.Is(err, ErrWorkspaceBusy) {
		return nil, fmt.Errorf("another lit process is using this workspace; close other lit commands and retry: %w", err)
	}
	if err != nil {
		return nil, err
	}
	if err := forgetReceivedRefs(doltRootDir); err != nil {
		return nil, errors.Join(err, release())
	}
	return release, nil
}

// SyncPushLockPath returns the single-flight lock path for the background
// mirror, a sibling of the Dolt directory at
// <dirname(databasePath)>/.links-sync-push.lock — the same position as the
// commit and workspace locks, so it survives a lit snapshots restore that
// rotates the Dolt directory. [LAW:one-source-of-truth] One naming convention;
// every mirror reads the path from here.
func SyncPushLockPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), ".links-sync-push.lock")
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

// receiveLockPath is the automatic receive's single-flight lock, a sibling of
// the Dolt directory beside the mirror's, for the same rotation-surviving
// reason. [LAW:one-source-of-truth]
func receiveLockPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), ".links-sync-receive.lock")
}

// TryAcquireReceiveLock takes a non-blocking exclusive hold guaranteeing only
// one automatic receive runs at a time. false means another receive holds it,
// and the caller does nothing: that receive fetches what this one would have.
// The hold is also what makes the receive's sweep of a dead receive's clone
// safe, as the sync-push lock is for the mirror's.
func TryAcquireReceiveLock(databasePath string) (func() error, bool, error) {
	return filelock.Acquire(context.Background(), receiveLockPath(databasePath), true, 1, 0)
}

// MirrorBeaconLockPath returns the mirror liveness beacon path, a sibling of
// the Dolt directory at <dirname(databasePath)>/.links-sync-mirror.lock — the
// same rotation-surviving position as the sync-push lock it accompanies.
// [LAW:one-source-of-truth] One naming convention; holders and probers both
// read the path from here.
func MirrorBeaconLockPath(databasePath string) string {
	return filepath.Join(workspaceStorageDir(databasePath), ".links-sync-mirror.lock")
}

// HoldMirrorBeacon marks the calling process as answering for the
// mirror-pending marker: a shared hold on the beacon, kept until the holder's
// work is done or it dies, so "is anyone still answering" is decided by the
// kernel — a SIGKILLed holder's hold evaporates with its process — never by
// an age threshold (the lock discipline in this package's doc). Two
// holder kinds share the one shared mode: a mirror holds from process entry
// for its whole run, and the claimant that spawned it holds from its claim
// until its own process exit — overlapping lifetimes, so the answering set
// is continuous from claim to push with no window in which a healthy spawn
// reads as dead. Shared because concurrent answerers are legal: racing
// claimants may each spawn a mirror, and every one is a live owner the probe
// must count.
//
// The only exclusive holds ever taken on the beacon are claimants'
// single-attempt probes, held for the microseconds between acquire and
// release, so contention here is momentary by construction; the wait exists
// so a mirror starting inside one probe's window waits it out instead of
// dying to that collision, and it is the package's one wait rather than a
// figure of its own. [LAW:one-source-of-truth]
func HoldMirrorBeacon(ctx context.Context, databasePath string) (func() error, error) {
	release, err := acquireStoreLock(ctx, workspaceStorageDir(databasePath), MirrorBeaconLockPath(databasePath), false, coResidentHolderWait)
	if errors.Is(err, ErrWorkspaceBusy) {
		// Only a probe's instantaneous exclusive hold can legitimately contend,
		// so outlasting the whole budget means something anomalous is squatting
		// on the beacon — and every mirror will keep refusing to run until it
		// leaves. That is channel degradation, not the healthy one-write-engine
		// serialization ErrWorkspaceBusy names, so the sentinel is deliberately
		// NOT propagated: wrapping it would record the ending as the non-paging
		// workspace_busy class and stop pushes with no FAILING banner and no
		// owner page. [LAW:no-silent-failure] Its TEXT still rides along, via
		// %v rather than %w — naming the squatter is the whole question this
		// message raises, and %v carries the holder account without carrying
		// the sentinel's identity.
		return nil, fmt.Errorf("mirror liveness beacon held exclusively past every probe window (a foreign process holding %s?): %v", MirrorBeaconLockPath(databasePath), err)
	}
	return release, err
}

// MirrorBeaconVerdict is ProbeMirrorBeacon's parse of the beacon's kernel
// state. [LAW:types-are-the-program] The flock has three observable states —
// free, shared-held, exclusive-held — and each carries a different obligation
// for the caller, so the verdict enumerates all three rather than collapsing
// the last two into one "alive" a squatter could hide behind.
type MirrorBeaconVerdict int

const (
	// BeaconUnheld: the deciding exclusive attempt acquired — kernel proof
	// that nobody (mirror, claimant, or squatter) held the beacon at that
	// instant. A marker seen beside this verdict is residue to re-claim.
	BeaconUnheld MirrorBeaconVerdict = iota
	// BeaconAnswered: someone held the beacon at the deciding instant, and
	// no exclusive holder stood at the shared step moments earlier —
	// normally a shared answerer (a claimant or the mirror it spawned, each
	// of whose code-running failure paths clears the marker and records a
	// loud outcome), though the probe cannot exclude an exclusive holder
	// that arrived between the steps (the function doc's residual: a
	// one-occasion deferral the next claim's obstructed verdict recovers
	// loudly). A marker seen beside this verdict is covered — for as long
	// as the holder lives; a holder dying immediately after leaves residue
	// the NEXT claim's unheld verdict recovers, the irreducible envelope
	// (no observable state proves a future push lands).
	BeaconAnswered
	// BeaconObstructed: an exclusive holder — a foreign squatter, or the
	// microsecond window of another claimant's own probe. Never covered:
	// callers spawn, and the spawned mirror either takes the beacon normally
	// (the holder was transient) or fails its shared hold loudly against the
	// persistent squatter.
	BeaconObstructed
)

// String names the verdict for the diagnostics that report one. A message
// carrying "beacon verdict: 1" sends its reader to this file to count iota;
// the whole value of a verdict over raw lock mechanics is that it can say what
// it saw. Exhaustive by construction: an unnamed value can only be one that
// was added to the enum without being added here, so it reports itself as
// exactly that rather than as any real state. [LAW:no-silent-failure]
func (v MirrorBeaconVerdict) String() string {
	switch v {
	case BeaconUnheld:
		return "unheld"
	case BeaconAnswered:
		return "answered"
	case BeaconObstructed:
		return "obstructed"
	}
	return fmt.Sprintf("unnamed MirrorBeaconVerdict(%d)", int(v))
}

// ProbeMirrorBeacon parses the beacon's kernel state into a
// MirrorBeaconVerdict via two single-attempt probes, shared first, exclusive
// LAST — the order is the correctness: the final, deciding attempt is the
// exclusive one, so its failure proves a holder exists at that instant and
// its success is the pure nobody-holds proof, which means a holder dying
// between the two steps produces BeaconUnheld and lands on the safe
// re-claim-and-spawn side rather than fabricating an answerer (the steps
// are sequential, never atomic — a verdict must not claim more than its
// last observation). The first, shared attempt only rules out an exclusive
// holder: its failure is BeaconObstructed. Each probe hold is released
// immediately — the probe takes custody of nothing. Residuals, stated
// rather than implied: an exclusive holder arriving between the steps reads
// answered for one occasion (the next claim's shared step then reads it
// obstructed, loudly), and two claims' probes interleaving can defer one
// re-claim to the next mutation — both bounded by the next occasion, the
// same envelope as every SIGKILL recovery here.
// [LAW:parse-dont-validate] The raw acquisition triples become the one
// domain verdict callers consume; no caller re-derives liveness from lock
// mechanics.
func ProbeMirrorBeacon(databasePath string) (MirrorBeaconVerdict, error) {
	// context.Background: single-attempt probes never sleep, so there is no
	// wait for a context to bound (same as the sync-push probe above).
	path := MirrorBeaconLockPath(databasePath)
	release, acquired, err := filelock.Acquire(context.Background(), path, false, 1, 0)
	if err != nil {
		return BeaconUnheld, fmt.Errorf("probe mirror liveness beacon (shared step): %w", err)
	}
	if !acquired {
		return BeaconObstructed, nil
	}
	if relErr := release(); relErr != nil {
		// A stuck SHARED probe hold excludes no answerer but falsifies later
		// exclusive probes from other processes; surface it as the probe
		// failing, never as a clean verdict. [LAW:no-silent-failure]
		return BeaconUnheld, fmt.Errorf("release mirror liveness beacon probe (shared step): %w", relErr)
	}
	release, acquired, err = filelock.Acquire(context.Background(), path, true, 1, 0)
	if err != nil {
		return BeaconUnheld, fmt.Errorf("probe mirror liveness beacon: %w", err)
	}
	if !acquired {
		return BeaconAnswered, nil
	}
	if relErr := release(); relErr != nil {
		// A stuck EXCLUSIVE probe hold blocks every answerer until this
		// process exits — the loud contract doubly applies.
		// [LAW:no-silent-failure]
		return BeaconUnheld, fmt.Errorf("release mirror liveness beacon probe: %w", relErr)
	}
	return BeaconUnheld, nil
}

func acquireWorkspaceLock(ctx context.Context, doltRootDir string, exclusive bool, wait time.Duration) (func() error, error) {
	return acquireStoreLock(ctx, workspaceStorageDir(doltRootDir), WorkspaceLockPath(doltRootDir), exclusive, wait)
}

// errLockHeld is the retry loop's own signal that an attempt found the lock
// held: a value for holdWait to retry on, never returned to a caller, who
// gets ErrWorkspaceBusy with the holder account instead.
var errLockHeld = errors.New("lock held")

// acquireStoreLock runs the filelock acquisition under the package's one
// wait policy (holdWait) and stamps its contention outcome with the store's
// domain sentinel. wait is how long the holders in front of the contender may
// stand still before it gives up; 0 is a single attempt that refuses on
// contention.
//
// [LAW:parse-dont-validate] filelock reports contention as a value (a healthy
// lock being held is not a failure of the primitive); this is the one
// boundary where that value becomes ErrWorkspaceBusy, so every store lock's
// contention carries the same errors.Is discriminator.
//
// It is also the one boundary where a lock says who holds it. Both halves of
// that live here rather than at each wrapper: while the wait runs,
// announceLockWait reports it instead of leaving the caller to guess whether
// lit is wedged or merely slow; when the wait elapses, the holder account
// rides the sentinel out to every wrapper's message for free. A ctx that ends
// while the lock is still held cuts the wait with the ctx's own error, which
// keeps its identity — an ended wait is not contention, so it never becomes
// ErrWorkspaceBusy — and carries the same account, so a caller whose deadline
// ran out in the wait learns who it was waiting on.
// [LAW:single-enforcer]
func acquireStoreLock(ctx context.Context, storageDir, lockPath string, exclusive bool, wait time.Duration) (func() error, error) {
	// Deferred, not called after the acquire: the reporter is a goroutine
	// whose only other exit is ctx.Done(), and callers pass
	// context.Background(), so a panic in the acquire would strand it waking
	// to do filesystem I/O for the life of the process.
	defer announceLockWait(ctx, storageDir, lockPath)()
	var release func() error
	// lastAttempt is the outcome of the most recent try: the retry loop
	// returns a bare ctx error when the ctx ends between tries, and this is
	// what still knows the lock was held when it did.
	var lastAttempt error
	policy := newHoldWait(storageDir, lockPath, func() time.Duration { return wait })
	err := backoff.Retry(func() error {
		acquiredRelease, acquired, err := filelock.Acquire(ctx, lockPath, exclusive, 1, 0)
		if err != nil {
			lastAttempt = err
			return backoff.Permanent(err)
		}
		if !acquired {
			lastAttempt = errLockHeld
			return errLockHeld
		}
		release = acquiredRelease
		return nil
	}, backoff.WithContext(policy, ctx))
	if errors.Is(err, errLockHeld) {
		return nil, fmt.Errorf("%s: %w", describeLockHolders(storageDir, lockPath), ErrWorkspaceBusy)
	}
	if err != nil && errors.Is(lastAttempt, errLockHeld) && errors.Is(err, ctx.Err()) {
		return nil, fmt.Errorf("%w waiting for %s, %s", err, lockPath, describeLockHolders(storageDir, lockPath))
	}
	if err != nil {
		return nil, err
	}
	return recordLockHolder(storageDir, lockPath, release), nil
}

// DoltJournalLockPath returns Dolt's own journal-manifest lock path for a
// Dolt root directory: <databasePath>/<database>/.dolt/noms/LOCK. This is not
// a lit-minted lock — the embedded driver takes it (a kernel flock through
// the same promptctl/primitives/filelock package lit's own locks use)
// whenever an engine opens, holds it for the engine's lifetime, and demotes
// the open to Dolt's read-only fallback when a 100ms attempt on it times out.
// Losing it is therefore the one condition under which an engine performs no
// lifecycle writes — no journal crash-recovery truncate, no close-time
// manifest flush.
//
// ONE HOME exception, stated here where the path is minted: the file lives
// INSIDE the dolt directory because it is Dolt's file, and that placement is
// correct for what it guards — a `lit snapshots restore` rotation carries the
// lock with the journal whose integrity it protects, and every acquirer lit
// controls holds the workspace lock, which is what serializes against the
// rotation itself. A non-lit dolt process opening the store directly holds
// this lock with no workspace hold — the same foreign holder
// coResidentHolderWait budgets for — and a rotation under that holder
// is outside lit's exclusion, as every lit-vs-non-lit interaction is.
//
// [LAW:one-source-of-truth] A lit-minted lock for the fact "one write-capable
// engine on this path", taken by write opens but not by OpenForRead, would be
// a partial second representation of it — the disagreement that lets a read
// command's engine run journal recovery underneath a snapshot walk. Code that
// needs the fact contends on Dolt's own lock; it does not mint a shadow.
func DoltJournalLockPath(databasePath string) string {
	return filepath.Join(filepath.Clean(databasePath), doltDatabaseName, ".dolt", "noms", "LOCK")
}

// LockDoltJournalExclusive takes an exclusive hold on Dolt's own journal lock
// for a caller that must exclude engine-lifecycle I/O without opening an
// engine — i.e. the `lit snapshots new` copy. While held, no concurrent
// engine open in any process performs journal crash-recovery or close-time
// flush, by one of two arms: a read open demotes to Dolt's read-only
// fallback after its 100ms attempt (read-only is purely this lock's
// contention fallback — lit never requests it), while a write-capable open
// refuses the fallback, waits coResidentHolderWait, and fails naming this
// holder. Fallback or refusal, nothing writes, so a
// file walk under this hold cannot capture a torn journal. Take it AFTER
// the workspace lock and BEFORE the commit lock, per the acquisition order
// in this package's doc (doc.go) — taking it inside the commit lock inverts the
// order against every live write Store.
//
// The one lifecycle write this hold does not stop: journal.idx is opened
// O_RDWR and truncated on every engine bootstrap with no can-write gate, so
// a copy can still capture a torn index. Severity downgrade, not a hole —
// Dolt's corruptIndexRecovery truncates a torn index to zero and rebuilds it
// from the journal on the restored store's first open, where a torn journal
// would be data loss.
func LockDoltJournalExclusive(ctx context.Context, databasePath string) (func() error, error) {
	lockPath := DoltJournalLockPath(databasePath)
	// This helper CONTENDS on Dolt's lock; it never mints Dolt's tree. The
	// shared acquisition path MkdirAll+O_CREATEs missing lock files — right
	// for lit-minted locks, wrong here: on an uninitialized workspace it
	// would fabricate <db>/links/.dolt/noms/, and the snapshot copy's
	// database-dir stat would then bless the fabrication as a snapshotable
	// store. Refuse instead. Stable against rotation/adopt because every
	// caller holds the workspace lock across this check and the acquisition.
	if err := requireInitializedWorkspace(databasePath); err != nil {
		return nil, err
	}
	// The root exists, so the from-scratch answer — nothing here at all — is
	// ruled out. What lies under it this code cannot tell apart: a damaged or
	// half-deleted Dolt tree, or a `lit init` interrupted after it made the root
	// and before Dolt wrote noms. Both are ENOENT beneath a live root, and no
	// marker separates them, so no type can. [LAW:types-are-the-program]
	//
	// Both get the fault error rather than a confident "run `lit init`", because
	// that sentence is the one that loops: init refuses a root it cannot read
	// and says retry. For the interrupted half that is an accepted downgrade —
	// re-running init would in fact fix it — and the trade is deliberate: a
	// diagnosis that terminates beats an instruction that spins.
	// [LAW:one-type-per-behavior]
	if _, err := os.Stat(filepath.Dir(lockPath)); err != nil {
		return nil, fmt.Errorf("stat dolt journal dir: %w", err)
	}
	// The same wait a write engine open gives the same holder: the engine
	// connector waits with holdWait on this lock's own holder directory, so
	// an engine and a copy contending for LOCK read one policy.
	// [LAW:one-source-of-truth]
	release, err := acquireStoreLock(ctx, workspaceStorageDir(databasePath), lockPath, true, coResidentHolderWait)
	if errors.Is(err, ErrWorkspaceBusy) {
		// [LAW:no-silent-failure] Wrap rather than replace so errors.Is(err,
		// ErrWorkspaceBusy) still detects contention while the operator sees
		// which holder to blame instead of a bare sentinel string.
		return nil, fmt.Errorf("another process is holding this workspace's Dolt store open; retry after it completes: %w", err)
	}
	return release, err
}

// The single attempt these wrappers share lives at filelock.Acquire and the
// wait between attempts at holdWait; only the lock meanings (paths, modes,
// operator guidance) remain here.
