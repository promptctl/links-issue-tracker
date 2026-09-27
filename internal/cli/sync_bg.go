package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// backgroundMirrorSubcommand is the hidden `lit sync` subcommand the on-change
// cadence owner spawns. It is absent from the family usage string, so it never
// appears in help; it exists only as the detached worker's entrypoint.
const backgroundMirrorSubcommand = "__mirror-bg"

// mirrorLogName is the detached worker's durable output sink. A detached
// process owns no terminal, so its stdout/stderr must land somewhere inspectable
// rather than /dev/null — otherwise a trace-write failure or a panic vanishes.
const mirrorLogName = "mirror.log"

// mirrorLogMaxBytes caps mirror.log's growth now that every cycle logs a
// start/end line (previously only failures wrote, which is why the field's log
// sat at 0 bytes while mirrors had been pushing for weeks — the attribution
// gap links-sync-pgct.11.1 closes). Rotation keeps one previous generation so
// the recent window survives each cut; the log is diagnostics, not state, so
// older lines are free to go.
const mirrorLogMaxBytes = 256 * 1024

// rotateMirrorLog moves an over-cap mirror.log aside (one kept generation)
// before the next worker appends. A rotation problem is reported, never fatal:
// the worst outcome of skipping it is a log that keeps growing, which must not
// cost a mirror. [LAW:no-silent-failure]
func rotateMirrorLog(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= mirrorLogMaxBytes {
		// Absent is the common first-spawn case and needs no rotation; any
		// other stat failure will resurface loudly from OpenFile just after.
		return nil
	}
	if renameErr := os.Rename(path, path+".1"); renameErr != nil && !errors.Is(renameErr, fs.ErrNotExist) {
		// A missing source means a concurrent spawner rotated between the stat
		// and the rename — the rotation happened, just not by this process.
		return renameErr
	}
	return nil
}

const (
	// parentPostSpawnTail is how long a HEALTHY parent can legitimately live
	// after spawning the mirror: every bounded step maybeAutoSyncAfterCommand
	// has scheduled for after the spawn, summed from those steps' own caps.
	//
	// It is a sum rather than a number because this was previously a hand-kept
	// figure in prose ("15s + 10s + 1s, so ~26s"), and prose does not fail to
	// compile when a fourth step joins the tail. It did: the compaction
	// backstop was added after the spawn and, left unsummed here, a pass slower
	// than the leftover margin would have let a perfectly healthy parent outlive
	// the wait below — abandoning a mirror that owed a push, for work the parent
	// was designed to do. Adding a step to the tail now means adding it here.
	// [LAW:one-source-of-truth]
	parentPostSpawnTail = store.InlineReceiveDeadline + // the inline receive
		ownerNotifyHookTimeout + ownerNotifyPipeWaitDelay + // a divergence's owner-notify hook and its pipe
		compactTimeout // the compaction backstop

	// mirrorParentWaitMargin is the headroom above the parent's designed tail:
	// scheduling slop on a loaded machine, not another step. A bound inside the
	// tail would manufacture parent-wait failures out of the parent's own work,
	// so the margin exists to keep the two clearly separated.
	mirrorParentWaitMargin = 30 * time.Second

	// mirrorParentWaitTimeout bounds the wait for the spawning command to
	// release its engine. The wait ends the instant the parent exits; the cap
	// only guards a parent that never exits (e.g. a long-lived REPL), in which
	// case the mirror gives up rather than hang forever.
	mirrorParentWaitTimeout = parentPostSpawnTail + mirrorParentWaitMargin
	mirrorParentPollDelay   = 20 * time.Millisecond
)

