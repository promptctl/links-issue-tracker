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
	parentPostSpawnTail = receiveTimeout + // the inline receive
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

// runBackgroundMirror is the detached worker. It runs as its own process after
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
func runBackgroundMirror(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("sync " + backgroundMirrorSubcommand)
	parentPID := fs.Int("parent-pid", 0, "PID of the spawning command; the mirror waits for it to exit")
	if err := parseFlagSet(fs, args, io.Discard); err != nil {
		return err
	}

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

// mirrorCycle is one full engine session of the mirror: open, push
// (performSyncPush clears the mirror-pending marker at entry and completes the
// attempt's outcome record on every path), close. It reports whether the push
// attempt was reached; false means the failure was already completed through
// the push-outcome seam and the caller must stop rather than loop on a broken
// precondition.
//
// The whole session runs under store.MirrorHoldBudget. The push crosses the
// network while this process holds the store's one read-write engine (and its
// journal lock), and nothing on the transport side bounds how long a hung
// remote can stall it — so the bound is imposed here, by the holder
// (links-sync-pgct.11.1). The deadline must wrap the ctx the session is OPENED
// with, not just the push's: the embedded driver builds the connection's
// execution context at Connect, and only a deadline present there reaches the
// engine's git subprocesses; a per-query deadline is inert.
// [LAW:no-ambient-temporal-coupling] the hold's owner declares its bound. A
// mutation cut short loses nothing durable — the push-outcome record is loud
// and the next mutation's mirror retries.
//
// log receives one line at cycle start and one at cycle end (the detached
// worker's stdout is mirror.log), so a later store-open contention can be
// correlated against whether a mirror was mid-cycle — the attribution gap that
// made links-sync-pgct.11.1 unprovable in the field. Only a cycle that holds
// the single-flight lock writes: a mirror that loses the race stays silent, as
// the quiescence property requires.
func mirrorCycle(ctx context.Context, log io.Writer, ws workspace.Info, stopAnswering func()) (attempted bool) {
	start := time.Now()
	fmt.Fprintf(log, "%s mirror cycle start (hold budget %s)\n", start.UTC().Format(time.RFC3339), store.MirrorHoldBudget)
	cycleCtx, cancel := context.WithTimeout(ctx, store.MirrorHoldBudget)
	defer cancel()
	var onceErr error
	attempted = func() bool {
		session, closeStore, err := openSyncSession(cycleCtx, ws)
		if err != nil {
			_ = completeMirrorWithoutAttempt(ctx, ws, fmt.Errorf("open sync store: %w", err), stopAnswering)
			return false
		}
		defer closeStore()
		// Completion effects (the outcome marker, the owner-notify hook) run
		// under the parent ctx: a cut cycle needs them most at exactly the
		// moment its own budget has expired.
		onceErr = mirrorOnce(cycleCtx, ctx, session, ws)
		return true
	}()
	// A cycle that dies of ITS OWN deadline (not the process's teardown) names
	// the budget in the durable trail — without that record the next
	// hung-remote episode is as unattributable as the first.
	// [LAW:no-silent-failure] Gated on a session having existed: a budget that
	// expires during the OPEN held no engine and reached no transport, and that
	// branch's accurate record was already written through
	// completeMirrorWithoutAttempt. One trace owner per event: an attempt that
	// RAN recorded itself inside performSyncPush (onceErr nil, the budget
	// explanation folded in there), so the out-of-band record here exists only
	// for a could-not-attempt failure, which nothing else recorded — the budget
	// cause joins it instead of writing its own. [LAW:single-enforcer]
	budgetCut := attempted && cycleCtx.Err() != nil && ctx.Err() == nil
	if onceErr != nil {
		var cause error
		if budgetCut {
			cause = holdBudgetCutExplanation()
		}
		recordMirrorTraceError(ws, errors.Join(cause, onceErr))
	}
	fmt.Fprintf(log, "%s mirror cycle end attempted=%t hold_budget_cut=%t elapsed=%s\n",
		time.Now().UTC().Format(time.RFC3339), attempted, budgetCut, time.Since(start).Round(time.Millisecond))
	return attempted
}

// holdBudgetCutExplanation is the one wording of "the holder cut itself
// loose": the fact that explains a raw transport error killed by the hold
// budget. Both record owners — performSyncPush folding it into an attempt's
// own record, mirrorCycle joining it to a could-not-attempt failure — say it
// through this function, so the durable trail names the budget identically
// wherever the cut landed. [LAW:one-source-of-truth]
func holdBudgetCutExplanation() error {
	return fmt.Errorf(
		"mirror cycle exceeded its %s hold budget (a hung or slow remote transport while holding the store's engine); the engine was released so foreground commands can proceed, and the next mutation's mirror retries the push",
		store.MirrorHoldBudget)
}

// mirrorOnce runs the one shared push path, without compaction. It is a single
// path with no freshness branch: [LAW:dataflow-not-control-flow] the skip
// decisions (no remote, empty remote) already live in performSyncPush, and an
// up-to-date push is a cheap no-op, so the mirror does not pre-decide whether
// to push. It never re-pushes in the same session, either: the engine is the
// path's only writer, so an in-session HEAD re-read can never see a newer
// commit — commits land only between sessions. Coalescing of a burst comes
// from dolt push sending the current HEAD (commits that landed before this
// session's open go out with it) funnelled through the single-flight lock; a
// commit that lands after this session is a fresh mirror-pending claim, and
// the caller's post-release re-check answers it with another whole cycle. The
// unsynced window shrinks toward zero without ever blocking a mutation.
func mirrorOnce(ctx, completionCtx context.Context, session syncSession, ws workspace.Info) error {
	// The mirror pushes without compaction — plain SyncPush, never the
	// compact-and-push variant the explicit command uses.
	outcome, err := performSyncPush(ctx, completionCtx, session, ws, "", false, false, session.syncer.SyncPush)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): performSyncPush's
		// own deferred completion already recorded the outcome. The cycle's
		// single out-of-band automation trace is the caller's to write — it
		// alone knows whether the hold budget is what cut this attempt short.
		// [LAW:single-enforcer]
		return err
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
	return nil
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
