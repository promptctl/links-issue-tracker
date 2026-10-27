package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/promptctl/primitives/filelock"
)

// backgroundMirrorSubcommand is the hidden top-level command the on-change
// cadence owner spawns. It never appears in help; it exists only as the
// detached worker's entrypoint.
const backgroundMirrorSubcommand = "__mirror-bg"

// mirrorLogName is the detached mirror's durable output sink. A detached
// process owns no terminal, so its stdout/stderr must land somewhere inspectable
// rather than /dev/null — otherwise a trace-write failure or a panic vanishes.
const mirrorLogName = "mirror.log"

// workerLogMaxBytes caps a detached worker's log: every cycle logs a
// start/end line. Rotation keeps one previous generation so the recent window
// survives each cut; the log is diagnostics, not state, so older lines are
// free to go.
const workerLogMaxBytes = 256 * 1024

// A rotation hold spans one stat and at most one rename, so a holder still in
// front after half a second is stalled, and the caller's run matters more
// than this rotation.
const (
	logRotationLockAttempts   = 50
	logRotationLockRetryDelay = 10 * time.Millisecond
)

// rotateLog moves an append-only log past maxBytes aside (one kept generation)
// before the next append, for the worker logs and the invocation events file
// alike, and reports whether it did. The size check and the rename are one
// step under the log's lock: two appenders that both saw the file over its cap
// would otherwise rotate twice, and the second rename would move the first's
// fresh file over the generation it had just kept.
// [LAW:no-ambient-temporal-coupling] the kernel flock owns the ordering.
// Only a log that looks over its cap takes the lock, so the append that
// needs no rotation — nearly all of them — never depends on a filesystem
// whose flock fails or stalls.
// A rotation problem is reported, never fatal: the worst outcome of skipping
// it is a log that keeps growing, which must not cost the caller its run.
// [LAW:no-silent-failure]
func rotateLog(path string, maxBytes int64) (bool, error) {
	if !logOverCap(path, maxBytes) {
		return false, nil
	}
	lockPath := path + ".lock"
	release, acquired, err := filelock.Acquire(context.Background(), lockPath, true, logRotationLockAttempts, logRotationLockRetryDelay)
	if err != nil {
		return false, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	if !acquired {
		return false, fmt.Errorf("another lit process held %s for over %s", lockPath, time.Duration(logRotationLockAttempts)*logRotationLockRetryDelay)
	}
	rotated, rotateErr := rotateLogLocked(path, maxBytes)
	if relErr := release(); relErr != nil {
		return rotated, errors.Join(rotateErr, fmt.Errorf("release %s: %w", lockPath, relErr))
	}
	return rotated, rotateErr
}

// rotateLogLocked rotates under the log's lock, checking the size again: an
// appender that held the lock first may already have rotated.
func rotateLogLocked(path string, maxBytes int64) (bool, error) {
	if !logOverCap(path, maxBytes) {
		return false, nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return false, err
	}
	return true, nil
}

func logOverCap(path string, maxBytes int64) bool {
	st, err := os.Stat(path)
	// Absent is the common first-append case and needs no rotation; any other
	// stat failure will resurface loudly from OpenFile just after.
	return err == nil && st.Size() > maxBytes
}

const (
	// parentPostSpawnTail is how long a HEALTHY parent can legitimately live
	// after spawning a detached worker: every bounded step
	// maybeAutoSyncAfterCommand has scheduled for after the spawn, summed from
	// those steps' own caps. The receive is not one of them: it is a detached
	// worker itself, and spawning it returns at once. A receive worker that
	// owes its settle a mirror (mirrorAfterReceive) spawns it as its last step
	// and exits, so its tail is inside a command's.
	//
	// It is a sum rather than a number because prose does not fail to compile
	// when a step joins the tail. A step left unsummed here lets a pass
	// slower than the leftover margin make a perfectly healthy parent outlive
	// the wait below — abandoning a mirror that owes a push, for work the
	// parent was designed to do. Adding a step to the tail means adding it
	// here.
	// [LAW:one-source-of-truth]
	parentPostSpawnTail = compactTimeout // the compaction backstop

	// workerParentWaitMargin is the headroom above the parent's designed tail:
	// scheduling slop on a loaded machine, not another step. A bound inside the
	// tail would manufacture parent-wait failures out of the parent's own work,
	// so the margin exists to keep the two clearly separated.
	workerParentWaitMargin = 30 * time.Second

	// workerParentWaitTimeout bounds the wait for the spawning command to
	// release its engine. The wait ends the instant the parent exits; the cap
	// only guards a parent that never exits (e.g. a long-lived REPL), in which
	// case the worker gives up rather than hang forever.
	workerParentWaitTimeout = parentPostSpawnTail + workerParentWaitMargin
	workerParentPollDelay   = 20 * time.Millisecond
)

// spawnBackgroundMirror starts the detached mirror and returns immediately,
// without waiting for it. [LAW:effects-at-boundaries] The mutating command's
// change is already durable in the local Dolt store; getting it to the remote
// is an effect pushed entirely off the command's own latency path into a
// separate process. The automation-trace env is set here so a push that runs
// and fails records a trace through the one shared writer the pre-push hook
// already uses. [LAW:one-source-of-truth]
func spawnBackgroundMirror(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, parentPID int) error {
	return spawnDetachedWorker(ctx, stderr, ws, backgroundMirrorSubcommand, mirrorLogName, mirrorEnv(env), parentPID)
}

// detachedWorkerLeaf is the worker side of spawnDetachedWorker, the one entry
// every detached worker runs through: its --store and --parent-pid flags, the
// inherited hold made this process's alone, and only then the build of its
// workspace from the store and the handoff on stdin. The worker waits for its
// own children, so close-on-exec drops only descendants that outlive it — a
// backgrounded owner-notify hook, a git credential daemon — which would keep
// the lock held, and every join waiting, long after this worker is gone.
// [LAW:single-enforcer]
//
// It acquires nothing before its work, because nothing that can fail may run
// before the worker's own ordering: the store's Location is pure geometry, and
// a build that fails is handed to the worker, which ends it through its
// completion seam. [LAW:no-ambient-temporal-coupling]
func detachedWorkerLeaf(subcommand, parentPIDUsage string, work func(ctx context.Context, inv Invocation, built workerWorkspace, parentPID int) error) bareLeaf {
	fs := newCobraFlagSet(subcommand)
	storageDir := fs.String("store", "", "storage directory of the store the spawning command ran against")
	parentPID := fs.Int("parent-pid", 0, parentPIDUsage)
	return bareLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, _ struct{}, _ []string) error {
		if !filepath.IsAbs(*storageDir) {
			return UsageError{Message: fmt.Sprintf("lit %s: --store must name the store's absolute storage directory, got %q", subcommand, *storageDir)}
		}
		loc := workspace.LocationFromStorageDir(*storageDir)
		keepWorkerHoldFromChildren()
		err := work(ctx, inv, buildWorkerWorkspace(ctx, inv.Stderr, inv.Env, loc, inv.Stdin), *parentPID)
		// The workspace's store, once, after the worker's own closes: the clones
		// a worker pushes and fetches through are its means, not the store it
		// ran against, and one fact read at each of their closes would keep only
		// whichever closed last.
		recordStoreBytes(ctx, loc.DatabasePath)
		return err
	}}
}