// spawnBackgroundMirror starts the detached mirror and returns immediately,
// without waiting for it. [LAW:effects-at-boundaries] The mutating command's
// change is already durable in the local Dolt store; getting it to the remote
// is an effect pushed entirely off the command's own latency path into a
// separate process. The automation-trace env is set here so a push that runs
// and fails records a trace through the one shared writer the pre-push hook
// already uses. [LAW:one-source-of-truth]
func spawnBackgroundMirror(ws workspace.Info, parentPID int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve lit binary: %w", err)
	}
	cmd := exec.Command(self, "sync", backgroundMirrorSubcommand, "--parent-pid", strconv.Itoa(parentPID))
	cmd.Dir = ws.RootDir
	cmd.Stdin = nil
	// Route the detached worker's output to a durable log. [LAW:no-silent-failure]
	// If the log cannot be opened, surface that on the command's terminal-attached
	// stderr and still spawn with discarded streams — the mirror matters more than
	// its log, and the inability to log is itself loud here rather than swallowed.
	logPath := filepath.Join(ws.StorageDir, mirrorLogName)
	if rotateErr := rotateMirrorLog(logPath); rotateErr != nil {
		fmt.Fprintf(os.Stderr, "lit: mirror log rotation failed (%v); the log keeps growing past its cap\n", rotateErr)
	}
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change mirror log unavailable (%v); worker output will be discarded\n", logErr)
	} else {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Env = mirrorEnv()
	startErr := cmd.Start()
	if logFile != nil {
		// The child inherited its own dup of the fd at exec; the parent's copy is
		// no longer needed. Closing it cannot affect the child's logging.
		_ = logFile.Close()
	}
	return startErr
}

// mirrorEnv builds the detached mirror's environment: the parent's environment
// with every automation-trace variable stripped, then the mirror's own trigger
// and reason set. [LAW:one-source-of-truth] The parent's
// LNKS_AUTOMATION_TRACE_REF_FILE points at a file the parent's caller reads to
// learn which trace the command recorded; the detached mirror must not inherit
// it and overwrite that file with its own trace path after the command has
// returned. The mirror has no reader for a trace-ref file, so it carries none —
// it records traces by trigger alone.
func mirrorEnv() []string {
	stripped := []string{
		automationTriggerEnvVar + "=",
		automationReasonEnvVar + "=",
		automationTraceRefFileEnvVar + "=",
	}
	parent := os.Environ()
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		keep := true
		for _, prefix := range stripped {
			if strings.HasPrefix(kv, prefix) {
				keep = false
				break
			}
		}
		if keep {
			env = append(env, kv)
		}
	}
	return append(env,
		automationTriggerEnvVar+"=on-change",
		automationReasonEnvVar+"=on-change cadence mirrored after a mutating command",
	)
}