// spawnDetachedWorker starts one of the hidden worker commands as a detached
// process of its own, told which store it runs against and which process
// spawned it, so it can wait for that process's engine to be released, and
// returns without waiting. The worker runs in the store's directory, which
// lives exactly as long as the store, and reads the handoff (workerHandoff)
// from its stdin. [LAW:one-type-per-behavior] the mirror and the receive are
// two instances of one detached worker; what differs is the subcommand, the
// log and the environment, all values.
func spawnDetachedWorker(ctx context.Context, stderr io.Writer, ws workspace.Info, subcommand, logName string, env environ.Env, parentPID int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lit binary: %w", err)
	}
	handoff, err := handoffOf(ctx, ws)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(handoff)
	if err != nil {
		return fmt.Errorf("encode the worker's handoff: %w", err)
	}
	handoffRead, handoffWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open the worker's handoff pipe: %w", err)
	}
	// Each end is also closed here, so no path leaks one; the read end is
	// closed early, right after Start, and closing it again is a no-op.
	defer func() { _ = handoffRead.Close(); _ = handoffWrite.Close() }()
	// The worker is joinable from before it exists: the shared hold is taken
	// here and inherited, so there is no instant at which the worker is alive
	// and unheld. A worker nobody can join is the race the hold exists to
	// close, so a hold that cannot be taken fails the spawn.
	// [LAW:no-ambient-temporal-coupling]
	holds, err := holdWorkerLock(ws.StorageDir)
	if err != nil {
		return err
	}
	cmd := env.Command(ws.StorageDir, self, subcommand, "--store", ws.StorageDir, "--parent-pid", strconv.Itoa(parentPID))
	cmd.Stdin = handoffRead
	cmd.ExtraFiles = holds
	// Route the detached worker's output to a durable log. [LAW:no-silent-failure]
	// If the log cannot be opened, surface that on the command's terminal-attached
	// stderr and still spawn with discarded streams — the worker's run matters
	// more than its log, and the inability to log is itself loud here rather
	// than swallowed.
	logPath := filepath.Join(ws.StorageDir, logName)
	if _, rotateErr := rotateLog(logPath, workerLogMaxBytes); rotateErr != nil {
		fmt.Fprintf(stderr, "lit: %s rotation failed (%v); the log keeps growing past its cap\n", logName, rotateErr)
	}
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logErr != nil {
		fmt.Fprintf(stderr, "lit: %s unavailable (%v); background worker output will be discarded\n", logName, logErr)
	} else {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	cmd.SysProcAttr = detachSysProcAttr()
	startErr := cmd.Start()
	// The child holds its own copy of the read end from exec on. Closing ours
	// is what makes a write to a worker that already died fail instead of
	// waiting on a pipe nobody reads.
	_ = handoffRead.Close()
	if logFile != nil {
		// The child inherited its own dup of the fd at exec; the parent's copy is
		// no longer needed. Closing it cannot affect the child's logging.
		_ = logFile.Close()
	}
	// The child shares each hold's open file description, so closing the
	// parent's copy leaves the lock held by the worker; after a failed start
	// it is the release.
	for _, hold := range holds {
		_ = hold.Close()
	}
	if startErr != nil {
		return startErr
	}
	// Written after Start, then closed by the defer, so the worker reads the
	// whole handoff and then EOF. A write the pipe's buffer cannot take waits
	// for the worker, which reads its handoff first.
	//
	// A failed write is a failed spawn: the worker holds the only read end, so
	// the write fails only once the worker has exited, before it could finish
	// reading and so before it recorded anything. Its hold went with it.
	if _, err := handoffWrite.Write(payload); err != nil {
		return fmt.Errorf("write the worker's handoff: %w", err)
	}
	return nil
}

// mirrorEnv builds the detached mirror's environment: the parent's environment
// with every automation-trace variable stripped, then the mirror's own trigger
// and reason set. The mirror has no reader for a trace-ref file, so it carries
// none — it records traces by trigger alone.
func mirrorEnv(parent environ.Env) environ.Env {
	return workerEnv(parent,
		[]string{automationTriggerEnvVar, automationReasonEnvVar, automationTraceRefFileEnvVar},
		automationTriggerEnvVar+"=on-change",
		automationReasonEnvVar+"=on-change cadence mirrored after a mutating command",
	)
}

// workerEnv is the parent's environment with the named variables removed and
// set appended. Every detached worker strips LNKS_AUTOMATION_TRACE_REF_FILE:
// it points at a file the parent's caller reads to learn which trace the
// command recorded, and a worker that inherited it would overwrite that file
// with its own trace path after the command has returned.
// [LAW:one-source-of-truth]
func workerEnv(parent environ.Env, strip []string, set ...string) environ.Env {
	return parent.Without(strip...).With(set...)
}

// backgroundMirrorLeaf is the detached worker. It runs as its own process after
// the spawning command has returned, so it establishes the engine-release
// invariant first (wait-for-parent), then runs single-flight push cycles until
// no mirror-pending claim remains. [LAW:no-ambient-temporal-coupling]
//
// The cycle loop is what makes losing the single-flight race safe to treat as a
// silent exit: the loser's spawner claimed the mirror-pending marker, and the
// current lock holder is obligated to re-check that marker AFTER releasing — a
// claim it cannot have covered (stamped while its engine was open, or after)
// triggers another full cycle on a fresh engine, whose open then postdates the
// claimant's commit. Custody of the marker passes from holder to holder at the
// lock, never resting on timing. Losing therefore never strands a claim, and
// the loser still exits without opening a store, writing a trace, or creating a
// file — the quiescence property test cleanups rely on.
func backgroundMirrorLeaf() bareLeaf {
	return detachedWorkerLeaf(backgroundMirrorSubcommand, "PID of the spawning command; the mirror waits for it to exit", func(ctx context.Context, inv Invocation, built workerWorkspace, parentPID int) error {
		ws := built.ws
		// A live mirror IS the coverage the claim protocol counts on, and its
		// liveness is proven by the kernel: hold the beacon shared from entry —
		// before the parent-exit wait, so mutations claiming during that wait read
		// this mirror as alive — until the process ends, when the kernel releases
		// it on any death mode. A mirror that cannot take the hold must not run:
		// its work would be invisible to every claimant's probe, so each would
		// spawn a redundant sibling anyway. [LAW:no-ambient-temporal-coupling]
		// stopAnswering is the idempotent release the dying paths run BEFORE their
		// completion effects (see the helpers); a no-op until the hold exists.
		stopAnswering := func() {}
		releaseBeacon, beaconErr := store.HoldMirrorBeacon(ctx, ws.DatabasePath)
		if beaconErr != nil {
			if ctx.Err() != nil {
				return teardownMirror(inv.Stderr, inv.Env, ws, ctx.Err(), stopAnswering)
			}
			return completeMirrorWithoutAttempt(ctx, inv.Stderr, inv.Env, ws, fmt.Errorf("hold mirror liveness beacon: %w", beaconErr), stopAnswering)
		}
		var stopOnce sync.Once
		stopAnswering = func() {
			stopOnce.Do(func() {
				// A failed release matters on the stop-before-effects path: the
				// dying mirror keeps reading as a live answerer through the
				// completion effects (the owner-notify hook's cap included) until
				// process exit finally drops the hold. Loud, not fatal — the
				// kernel's on-exit release remains the backstop.
				// [LAW:no-silent-failure]
				if relErr := releaseBeacon(); relErr != nil {
					fmt.Fprintf(inv.Stderr, "lit: mirror beacon not released (%v); concurrent claims may read this dying mirror as live until process exit\n", relErr)
				}
			})
		}
		defer stopAnswering()

		// Wait for the spawning command's embedded engine to be released. Opening
		// a second engine on the same path while the first is live collides on
		// Dolt's online garbage collection. If the parent outlives the timeout, the
		// precondition is unmet — abort rather than race a live engine. A wait cut
		// short by teardown is not that failure: it ends as a teardown, below.
		// [LAW:no-ambient-temporal-coupling]
		if !waitForParentExit(ctx, parentPID, os.Getppid, workerParentWaitTimeout, workerParentPollDelay) {
			if ctx.Err() != nil {
				return teardownMirror(inv.Stderr, inv.Env, ws, ctx.Err(), stopAnswering)
			}
			return completeMirrorWithoutAttempt(ctx, inv.Stderr, inv.Env, ws, fmt.Errorf(
				"spawning command (pid %d) still running after %s; skipping mirror to avoid racing its engine",
				parentPID, workerParentWaitTimeout), stopAnswering)
		}
		// A workspace that could not be built ends like the mirror's other
		// pre-attempt deaths, once the parent has exited, so no push outcome is
		// written while the spawning command may still be writing its own.
		// [LAW:no-ambient-temporal-coupling]
		if built.failed != nil {
			return completeMirrorWithoutAttempt(ctx, inv.Stderr, inv.Env, ws, built.failed, stopAnswering)
		}

		for {
			// Teardown owns the loop's lifetime: once the context is done (the
			// SIGTERM grace window), starting another engine cycle would fight the
			// shutdown for its last seconds. [LAW:no-ambient-temporal-coupling]
			if ctx.Err() != nil {
				return teardownMirror(inv.Stderr, inv.Env, ws, ctx.Err(), stopAnswering)
			}
			release, acquired, err := store.TryAcquireSyncPushLock(ws.DatabasePath)
			if err != nil {
				return completeMirrorWithoutAttempt(ctx, inv.Stderr, inv.Env, ws, fmt.Errorf("acquire sync-push lock: %w", err), stopAnswering)
			}
			if !acquired {
				// Lost the single-flight race: the holder's post-release re-check
				// below now owns any claim this mirror was spawned for. Exit with
				// no store open, no trace, no file — see the function comment.
				return nil
			}
			// The cycle-start instant is the re-check's ordering witness: any
			// marker older than it existed before this cycle's entry-clear ran,
			// so its survival means the clear is failing, not that a claim landed.
			cycleStart := time.Now()
			// A cycle is a unit of work of its own, so it reads git's remote
			// configuration afresh: a remote added while this worker kept
			// cycling is pushed to by the next cycle, not the worker's next life.
			// Replaced on the worker's one Info, so the cycle and everything after
			// it read the same remotes. [LAW:one-source-of-truth]
			ws.Remotes = ws.Remotes.Reread()
			attempted := mirrorCycle(ctx, inv.Stderr, inv.Env, inv.Stdout, ws, stopAnswering)
			// Released only after the cycle's engine has closed (mirrorCycle's
			// deferred Close), so the lock brackets the whole session. The kernel
			// drops the flock on process exit, so an unlock error cannot strand
			// the lock; surfacing it would only add noise to a detached worker.
			_ = release()
			if !attempted {
				// The failure was already completed through the push-outcome seam;
				// looping again would hot-spin on the same broken precondition.
				// Any surviving claim ages into crash recovery.
				return nil
			}
			again, recheckErr := recheckMirrorPending(ws, cycleStart)
			if recheckErr != nil {
				// A re-check that cannot give a truthful verdict (unreadable
				// marker, or a marker this cycle's own clear failed to remove) is
				// terminal, loudly: cycling on it would push forever against a
				// marker that never goes away. [LAW:no-silent-failure]
				recordMirrorTraceError(inv.Stderr, inv.Env, ws, recheckErr)
				return nil
			}
			if !again {
				return nil
			}
			// A claim landed after this cycle began, so its claimant's commit may
			// postdate this cycle's HEAD read (its commit preceded this cycle's
			// open only if its command's session did — a claim alone cannot prove
			// that). Run another cycle on a fresh engine: its open postdates the
			// claimant's closed session, which is the proof. An extra cycle for a
			// claim that WAS already covered is an up-to-date push — cheap, and
			// always on the correct side. [LAW:dataflow-not-control-flow] every
			// cycle runs the same path; only the marker decides whether another
			// begins.
		}
	})
}