// backgroundMirrorLeaf is the detached worker. It runs as its own process after
// the spawning command has returned, so it establishes the engine-release
// invariant first (wait-for-parent), then runs single-flight push cycles until
// no mirror-pending claim remains. [LAW:no-ambient-temporal-coupling]
//
// The cycle loop is what makes losing the single-flight race safe to treat as
// a silent exit (links-sync-pgct.12): the loser's spawner claimed the
// mirror-pending marker, and the current lock holder is obligated to re-check
// that marker AFTER releasing — a claim it cannot have covered (stamped while
// its engine was open, or after) triggers another full cycle on a fresh
// engine, whose open then postdates the claimant's commit. Custody of the
// marker passes from holder to holder at the lock, never resting on timing.
// Losing therefore never strands a claim, and the loser still exits without
// opening a store, writing a trace, or creating a file — the quiescence
// property test cleanups rely on.
func backgroundMirrorLeaf() wsLeaf {
	fs := newCobraFlagSet("sync " + backgroundMirrorSubcommand)
	parentPID := fs.Int("parent-pid", 0, "PID of the spawning command; the mirror waits for it to exit")
	return wsLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
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
				return teardownMirror(ws, ctx.Err(), stopAnswering)
			}
			return completeMirrorWithoutAttempt(ctx, ws, fmt.Errorf("hold mirror liveness beacon: %w", beaconErr), stopAnswering)
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
					fmt.Fprintf(os.Stderr, "lit: mirror beacon not released (%v); concurrent claims may read this dying mirror as live until process exit\n", relErr)
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
		if !waitForParentExit(ctx, *parentPID, os.Getppid, mirrorParentWaitTimeout, mirrorParentPollDelay) {
			if ctx.Err() != nil {
				return teardownMirror(ws, ctx.Err(), stopAnswering)
			}
			return completeMirrorWithoutAttempt(ctx, ws, fmt.Errorf(
				"spawning command (pid %d) still running after %s; skipping mirror to avoid racing its engine",
				*parentPID, mirrorParentWaitTimeout), stopAnswering)
		}

		for {
			// Teardown owns the loop's lifetime: once the context is done (the
			// SIGTERM grace window), starting another engine cycle would fight the
			// shutdown for its last seconds. [LAW:no-ambient-temporal-coupling]
			if ctx.Err() != nil {
				return teardownMirror(ws, ctx.Err(), stopAnswering)
			}
			release, acquired, err := store.TryAcquireSyncPushLock(ws.DatabasePath)
			if err != nil {
				return completeMirrorWithoutAttempt(ctx, ws, fmt.Errorf("acquire sync-push lock: %w", err), stopAnswering)
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
			attempted := mirrorCycle(ctx, stdout, ws, stopAnswering)
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
				recordMirrorTraceError(ws, recheckErr)
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
	}}
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
func teardownMirror(ws workspace.Info, cause error, stopAnswering func()) error {
	stopAnswering()
	clearMirrorPending(ws)
	recordMirrorTraceError(ws, cause)
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
	// dir is the cycle's directory: the clone's dolt root and the lock files
	// its own engine mints live under it, and removing it removes them all.
	dir string
	// databasePath is the cloned dolt root, an engine-openable store.
	databasePath string
	// held is how long the live store was held for the take: from the last
	// lock acquired to the first released, measured inside the hold.
	held time.Duration
}

// takeMirrorClone takes this cycle's clone of the live store under exactly
// the holds a file-by-file copy of the Dolt directory needs (withDoltDirectoryHeld:
// workspace shared, Dolt's journal lock, commit lock), clears the
// mirror-pending marker inside that same hold, and releases everything before
// returning. The live store is held for the clone and nothing else — that is
// the whole change links-scale-om3r.s2h makes: the network round trip that
// used to run under these locks now runs from the clone with none of them
// held.
//
// The clear is inside the hold on purpose. Every commit lands through a write
// engine, and a write engine holds Dolt's journal lock for its lifetime, so
// no commit can land between the clone and the clear: a claim stamped before
// the clear belongs to a commit the clone holds (its command's session
// closed before this hold was taken), and a claim stamped after it survives
// for the caller's post-release re-check, which runs another cycle for it.
// Clearing after the release would open a window in which a commit lands,
// claims, and has its claim erased by a clone that does not hold it — the
// stranded tail links-sync-pgct.12 exists to prevent.
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
func takeMirrorClone(ctx context.Context, log io.Writer, ws workspace.Info) (mirrorClone, error) {
	base := mirrorCloneBase(ws)
	if err := os.RemoveAll(base); err != nil {
		return mirrorClone{}, fmt.Errorf("collect a dead mirror's clone under %s: %w", base, err)
	}
	dir := filepath.Join(base, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return mirrorClone{}, fmt.Errorf("create mirror clone dir: %w", err)
	}
	clone := mirrorClone{dir: dir, databasePath: filepath.Join(dir, "dolt")}
	var holdStart time.Time
	var holdCut bool
	err := withDoltDirectoryHeld(ctx, ws, func() error {
		holdStart = time.Now()
		holdCtx, cancel := context.WithTimeout(ctx, store.MirrorHoldBudget)
		defer cancel()
		if err := dbsnapshot.CloneTree(holdCtx, ws.DatabasePath, clone.databasePath); err != nil {
			holdCut = holdCtx.Err() != nil && ctx.Err() == nil
			return fmt.Errorf("clone the store for the push: %w", err)
		}
		clearMirrorPending(ws)
		return nil
	})
	if !holdStart.IsZero() {
		clone.held = time.Since(holdStart)
	}
	if err != nil {
		if holdCut {
			err = fmt.Errorf("%w: %w", holdBudgetCutExplanation("cloning the store"), err)
		}
		clone.remove(log)
		// The hold's cost travels with the failure: a cut hold logged as
		// hold=0s is the one reading holdBudgetCutExplanation tells the
		// operator to consult, erased. [LAW:no-silent-failure]
		return mirrorClone{held: clone.held}, err
	}
	return clone, nil
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
	remote string
	branch string
	head   string
	// proven is what the push proved it left the remote advertising, recorded
	// on the live store with the head; zero when it proved nothing.
	proven remoteAdvertisement
}