// teardownMirror is the ending for a mirror dismantled by its own context (the
// SIGTERM grace window) rather than by a failure: it releases the claim so the
// NEXT mutation re-claims and re-spawns immediately without even needing its
// beacon probe, traces the ending for the audit log, and deliberately
// writes NO push-outcome record — the teardown attempted nothing, so the last
// COMPLETED attempt's record (possibly a healthy "pushed" from this very
// process's previous cycle) remains the truthful answer to "where do things
// stand". [FRAMING:representation] Recording the teardown as an outcome would
// overwrite that answer with a non-event.
//
// stopAnswering runs FIRST: a mirror being dismantled will never push, so it
// must stop reading as an answerer before any teardown effect — the same
// ordering contract completeMirrorWithoutAttempt enforces.
func teardownMirror(stderr io.Writer, env environ.Env, ws workspace.Info, cause error, stopAnswering func()) error {
	stopAnswering()
	clearMirrorPending(stderr, ws)
	recordMirrorTraceError(stderr, env, ws, cause)
	return nil
}

// mirrorCloneDirName is the directory under the workspace storage dir that
// holds the mirror's clone of the Dolt directory while a cycle pushes from it:
// <StorageDir>/mirror-clone/<cycle-stamp>/dolt, with the clone's own lock files
// beside it in <cycle-stamp>/. One cycle stamp per cycle, never reused, so a
// handle a dolt-internal load left open on a previous cycle's tree can never
// be mistaken for this cycle's. [LAW:one-source-of-truth] One naming
// convention; the sweep and the take both read it from here.
const mirrorCloneDirName = "mirror-clone"

// mirrorCloneBase is the parent of every cycle's clone.
func mirrorCloneBase(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, mirrorCloneDirName)
}

// mirrorClone is one cycle's clone of the live store: the tree the push runs
// from. It is minted only by takeMirrorClone, which is what makes holding one
// the proof that the clone was taken under the live store's locks and the
// mirror-pending marker cleared inside that hold — the custody precondition
// performSyncPush states. [LAW:parse-dont-validate]
type mirrorClone struct {
	liveClone
}

// liveClone is a clone of the live store taken under the live store's locks by
// cloneLiveStore: the tree a network operation runs from so that the live
// store is not held across the network. The mirror pushes from one and the
// automatic receive fetches into one.
type liveClone struct {
	// dir is the clone's directory: the clone's dolt root and the lock files
	// its own engine mints live under it, and removing it removes them all.
	dir string
	// databasePath is the cloned dolt root, an engine-openable store.
	databasePath string
	// held is how long the live store was held for the take: from the last
	// lock acquired to the first released, measured inside the hold.
	held time.Duration
	// taken is the clone as it was taken, read before any session opened on
	// it: what a landing of its fetch requires the live store to still hold
	// (store.LandFetchedHead).
	taken store.TakenClone
}

// cloneLiveStore sweeps base, then clones the live store into a fresh
// directory under it, holding exactly what a file-by-file copy of the Dolt
// directory needs (withDoltDirectoryHeld: workspace shared, Dolt's journal
// lock, commit lock) and running underHold inside that same hold after the
// copy. The caller must hold the single-flight lock that owns base: that lock
// is what proves any tree already under base belongs to a holder that died,
// and so what makes the sweep safe.
//
// The copy runs under store.MirrorHoldBudget, and only the copy: the lock
// waits before it are bounded by their own retry budgets and are waiting, not
// holding. cut reports that the budget, not the work, ended the hold; the
// wording of that is the caller's. [LAW:single-enforcer] one take, whoever
// clones.
//
// Once the hold is released the clone is read as taken (store.ReadTakenClone),
// before anything runs on it; a clone that will not open for that read fails
// the take, since the session the caller opens on it next would not open
// either.
func cloneLiveStore(ctx context.Context, ws workspace.Info, base string, underHold func()) (clone liveClone, cut bool, err error) {
	if err := os.RemoveAll(base); err != nil {
		return liveClone{}, false, fmt.Errorf("collect a dead holder's clone under %s: %w", base, err)
	}
	dir := filepath.Join(base, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return liveClone{}, false, fmt.Errorf("create clone dir: %w", err)
	}
	clone = liveClone{dir: dir, databasePath: filepath.Join(dir, "dolt")}
	var holdStart time.Time
	err = withDoltDirectoryHeld(ctx, ws, func() error {
		holdStart = time.Now()
		holdCtx, cancel := context.WithTimeout(ctx, store.MirrorHoldBudget)
		defer cancel()
		if err := dbsnapshot.CloneTree(holdCtx, ws.DatabasePath, clone.databasePath); err != nil {
			cut = holdCtx.Err() != nil && ctx.Err() == nil
			return fmt.Errorf("clone the store: %w", err)
		}
		underHold()
		return nil
	})
	if !holdStart.IsZero() {
		clone.held = time.Since(holdStart)
	}
	if err == nil {
		clone.taken, err = store.ReadTakenClone(ctx, clone.databasePath)
	}
	if err != nil {
		// A failed take leaves nothing for the caller to remove. A removal
		// that fails here travels with the failure — the next take's sweep
		// collects the tree, but a sweep that keeps failing the same way
		// would otherwise fill the disk with no trail. [LAW:no-silent-failure]
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the failed take's clone %s: %w", dir, rmErr))
		}
		// The hold's cost travels with the failure: a cut hold reported as
		// hold=0s is the one reading a cut's explanation tells the operator
		// to consult, erased. [LAW:no-silent-failure]
		return liveClone{held: clone.held}, cut, err
	}
	return clone, false, nil
}

// takeMirrorClone takes this cycle's clone of the live store under exactly
// the holds a file-by-file copy of the Dolt directory needs (withDoltDirectoryHeld:
// workspace shared, Dolt's journal lock, commit lock), clears the
// mirror-pending marker inside that same hold, and releases everything before
// returning. The live store is held for the clone and nothing else — the
// network round trip runs from the clone with none of them held.
//
// The clear is inside the hold on purpose. Every commit lands through a write
// engine, and a write engine holds Dolt's journal lock for its lifetime, so
// no commit can land between the clone and the clear: a claim stamped before
// the clear belongs to a commit the clone holds (its command's session
// closed before this hold was taken), and a claim stamped after it survives
// for the caller's post-release re-check, which runs another cycle for it.
// Clearing after the release would open a window in which a commit lands,
// claims, and has its claim erased by a clone that does not hold it — the
// stranded tail.
// [LAW:no-ambient-temporal-coupling]
//
// Residue first: the caller holds the single-flight sync-push lock, so any
// tree under the clone base belongs to a mirror that died holding it (a
// SIGKILL, power loss — every code-running ending removes its own clone), and
// the kernel-proven exclusivity of that lock is what makes the sweep safe.
// The take is the one point every cycle reaches before new disk is consumed,
// which is why collection lives here and not on some later path a crash can
// skip. [LAW:no-ambient-temporal-coupling]
//
// The clone itself runs under store.MirrorHoldBudget, and only the clone:
// the lock waits before it are bounded by their own retry budgets and are
// waiting, not holding. A clone still running at the deadline is not a slow
// copy, it is a stalled one, and the cut is reported as such through the
// could-not-attempt seam — the hold explanation names the step so the reader
// does not go looking for a network fault.
func takeMirrorClone(ctx context.Context, stderr io.Writer, ws workspace.Info) (mirrorClone, error) {
	clone, cut, err := cloneLiveStore(ctx, ws, mirrorCloneBase(ws), func() { clearMirrorPending(stderr, ws) })
	if err != nil {
		err = fmt.Errorf("take the push's clone: %w", err)
		if cut {
			err = fmt.Errorf("%w: %w", holdBudgetCutExplanation("cloning the store"), err)
		}
		return mirrorClone{liveClone{held: clone.held}}, err
	}
	return mirrorClone{clone}, nil
}

// remove discards the cycle's clone. Loud on the log, never fatal: the push
// this clone served has already completed and been recorded (or the clone
// never served one), and a tree left behind is collected by the next cycle's
// sweep. [LAW:no-silent-failure]
func (c mirrorClone) remove(log io.Writer) {
	if err := os.RemoveAll(c.dir); err != nil {
		fmt.Fprintf(log, "%s mirror clone not removed (%v); the next cycle's sweep collects it\n", time.Now().UTC().Format(time.RFC3339), err)
	}
}

// pushedHead is what a landed push established at the peer, read from the
// clone that pushed: the remote-tracking ref the live store must now carry.
// The zero value means no push landed this cycle, so there is nothing to
// record. [LAW:types-are-the-program]
type pushedHead struct {
	remote storage.RemoteName
	branch string
	head   string
	// proven is what the push proved it left the remote advertising, recorded
	// on the live store with the head; zero when it proved nothing.
	proven remoteAdvertisement
	// followedReRoot says the clone followed a re-root of the remote, a
	// peer's before the push or its own history bound's after it, so the
	// live store must follow the same re-root to reach where the clone is.
	followedReRoot bool
	// reRoot is how far the clone's history bound got its re-root toward the
	// remote after the push (SyncPushResult.Bound.Publication): published or
	// possibly so, the remote may carry it in head's place.
	reRoot storage.ReRootPublication
	// pruned is every tracking ref the clone's fetches pruned, for the
	// receive that lands a re-rooted push (receivePushedReRoot) to carry
	// over. RecordPushedHead moves the sync ref alone, so after any other push
	// the next receive's own fetch prunes the live store.
	pruned []storage.PrunedRef
}

// landed reports whether a push landed and its head is known.
func (p pushedHead) landed() bool { return p.head != "" }

// liveStoreStep is how the live store learns where a mirror cycle left the
// remote. [LAW:types-are-the-program] the cycle's facts decide it once,
// and mirrorCycle runs what it names.
type liveStoreStep int

const (
	// liveStoreWaits leaves it to the next receive, which fetches.
	liveStoreWaits liveStoreStep = iota
	// liveStoreReceives lands the clone's fetch and follows its re-root.
	liveStoreReceives
	// liveStoreRecords records the pushed head on the live store.
	liveStoreRecords
)

// String is the step's name on the event.
func (s liveStoreStep) String() string {
	switch s {
	case liveStoreWaits:
		return "wait"
	case liveStoreReceives:
		return "receive"
	case liveStoreRecords:
		return "record"
	default:
		panic(fmt.Sprintf("liveStoreStep: unknown step %d", int(s)))
	}
}

// liveStoreStep is the step for p: nothing landed waits; a clone that
// followed a re-root is received, since the live store holds none of the
// rewritten commits; a push whose bound published its re-root, or may have,
// with the clone not following, waits, since the remote may no longer carry
// its head; a landed push whose bound's re-root never reached the remote, or
// that the remote does not carry, records its head.
func (p pushedHead) liveStoreStep() liveStoreStep {
	switch {
	case !p.landed():
		return liveStoreWaits
	case p.followedReRoot:
		return liveStoreReceives
	}
	switch p.reRoot {
	case storage.ReRootPublished, storage.ReRootPossiblyPublished:
		return liveStoreWaits
	case storage.ReRootNotPushed, storage.ReRootNotCarried:
		return liveStoreRecords
	default:
		panic(fmt.Sprintf("liveStoreStep: unknown re-root publication %d", int(p.reRoot)))
	}
}