// landed reports whether a push landed and its head is known.
func (p pushedHead) landed() bool { return p.head != "" }

// mirrorCycle is one full cycle of the mirror: take the clone under the live
// store's locks and release them, push from the clone under the push
// deadline, record the pushed head on the live store, discard the clone. It
// reports whether the push attempt was reached; false means the failure was
// already completed through the push-outcome seam and the caller must stop
// rather than loop on a broken precondition.
//
// The push — performSyncPush clears nothing here (the take already did, see
// takeMirrorClone) and completes the attempt's outcome record on every path —
// runs under store.MirrorPushDeadline. Nothing on the live store waits on it
// any more, but the mirror process holds the single-flight lock for its whole
// run and every mirror spawned meanwhile loses that race and exits, so a
// transport that stalls would stop pushes for as long as it cared to
// (links-sync-pgct.11.1). The deadline must wrap the ctx the clone's session
// is OPENED with, not just the push's: the embedded driver builds the
// connection's execution context at Connect, and only a deadline present
// there reaches the engine's git subprocesses; a per-query deadline is
// inert. Completion effects (the outcome marker, the owner-notify hook) run
// under the parent ctx: a cut push needs them most at exactly the moment its
// own deadline has expired. [LAW:no-ambient-temporal-coupling]
//
// log receives one line at cycle start, one per hold on the live store as it
// is released (`hold released step=clone|record elapsed=`), and one at cycle
// end carrying every phase's cost — the detached worker's stdout is
// mirror.log, and those lines are the durable record that the ticket's
// contract ("every hold under one second") is checked against in the field.
// Only a cycle that holds the single-flight lock writes: a mirror that loses
// the race stays silent, as the quiescence property requires.
func mirrorCycle(ctx context.Context, log io.Writer, ws workspace.Info, stopAnswering func()) (attempted bool) {
	start := time.Now()
	fmt.Fprintf(log, "%s mirror cycle start (hold budget %s, push deadline %s)\n", start.UTC().Format(time.RFC3339), store.MirrorHoldBudget, store.MirrorPushDeadline)
	clone, err := takeMirrorClone(ctx, log, ws)
	if err != nil {
		_ = completeMirrorWithoutAttempt(ctx, ws, err, stopAnswering)
		fmt.Fprintf(log, "%s mirror cycle end attempted=false push_deadline_cut=false hold=%s elapsed=%s\n",
			time.Now().UTC().Format(time.RFC3339), clone.held.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
		return false
	}
	defer clone.remove(log)
	fmt.Fprintf(log, "%s mirror hold released step=clone elapsed=%s\n", time.Now().UTC().Format(time.RFC3339), clone.held.Round(time.Millisecond))

	pushStart := time.Now()
	pushCtx, cancel := context.WithTimeout(ctx, store.MirrorPushDeadline)
	defer cancel()
	var onceErr error
	var landed pushedHead
	attempted = func() bool {
		session, closeStore, err := openSyncSessionAt(pushCtx, clone.databasePath, ws.WorkspaceID)
		if err != nil {
			_ = completeMirrorWithoutAttempt(ctx, ws, fmt.Errorf("open sync store on the clone: %w", err), stopAnswering)
			return false
		}
		defer closeStore()
		landed, onceErr = mirrorOnce(pushCtx, ctx, session, ws)
		return true
	}()
	pushElapsed := time.Since(pushStart)
	// A push that dies of ITS OWN deadline (not the process's teardown) names
	// the deadline in the durable trail — without that record the next
	// hung-remote episode is as unattributable as the first.
	// [LAW:no-silent-failure] Gated on a session having existed: a deadline
	// that expires during the OPEN reached no transport, and that branch's
	// accurate record was already written through completeMirrorWithoutAttempt.
	// One trace owner per event: an attempt that RAN recorded itself inside
	// performSyncPush (onceErr nil, the deadline explanation folded in there),
	// so the out-of-band record here exists only for a could-not-attempt
	// failure, which nothing else recorded — the deadline cause joins it
	// instead of writing its own. [LAW:single-enforcer]
	deadlineCut := attempted && pushCtx.Err() != nil && ctx.Err() == nil
	if onceErr != nil {
		var cause error
		if deadlineCut {
			cause = pushDeadlineCutExplanation()
		}
		recordMirrorTraceError(ws, errors.Join(cause, onceErr))
	}

	var recordHeld time.Duration
	var record store.PushedHeadRecord
	if landed.landed() {
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
			if errors.Is(recordErr, store.ErrMirrorHoldCut) {
				recordErr = fmt.Errorf("%w: %w", holdBudgetCutExplanation("recording the pushed head"), recordErr)
			}
			recordMirrorTraceError(ws, fmt.Errorf("record the push on the live store (tracking ref %s; while it is unrecorded, freshness reads say \"not pushed\" until the next fetch): %w", record, recordErr))
		}
		fmt.Fprintf(log, "%s mirror hold released step=record elapsed=%s ref=%s\n", time.Now().UTC().Format(time.RFC3339), recordHeld.Round(time.Millisecond), record)
	}
	fmt.Fprintf(log, "%s mirror cycle end attempted=%t push_deadline_cut=%t hold=%s record=%s ref=%s push=%s elapsed=%s\n",
		time.Now().UTC().Format(time.RFC3339), attempted, deadlineCut,
		clone.held.Round(time.Millisecond), recordHeld.Round(time.Millisecond), record, pushElapsed.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	return attempted
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

// pushDeadlineCutExplanation is the one wording of "the push cut itself
// loose": the fact that explains a raw transport error killed by the push
// deadline. Both record owners — performSyncPush folding it into an attempt's
// own record, mirrorCycle joining it to a could-not-attempt failure — say it
// through this function, so the durable trail names the deadline identically
// wherever the cut landed. [LAW:one-source-of-truth]
//
// It refuses to name a cause, and that refusal is the point. A deadline knows
// only that the work ran long; the previous wording turned that into "a hung
// or slow remote transport", a diagnosis nothing had observed, and it read as
// environmental and transient — something to retry past rather than a defect
// to file. links-sync-dauk is the bill: with the budget sized under the
// operation's own cost, 15.8% of cycles were cut, the condition was hit three
// times in one session, and each time the message sent the reader hunting a
// network fault that was not there. So the text names what the deadline
// actually established, names both causes that land here, and points at the
// evidence that separates them. [FRAMING:representation] a message that
// asserts more than its signal carries is a map of a territory nobody visited.
//
// Order matters as much as content: the FAILING banner prints this through
// oneLineReason, which keeps the first line and caps it at 160 runes, so the
// honest framing has to arrive before the truncation rather than after it —
// and with enough margin that a reworded lead or a deadline that formats
// longer (a value in minutes renders as "1h40m0s", not "40s") cannot push it
// over. TestDeadlineCutFramingSurvivesTheBanner pins that, because a margin
// nobody measures is how an invariant asserted in a comment stops being one:
// the first wording of this message left five runes of it.
func pushDeadlineCutExplanation() error {
	return fmt.Errorf(
		"mirror push exceeded its %s deadline — a deadline, not a diagnosis: check %s's push= values before blaming the remote. Pushes clustered just under the deadline mean it is sized under this workspace's real push cost; one push far past it means the transport stopped answering. Nothing on the live store waits on the push, and the next mutation's mirror retries it",
		store.MirrorPushDeadline, mirrorLogName)
}

// mirrorOnce runs the one shared push path from the clone's session, without
// compaction, and reports the head a landed push established. It is a single
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
func mirrorOnce(ctx, completionCtx context.Context, session syncSession, ws workspace.Info) (pushedHead, error) {
	// The mirror pushes without compaction, from a clone: SyncPushFromClone,
	// never the compact-and-push variant the explicit command uses. The clone
	// variant is what makes a race with that explicit command safe — nothing
	// serializes the two, so the explicit push can land a later commit while
	// this one is in flight, and the rejection that follows is judged by
	// whether the remote carries the clone's head, not reported as a failure
	// over a remote that is fully up to date.
	outcome, err := performSyncPush(ctx, completionCtx, session, ws, "", false, false, session.syncer.SyncPushFromClone)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): performSyncPush's
		// own deferred completion already recorded the outcome. The cycle's
		// single out-of-band automation trace is the caller's to write — it
		// alone knows whether the push deadline is what cut this attempt short.
		// [LAW:single-enforcer]
		return pushedHead{}, err
	}
	// performSyncPush records its own trace (push-ok, push-failure, or skip). If
	// that trace write itself failed, surface it rather than drop it. [LAW:no-silent-failure]
	if outcome.traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: on-change mirror trace not recorded: %v\n", outcome.traceErr)
	}
	// A remote schema ahead of this binary is NOT the "next push retries" case: it
	// will never succeed until the binary is upgraded, so surface the one
	// sync-failure contract to stderr instead of letting it read as a transient
	// hiccup — the exact "will retry" shrug the sync-skew epic kills.
	// [LAW:single-enforcer] one adapter, one contract. Other pushErr (e.g. offline)
	// is already captured in the trace; the mutation is durable locally and the next
	// push retries, so the mirror stops cleanly either way.
	if failure, ok := remoteSchemaAheadFailure(outcome.pushErr); ok {
		fmt.Fprintln(os.Stderr, failure.blockString())
	}
	if outcome.skip != syncTargetReady || outcome.pushErr != nil {
		return pushedHead{}, nil
	}
	// The head the push reports is the clone's HEAD — the push is
	// HEAD:<branch> and the clone is frozen, so nothing moved it. The live
	// store learns it through RecordPushedHead.
	return pushedHead{remote: outcome.remote, branch: outcome.branch, head: outcome.head, proven: outcome.proven}, nil
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
// engine-open failure). These endings were the deliberate gap links-sync-pgct.10
// left and this ticket closes: they now flow through the same
// completePushAttempt seam as an attempt that ran, so the push-outcome marker
// and the owner notification hear "the push layer could not even start" from
// the one record they already share — never a second representation of push
// health. [LAW:one-source-of-truth] The trace half stays: it is the ordered
// audit log, not the "where do things stand" marker. Always returns nil: the
// mutation is already durable, so the mirror is best-effort and never reports
// a non-zero exit.
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
func completeMirrorWithoutAttempt(ctx context.Context, ws workspace.Info, cause error, stopAnswering func()) error {
	stopAnswering()
	clearMirrorPending(ws)
	completePushAttempt(ctx, ws, syncPushOutcome{}, cause)
	recordMirrorTraceError(ws, cause)
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
func recordMirrorTraceError(ws workspace.Info, cause error) {
	if _, traceErr := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync push",
		"mirror Dolt data to the configured git remote",
		"error",
		cause.Error(),
		map[string]string{"error": cause.Error()},
	); traceErr != nil {
		fmt.Fprintf(os.Stderr,
			"lit: on-change mirror could not record failure trace (%v); original error: %v\n",
			traceErr, cause)
	}
	// recordSyncCommandTrace already sets Reason from cause; no metadata needed
	// to carry the same string a second time.
	recordSyncCommandTrace(ws, "lit sync push", "error", cause, nil)
}