// mirrorCycle is one full cycle of the mirror: take the clone under the live
// store's locks and release them, push from the clone under the push
// deadline, record the pushed head on the live store, discard the clone. It
// reports whether the push attempt was reached; false means the failure was
// already completed through the push-outcome seam and the caller must stop
// rather than loop on a broken precondition.
//
// The push — performSyncPush clears nothing here (the take already did, see
// takeMirrorClone) and completes the attempt's outcome record on every path —
// runs under store.MirrorPushDeadline. Nothing on the live store waits on it,
// but the mirror process holds the single-flight lock for its whole run and
// every mirror spawned meanwhile loses that race and exits, so a transport
// that stalls would stop pushes for as long as it cared to. The deadline
// bounds the clone session's open and its push alike: each statement runs on
// the ctx it is called with, so the push's deadline reaches the engine's git
// subprocesses (TestMirrorPushHoldsNothingOnTheLiveStore cuts a wedged
// `git push` this way). Completion effects (the outcome marker, the
// owner-notify hook) run under the parent ctx: a cut push needs them most at
// exactly the moment its own deadline has expired.
// [LAW:no-ambient-temporal-coupling]
//
// log receives one line at cycle start, one per hold on the live store as it
// is released (`hold released step=clone|record elapsed=`), and one at cycle
// end carrying every phase's cost — the detached worker's stdout is
// mirror.log, and those lines are the durable record that the contract
// "every hold under one second" is checked against in the field. push= is
// the clone session end to end — the open and the push, which the push
// deadline bounds, and the close after them, which it does not — so a cut
// reads against it directly; open= is the open's share of it, so a push=
// that grew is attributable to the open or to the network from the log
// alone. [LAW:nothing-unseen] A landed push's history bound runs inside
// push= too, under its own deadline; its share is history_bound.elapsed_ms.
// Only a cycle that holds the single-flight lock writes: a mirror that loses
// the race stays silent, as the quiescence property requires.
func mirrorCycle(ctx context.Context, stderr io.Writer, env environ.Env, log io.Writer, ws workspace.Info, stopAnswering func()) (attempted bool) {
	start := time.Now()
	fmt.Fprintf(log, "%s mirror cycle start (hold budget %s, push deadline %s)\n", start.UTC().Format(time.RFC3339), store.MirrorHoldBudget, store.MirrorPushDeadline)
	clone, err := takeMirrorClone(ctx, stderr, ws)
	if err != nil {
		_ = completeMirrorWithoutAttempt(ctx, stderr, env, ws, err, stopAnswering)
		fmt.Fprintf(log, "%s mirror cycle end attempted=false push_deadline_cut=false hold=%s elapsed=%s\n",
			time.Now().UTC().Format(time.RFC3339), clone.held.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
		return false
	}
	defer clone.remove(log)
	fmt.Fprintf(log, "%s mirror hold released step=clone elapsed=%s\n", time.Now().UTC().Format(time.RFC3339), clone.held.Round(time.Millisecond))

	pushStart := time.Now()
	pushCtx, cancel := context.WithTimeout(ctx, store.MirrorPushDeadline)
	defer cancel()
	// ending is the error the attempt ended on, every step's verdict on the
	// push deadline already taken inside it where that step returned. A push
	// that dies of ITS OWN deadline (not the process's teardown) names the
	// deadline in every record of it, so the next hung-remote episode is
	// attributable. [LAW:no-silent-failure] [LAW:single-enforcer]
	var ending error
	var landed pushedHead
	var openElapsed time.Duration
	attempted = func() bool {
		session, closeStore, err := openSyncSessionAt(pushCtx, env, clone.databasePath, ws.WorkspaceID)
		openElapsed = time.Since(pushStart)
		if err != nil {
			ending = cutByPushDeadline(pushCtx, ctx, fmt.Errorf("open sync store on the clone: %w", err))
			_ = completeMirrorWithoutAttempt(ctx, stderr, env, ws, ending, stopAnswering)
			return false
		}
		defer closeStore()
		landed, ending = mirrorOnce(pushCtx, ctx, stderr, env, session, ws)
		return true
	}()
	pushElapsed := time.Since(pushStart)

	record, recordHeld := liveStoreLearns(ctx, stderr, env, log, ws, clone.liveClone, landed)
	fmt.Fprintf(log, "%s mirror cycle end attempted=%t push_deadline_cut=%t hold=%s record=%s ref=%s open=%s push=%s elapsed=%s\n",
		time.Now().UTC().Format(time.RFC3339), attempted, pushDeadlineCutOf(ending),
		clone.held.Round(time.Millisecond), recordHeld.Round(time.Millisecond), record, openElapsed.Round(time.Millisecond), pushElapsed.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	return attempted
}

// liveStoreLearns runs the live-store step a cycle's facts chose
// (pushedHead.liveStoreStep) and puts the step, and how it ended, on the
// event: one value per cycle, since a mirror invocation can run several.
// [LAW:nothing-unseen] It returns the pushed head it recorded and how long
// the record held the live store, both zero unless the step records.
//
// A push of a re-root the clone followed is received on the live store
// rather than recorded: the live store holds none of the rewritten commits,
// so it lands the clone's fetch and follows the same re-root, which arrives
// at the hashes the clone pushed. [LAW:one-source-of-truth] the tracking ref
// still says where the remote is, through the receive's own landing.
func liveStoreLearns(ctx context.Context, stderr io.Writer, env environ.Env, log io.Writer, ws workspace.Info, clone liveClone, landed pushedHead) (record store.PushedHeadRecord, recordHeld time.Duration) {
	step := landed.liveStoreStep()
	var stepErr error
	switch step {
	case liveStoreReceives:
		stepErr = receivePushedReRoot(ctx, stderr, env, ws, clone, landed)
		fmt.Fprintf(log, "%s mirror step=receive_reroot received=%t err=%v\n", time.Now().UTC().Format(time.RFC3339), stepErr == nil, stepErr)
	case liveStoreRecords:
		recordStart := time.Now()
		var recordErr error
		record, recordErr = store.RecordPushedHead(ctx, ws.DatabasePath, landed.remote, landed.branch, landed.head, landed.proven.record())
		recordHeld = time.Since(recordStart)
		if recordErr != nil {
			// The push landed and its outcome stands; what failed is the
			// live store's bookkeeping of it, which the next fetch repairs.
			// Loud in the trail, never a re-coloring of the push. A cut is
			// named as the budget's doing, through the one wording the clone
			// step uses. [LAW:no-silent-failure] [LAW:one-source-of-truth]
			if errors.Is(recordErr, store.ErrHoldCut) {
				recordErr = fmt.Errorf("%w: %w", holdBudgetCutExplanation("recording the pushed head"), recordErr)
			}
			// Which write failed decides the consequence: the tracking ref's
			// (freshness reads are wrong until the next fetch) or only the
			// received-refs record's (the next receive fetches), which says so
			// in its own error.
			if errors.Is(recordErr, store.ErrReceivedRefsNotRecorded) {
				stepErr = fmt.Errorf("record the push on the live store: %w", recordErr)
			} else {
				stepErr = fmt.Errorf("record the pushed head on the live store (freshness reads say \"not pushed\" until the next fetch): %w", recordErr)
			}
			recordMirrorTraceError(stderr, env, ws, stepErr)
		}
		fmt.Fprintf(log, "%s mirror hold released step=record elapsed=%s ref=%s\n", time.Now().UTC().Format(time.RFC3339), recordHeld.Round(time.Millisecond), record)
	}
	telemetry.Append(ctx, "mirror.live_store_steps", map[string]any{
		"mirror.live_store_step":  step.String(),
		"mirror.live_store_error": telemetry.ErrorText(stepErr),
	})
	return record, recordHeld
}

// receivePushedReRoot brings the live store to a push its clone made after
// following a re-root of the remote: the clone's fetch is landed and settled
// on the live store exactly as the automatic receive does it
// (performSyncReceive), and the settle follows the re-root the clone followed.
// What it established is surfaced through the receive's one contract, and
// where it stopped — the landing, the open, the remotes, the settle or the
// reconcile (outcome.err, already traced by performSyncReceive) — is returned,
// for the mirror's log. It does not hold the receive lock, so it reads the run
// of post-fetch stops and leaves writing it to the automatic receive, which
// fetches past the head this push moved and counts its own stop
// (leaveUnsettledRun).
func receivePushedReRoot(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, clone liveClone, landed pushedHead) error {
	target := syncTarget{remote: landed.remote, branch: landed.branch, skip: syncTargetReady}
	outcome, closeLive := performSyncReceive(ctx, stderr, env, ws, clone, receiveFetch{target: target, pruned: landed.pruned}, leaveUnsettledRun(ws.DatabasePath))
	if err := closeLive(); err != nil {
		fmt.Fprintf(stderr, "lit: on-change mirror: live store not closed cleanly after receiving its re-rooted push: %v\n", err)
	}
	if outcome.traceErr != nil {
		fmt.Fprintf(stderr, "lit: on-change mirror: receive trace not recorded: %v\n", outcome.traceErr)
	}
	surfaceReceiveOutcome(ctx, stderr, env, ws, outcome, time.Now())
	if outcome.err != nil {
		return fmt.Errorf("receive the re-rooted push on the live store: stopped at %s: %w", outcome.stopped, outcome.err)
	}
	return nil
}

// holdBudgetCutExplanation is the one wording of "the holder cut itself
// loose": the fact that explains a hold killed by the hold budget, for either
// hold the mirror takes on the live store — step names which ("cloning the
// store", "recording the pushed head"). It refuses to name a cause beyond
// what the deadline established — the hold ran past twice the slowest healthy
// one ever measured — and names the evidence that separates a stalled disk
// from an undersized budget. [LAW:one-source-of-truth] [FRAMING:representation]
func holdBudgetCutExplanation(step string) error {
	return fmt.Errorf(
		"mirror hold exceeded its %s budget %s — a deadline, not a diagnosis: check %s before blaming the disk. Its hold= and record= values say which: holds clustered just under the budget mean the budget is sized under this store's real cost; one hold far past it means the work stopped making progress. The live store is released as the cut unwinds, and the next mutation's mirror retries",
		store.MirrorHoldBudget, step, mirrorLogName)
}

// pushDeadlineCut is an attempt's ending error once the push deadline is
// known to have cut it: the verdict and its explanation are one value, so no
// record can name the deadline without the verdict or carry the verdict
// without naming it. It unwraps to the cause, so every classification of
// the ending (canceled, busy, diverged, remote schema ahead) still reads it.
// The explanation is rendered when the cut is taken, so it names the deadline
// that cut the attempt, not whatever the deadline reads when the error is.
// cutByPushDeadline is its one constructor. [LAW:types-are-the-program]
type pushDeadlineCut struct{ explanation, cause error }

func (c pushDeadlineCut) Error() string {
	return c.explanation.Error() + ": " + c.cause.Error()
}

func (c pushDeadlineCut) Unwrap() error { return c.cause }

// cutByPushDeadline takes an attempt's one verdict on whether its push
// deadline cut it, at the moment a step that deadline bounds returns on err:
// the operation lifetime (ctx) has ended while the completion lifetime has
// not. Foreground callers pass one lifetime twice, so it never holds for
// them. Every later reader — the log line, the trace, the outcome record —
// reads the verdict off the error with pushDeadlineCutOf, never off a clock,
// whose reading would drift with whatever ran after the step.
// [LAW:single-enforcer]
func cutByPushDeadline(ctx, completionCtx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || completionCtx.Err() != nil {
		return err
	}
	return pushDeadlineCut{explanation: pushDeadlineCutExplanation(), cause: err}
}

// pushDeadlineCutOf reads the verdict cutByPushDeadline took for the
// attempt err ended it.
func pushDeadlineCutOf(err error) bool {
	var cut pushDeadlineCut
	return errors.As(err, &cut)
}

// pushDeadlineCutExplanation is the one wording of "the push cut itself
// loose": the fact that explains a raw transport error killed by the push
// deadline. pushDeadlineCut says it through this function, so the durable
// trail names the deadline identically wherever the cut landed.
// [LAW:one-source-of-truth]
//
// It refuses to name a cause, and that refusal is the point. A deadline knows
// only that the work ran long. Naming a cause such as "a hung or slow remote
// transport" would be a diagnosis nothing had observed, and it reads as
// environmental and transient — something to retry past rather than a defect to
// file. The text names what the deadline actually established, names every
// cause that lands here — an undersized deadline, a stalled transport, a
// stalled open of the clone (the open runs under the same deadline) — and
// points at the evidence that separates them.
// [FRAMING:representation] a message that asserts more than its signal carries
// is a map of a territory nobody visited.
//
// Order matters as much as content: the FAILING banner prints this through
// oneLineReason, which keeps the first line and caps it at 160 runes, so the
// honest framing has to arrive before the truncation rather than after it —
// and with enough margin that a reworded lead or a deadline that formats
// longer (a value in minutes renders as "1h40m0s", not "40s") cannot push it
// over. TestDeadlineCutFramingSurvivesTheBanner pins that, because a margin
// nobody measures is how an invariant asserted in a comment stops being one.
func pushDeadlineCutExplanation() error {
	return fmt.Errorf(
		"mirror push exceeded its %s deadline — a deadline, not a diagnosis: check %s's push= values before blaming the remote. Pushes clustered just under the deadline mean it is sized under this workspace's real push cost; one push far past it means the transport stopped answering, or the clone's open did when open= carries the time. Nothing on the live store waits on the push, and the next mutation's mirror retries it",
		store.MirrorPushDeadline, mirrorLogName)
}

// mirrorOnce runs the one shared push path from the clone's session, without
// compaction, and reports the head a landed push established, or the error
// the attempt ended on — recorded already, its push-deadline verdict taken
// inside it — when it landed nothing; a skip ends on neither. It is a single
// path with no freshness branch: [LAW:dataflow-not-control-flow] the skip
// decisions (no remote, empty remote) already live in performSyncPush, and an
// up-to-date push is a cheap no-op, so the mirror does not pre-decide whether
// to push. It never re-pushes in the same session, either: the clone is a
// frozen copy, so an in-session HEAD re-read can never see a newer commit —
// commits land on the live store, between cycles. Coalescing of a burst comes
// from dolt push sending the clone's HEAD (commits that landed before the
// clone was taken go out with it) funnelled through the single-flight lock; a
// commit that lands after the clone is a fresh mirror-pending claim, and the
// caller's post-release re-check answers it with another whole cycle. The
// unsynced window shrinks toward zero without ever blocking a mutation.
func mirrorOnce(ctx, completionCtx context.Context, stderr io.Writer, env environ.Env, session syncSession, ws workspace.Info) (pushedHead, error) {
	// The mirror pushes without compaction, from a clone: SyncPushFromClone,
	// never the compact-and-push variant the explicit command uses. The clone
	// variant is what makes a race with that explicit command safe — nothing
	// serializes the two, so the explicit push can land a later commit while
	// this one is in flight, and the rejection that follows is judged by
	// whether the remote carries the clone's head, not reported as a failure
	// over a remote that is fully up to date.
	outcome, err := performSyncPush(ctx, completionCtx, stderr, env, session, ws, "", false, false, session.syncer.SyncPushFromClone)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): performSyncPush's
		// own deferred completion already recorded the outcome, but no trace,
		// so this is the event's one trace owner. [LAW:single-enforcer]
		recordMirrorTraceError(stderr, env, ws, err)
		return pushedHead{}, err
	}
	// performSyncPush records its own trace (push-ok, push-failure, or skip). If
	// that trace write itself failed, surface it rather than drop it. [LAW:no-silent-failure]
	if outcome.traceErr != nil {
		fmt.Fprintf(stderr, "lit: on-change mirror trace not recorded: %v\n", outcome.traceErr)
	}
	// A remote schema ahead of this binary is NOT the "next push retries" case: it
	// will never succeed until the binary is upgraded, so surface the one
	// sync-failure contract to stderr instead of letting it read as a transient
	// hiccup.
	// [LAW:single-enforcer] one adapter, one contract. Other pushErr (e.g. offline)
	// is already captured in the trace; the mutation is durable locally and the next
	// push retries, so the mirror stops cleanly either way.
	if failure, ok := remoteSchemaAheadFailure(outcome.pushErr); ok {
		fmt.Fprintln(stderr, failure.blockString())
	}
	if outcome.skip != syncTargetReady || outcome.pushErr != nil {
		return pushedHead{}, outcome.pushErr
	}
	// The head the push reports is the clone's HEAD — the push is
	// HEAD:<branch> and the clone is frozen, so nothing moved it but a follow
	// of a re-root, a peer's or the one the clone's history bound published,
	// which followedReRoot reports. The live store learns it through
	// RecordPushedHead, or by receiving a re-rooted push.
	return pushedHead{remote: outcome.remote, branch: outcome.branch, head: outcome.head, proven: outcome.proven, followedReRoot: len(session.syncer.FollowedReRoots()) > 0, reRoot: outcome.reRoot, pruned: session.syncer.PrunedRefs()}, nil
}

// waitForParentExit blocks until the spawning command has exited, returning
// true, or the timeout elapses with it still alive, returning false. It watches
// the worker's own parent pid (getppid): the detached worker is a direct child
// of the spawning command, so when that command exits the worker is reparented
// (to init or a subreaper) and getppid stops equalling parentPID. This is robust
// where a kill(pid,0) probe is not — a zombie parent still answers kill(pid,0)
// until it is reaped (delaying the mirror past the actual exit), and a reused
// pid could be mistaken for the original; reparenting happens at exit, before
// reaping, and getppid reports the real current parent. [LAW:no-ambient-temporal-coupling]
// The ordering owner is the getppid check, not the sleep, which is only the poll
// interval; the boolean distinguishes "parent exited" from "parent outlived the
// wait" so the caller aborts rather than proceeding blindly on a timeout.
// getppid is a parameter so the wait is testable without a real process tree.
// A done context also ends the wait (false): a mirror in the SIGTERM grace
// window must spend it releasing state, not sleeping toward a deadline; the
// caller tells teardown from timeout by ctx.Err().
func waitForParentExit(ctx context.Context, parentPID int, getppid func() int, timeout, poll time.Duration) bool {
	if parentPID <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for getppid() == parentPID {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
	return true
}

// completeMirrorWithoutAttempt is the completion path for a mirror that died
// BEFORE reaching a push attempt (parent-wait timeout, sync-push-lock error,
// engine-open failure). These endings flow through the same completePushAttempt
// seam as an attempt that ran, so the push-outcome marker and the owner
// notification hear "the push layer could not even start" from the one record
// they already share — never a second representation of push health.
// [LAW:one-source-of-truth] The trace half stays: it is the ordered audit log,
// not the "where do things stand" marker. Always returns nil: the mutation is
// already durable, so the mirror is best-effort and never reports a non-zero
// exit.
//
// The dying mirror also stops answering on the beacon and releases the
// mirror-pending claim it was spawned to answer — both FIRST, before the
// completion effects, because the completion can run the owner-notify hook
// for up to its cap and every mutation landing in that window would
// otherwise read live coverage from a mirror already known dead: a beacon
// still held would make the probe truthfully answer "alive" about a process
// that will never push, and a claim still present would read as covered
// under it. Running both up front bounds that exposure to the inherent
// instants of the release and removal; the next mutation then re-claims and
// re-spawns at once. (The endings that run no code at all — SIGKILL, power
// loss — drop the beacon with the process instead, and the next claim's
// probe recovers the marker the moment it runs.) Racing a newer live claim
// errs toward the safe side: a claim removed early only makes some mutation
// spawn a redundant mirror, while a claim left behind would falsely read as
// coverage. [LAW:no-ambient-temporal-coupling] [LAW:single-enforcer] the
// stop-before-effects ordering lives here and in teardownMirror, not at the
// call sites, so every current and future pre-attempt death gets it by
// construction.
func completeMirrorWithoutAttempt(ctx context.Context, stderr io.Writer, env environ.Env, ws workspace.Info, cause error, stopAnswering func()) error {
	stopAnswering()
	clearMirrorPending(stderr, ws)
	completePushAttempt(ctx, stderr, env, ws, syncPushOutcome{}, cause)
	recordMirrorTraceError(stderr, env, ws, cause)
	return nil
}

// recordMirrorTraceError writes a mirror failure to the shared automation
// trace so a detached mirror's ending is loud out-of-band rather than silent.
// [LAW:no-silent-failure] If the trace write itself fails, the error is not
// swallowed — it goes to stderr, the worker's only remaining channel
// (discarded when detached, visible when the hidden subcommand is run in the
// foreground for debugging). Traces only: the push-outcome completion either
// already ran inside performSyncPush (a failure after the attempt started) or
// is completeMirrorWithoutAttempt's job (a failure before it) — this function
// writing it too would double-complete one attempt.
func recordMirrorTraceError(stderr io.Writer, env environ.Env, ws workspace.Info, cause error) {
	if _, traceErr := maybeRecordAutomatedCommandTrace(
		env,
		ws,
		"lit sync push",
		"mirror Dolt data to the configured git remote",
		"error",
		cause.Error(),
		map[string]string{"error": cause.Error()},
	); traceErr != nil {
		fmt.Fprintf(stderr,
			"lit: on-change mirror could not record failure trace (%v); original error: %v\n",
			traceErr, cause)
	}
	// recordSyncCommandTrace already sets Reason from cause; no metadata needed
	// to carry the same string a second time.
	recordSyncCommandTrace(stderr, env, ws, "lit sync push", "error", cause, nil)
}
