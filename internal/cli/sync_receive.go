package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// receiveTraceCommand and receiveTraceSideEffect label every trace the
// automatic receive leaves, whatever it decided. [LAW:one-source-of-truth]
const (
	receiveTraceCommand    = "lit sync receive"
	receiveTraceSideEffect = "receive Dolt data from the configured git remote"
)

// The receive decisions that are the cli's own rather than a storage receive
// state: the question answered "unmoved" and no fetch ran, or the question
// could not be answered and the fetch ran regardless. Every other decision is
// the storage.SyncReceiveState the fetch reached, or "error".
const (
	receiveDecisionRemoteUnmoved     = "remote_unmoved"
	receiveDecisionRemoteCheckFailed = "remote_check_failed"
)

// backgroundReceiveSubcommand is the hidden `lit sync` subcommand
// scheduleReceive spawns: the automatic receive's detached worker. Like the
// mirror's, it never appears in help.
const backgroundReceiveSubcommand = "__receive-bg"

// ReceiveLogName is the receive worker's durable output sink: one start and
// one end line per receive, each naming the worker's pid — the pid a write
// refused while the receive holds the store names as the holder. Exported so
// the cmd/lit acceptance tests find the worker through the one canonical name.
// [LAW:one-source-of-truth]
const ReceiveLogName = "receive.log"

// receiveBlockPendingName holds the sync-failure block the last automatic
// receive reached and no command has printed yet. The receive runs detached,
// with no terminal of its own, so the block waits here for the next command
// on this checkout to print it (deliverPendingReceiveBlock). One slot: a later
// receive replaces an unprinted block with its own, current one.
const receiveBlockPendingName = "receive-block.pending"

func receiveBlockPendingPath(ws workspace.Info) string {
	return filepath.Join(ws.StorageDir, receiveBlockPendingName)
}

// scheduleReceive starts the automatic receive when one is due, and returns
// without waiting for it: the command's cost is a marker stat, a marker write,
// a local `git remote` read and a process spawn, never a round trip to the
// remote. The remote check, the fetch it may lead to and the reconcile behind
// that all run in the detached worker (backgroundReceiveLeaf) once this
// process has exited. [LAW:effects-at-boundaries] the network is the
// worker's, never the command's.
//
// Debounced so a command burst spawns at most one receive per interval, and
// the debounce marker is written before the spawn so a burst never spawns
// twice. [LAW:single-enforcer] The debounce marker has one writer — this owner.
// A workspace with no git remote has nothing to receive from, so it spawns
// nothing, the same precondition the mirror's spawn checks.
func scheduleReceive(ctx context.Context, ws workspace.Info) {
	if !shouldReceiveNow(ws, time.Now(), receiveDebounceInterval) {
		return
	}
	if err := markReceiveAttempt(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive debounce marker not written: %v\n", err)
	}
	hasRemote, err := workspaceHasGitRemote(ctx, ws)
	if err != nil {
		// Couldn't read remotes — unexpected; surface it loudly rather than treat
		// it as "no remote". [LAW:no-silent-failure]
		recordReceiveError(ws, fmt.Errorf("check git remotes: %w", err))
		return
	}
	if !hasRemote {
		return
	}
	if err := spawnDetachedWorker(ws, backgroundReceiveSubcommand, ReceiveLogName, receiveEnv(), os.Getpid()); err != nil {
		cause := fmt.Errorf("start the background receive: %w", err)
		fmt.Fprintf(os.Stderr, "lit: automatic receive not started: %v\n", err)
		recordReceiveError(ws, cause)
	}
}

// receiveEnv is the receive worker's environment: the parent's, keeping the
// automation trigger the command ran under — the receive's traces are that
// occasion's — and dropping only the trace-ref file (workerEnv).
func receiveEnv() []string {
	return workerEnv([]string{automationTraceRefFileEnvVar})
}

// backgroundReceiveLeaf is the receive's detached worker. It waits, in order,
// for two things before it receives:
//
//   - the spawning command to exit, so the one read-write engine embedded Dolt
//     permits on this path is never the command's and the worker's at once;
//   - every owed or live mirror to finish (awaitNoLiveMirror). A write command
//     spawns its mirror and its receive together, and the mirror records the
//     advertisement its push left on the remote. Asked after that record, the
//     question finds the remote unmoved; asked mid-push, it finds it moved and
//     clones the store to fetch what this checkout just pushed.
//
// It writes its start and end to receive.log, the end line carrying how long
// it waited for a mirror. [LAW:no-ambient-temporal-coupling] the parent's exit,
// the mirror-pending claim and the beacon are the ordering witnesses, not a
// sleep.
func backgroundReceiveLeaf() wsLeaf {
	return detachedWorkerLeaf(backgroundReceiveSubcommand, "PID of the spawning command; the receive waits for it to exit", func(ctx context.Context, stdout io.Writer, ws workspace.Info, parentPID int) error {
		start := time.Now()
		pid := os.Getpid()
		fmt.Fprintf(stdout, "%s receive start pid=%d deadline=%s\n", start.UTC().Format(time.RFC3339), pid, store.ReceiveDeadline)
		mirrorWait := receiveWhenQuiet(ctx, ws, parentPID)
		// The decision is in the receive's sync trace; this line is the worker's
		// own lifetime, which the trace does not carry.
		fmt.Fprintf(stdout, "%s receive end pid=%d mirror_wait=%s elapsed=%s\n", time.Now().UTC().Format(time.RFC3339), pid,
			mirrorWait.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
		return nil
	})
}

// receiveWhenQuiet runs one receive once the spawning command has exited and
// no mirror is live, and returns how long it waited on the mirror. A wait that
// cannot finish says why in the receive's trace: a worker torn down by its own
// context records that, and a parent that outlived the wait records that — two
// causes, never one joined sentence. [LAW:no-silent-failure]
func receiveWhenQuiet(ctx context.Context, ws workspace.Info, parentPID int) time.Duration {
	if !waitForParentExit(ctx, parentPID, os.Getppid, workerParentWaitTimeout, workerParentPollDelay) {
		if ctx.Err() != nil {
			recordReceiveError(ws, fmt.Errorf("receive torn down while waiting for the spawning command to exit: %w", ctx.Err()))
			return 0
		}
		recordReceiveError(ws, fmt.Errorf(
			"spawning command (pid %d) still running after %s; skipping the receive to avoid racing its engine",
			parentPID, workerParentWaitTimeout))
		return 0
	}
	waited, err := awaitNoLiveMirror(ctx, ws, receiveMirrorWait())
	if err != nil {
		if ctx.Err() != nil {
			recordReceiveError(ws, fmt.Errorf("receive torn down while waiting for the mirror: %w", ctx.Err()))
			return waited
		}
		// A mirror that will not finish, or a beacon that cannot be read, costs
		// the ordering, not the receive: say so and receive anyway.
		fmt.Fprintf(os.Stderr, "lit: receiving without waiting for the mirror: %v\n", err)
	}
	receiveOnce(ctx, ws)
	return waited
}

// receiveMirrorWait bounds the receive's wait for live mirrors: two whole
// mirror cycles' network work, which covers the cycle the spawning command
// started and one more it may re-check into. Past it the mirrors are a burst,
// not the command's own push, and the receive goes ahead. The wait costs no
// command anything; it only delays the worker. A function because the push
// deadline the cycle's derives from is the store's variable.
// [LAW:one-source-of-truth]
func receiveMirrorWait() time.Duration { return 2 * mirrorNetworkDeadline() }

// mirrorNetworkDeadline is the longest one mirror cycle's network work runs:
// the push under its deadline, then a landed push's history bound under its
// own. The proof's deadline is not added: the proof runs only after a bound
// that took no re-root to the remote, which reached no network.
// [LAW:one-source-of-truth]
func mirrorNetworkDeadline() time.Duration {
	return store.MirrorPushDeadline + historyBoundDeadline()
}

// mirrorQuietPoll is how often the receive re-reads the mirror liveness
// beacon while it waits. Each read briefly takes the beacon exclusively, and a
// mirror claimant probing at that instant reads it as obstructed and spawns a
// redundant mirror, so the poll is kept coarse; the wait is background time.
const mirrorQuietPoll = 100 * time.Millisecond

// awaitNoLiveMirror waits until no mirror is owed or live and returns how long
// that took. Two witnesses cover a mirror's whole life between them, with no
// gap: the mirror-pending claim a mutating command leaves (mirrorOwed) stands
// from before that command exits until the mirror clears it inside its clone
// hold, and the liveness beacon is held from the mirror's entry to its exit.
// The beacon alone is not enough: a just-spawned mirror may not hold it yet
// when the spawning command has already gone. It fails when bound elapses
// with a mirror still owed or live, when either witness cannot be read, or
// when ctx ends; a claim no mirror will ever clear therefore costs the bound,
// not the receive. A beacon held exclusively by something that is not a
// mirror (BeaconObstructed) answers for no mirror, so it does not hold the
// receive.
func awaitNoLiveMirror(ctx context.Context, ws workspace.Info, bound time.Duration) (time.Duration, error) {
	start := time.Now()
	for {
		// The beacon is probed only once nothing is owed: while a claim stands
		// the answer cannot end the wait, and each probe's brief hold is one
		// more chance to mislead a claimant probing at the same instant.
		live, err := mirrorOwed(ws)
		if err != nil {
			return time.Since(start), err
		}
		if !live {
			verdict, err := store.ProbeMirrorBeacon(ws.DatabasePath)
			if err != nil {
				return time.Since(start), fmt.Errorf("read the mirror liveness beacon: %w", err)
			}
			live = verdict == store.BeaconAnswered
		}
		if !live {
			return time.Since(start), nil
		}
		if waited := time.Since(start); waited >= bound {
			return waited, fmt.Errorf("a mirror is still owed or live after %s", bound)
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(mirrorQuietPoll):
		}
	}
}

// receiveOnce brings the local store up to the remote when the remote has
// moved. It runs in the receive worker, after the spawning command's engine
// is gone, so it is safe to open the one read-write engine embedded Dolt
// permits.
//
// It asks before it fetches: one `git ls-remote` of the remote's refs/dolt/*
// against the advertisement the last successful receive recorded
// (sync_receive_ask.go). The same answer means the remote has not moved and
// the receive ends with no store opened; any other answer — moved, never
// recorded, or unanswerable — runs the fetch and fast-forward. The question is
// asked of the remote the fetch would use, so an unmoved answer is about the
// same store the fetch would have read. [LAW:dataflow-not-control-flow] the
// answer is data; the fetch is the one operation it gates.
//
// It is best-effort and bounded: gated on a configured remote so a
// single-machine repo does no network work, and time-boxed so an offline or
// slow remote cannot keep the worker past store.ReceiveDeadline. A failure is
// recorded as a trace and never reaches a command's exit code.
// [LAW:no-silent-failure]
func receiveOnce(ctx context.Context, ws workspace.Info) {
	// Every receive says whether it owes a push; one that settles nothing
	// owes none. [LAW:nothing-unseen]
	telemetry.Set(ctx, "receive.push_owed", false)
	gitRemotes, err := workspace.GitRemotes(ctx, ws.RootDir)
	if err != nil {
		// Couldn't read remotes — unexpected; surface it loudly rather than treat
		// it as "no remote". [LAW:no-silent-failure]
		recordReceiveError(ws, fmt.Errorf("check git remotes: %w", err))
		return
	}
	if len(gitRemotes) == 0 {
		return
	}

	// One deadline spans the question and the fetch it may lead to, so a remote
	// that hangs costs the receive one deadline, never twice that. The fetch
	// runs on a clone, so the deadline bounds the worker and nothing else
	// waits on it; the live store is held only for the clone's copy, the
	// landing and the settle. [LAW:no-ambient-temporal-coupling]
	timeoutCtx, cancel := context.WithTimeout(ctx, store.ReceiveDeadline)
	defer cancel()

	observed, askErr := askRemote(timeoutCtx, ws, gitRemotes)
	// A remote lit cannot sync with is known before the remote is asked, and
	// the clone's own sync would refuse it the same way, so the receive fails
	// here, before the lock and the clone, rather than trace "could not tell"
	// and fetch. [LAW:no-silent-failure]
	if refused := (storage.RefusedRemoteNameError{}); errors.As(askErr, &refused) {
		recordReceiveError(ws, askErr)
		return
	}
	if askErr != nil {
		// "Could not tell" is not "nothing changed": say so, then fetch.
		// [LAW:no-silent-failure]
		if err := recordReceiveTrace(ws, receiveDecisionRemoteCheckFailed, "error", askErr.Error(),
			map[string]string{"error": askErr.Error()}); err != nil {
			fmt.Fprintf(os.Stderr, "lit: automatic receive trace not recorded: %v\n", err)
		}
	} else if observed.unmoved(readReceivedRefs(ws)) {
		confirmRemoteUnmoved(ws, observed)
		return
	} else if observed.nothingToReceive() {
		return
	}

	release, acquired, err := store.TryAcquireReceiveLock(ws.DatabasePath)
	if err != nil {
		recordReceiveError(ws, fmt.Errorf("take the receive lock: %w", err))
		return
	}
	if !acquired {
		// Another receive worker is running; it fetches what this one
		// would have.
		return
	}
	defer func() {
		if err := release(); err != nil {
			fmt.Fprintf(os.Stderr, "lit: automatic receive lock not released: %v\n", err)
		}
	}()
	outcome, err := receiveAndRecord(timeoutCtx, ws, observed)
	if err != nil {
		// Could-not-attempt (the clone, or the clone's own target): record and stop.
		recordReceiveError(ws, err)
		return
	}
	// The store is closed by now: surfacing can run the owner-notify hook,
	// and nothing waiting on the store should wait on that too.
	surfaceReceiveOutcome(ctx, ws, outcome, time.Now())
	// What the settle left unpushed is owed the push a write's commit is: a
	// refusal over a peer's commits is recorded diverged on the promise that
	// this merge reaches the remote. The mirror waits for this worker to exit.
	access := outcome.access()
	telemetry.Set(ctx, "receive.push_owed", access == app.AccessWrite)
	mirrorAfterReceive(ctx, access, ws)
}

// receiveAndRecord is the receive proper, run under the receive lock: clone
// the live store, fetch into the clone, land the fetch on the live store, and
// settle there (fast-forward, or reconcile a divergence), then record what that
// established. The live store is held only for the clone's copy, the landing,
// and the settle — none of them on the network. The error
// is a could-not-attempt failure; a receive that ran and failed is in the
// outcome, its trace already written.
func receiveAndRecord(ctx context.Context, ws workspace.Info, observed remoteAdvertisement) (syncReceiveOutcome, error) {
	clone, err := takeReceiveClone(ctx, ws)
	if err != nil {
		return syncReceiveOutcome{}, err
	}
	defer removeReceiveClone(clone)
	fetch, err := fetchIntoClone(ctx, ws, clone)
	if err != nil {
		return syncReceiveOutcome{}, err
	}
	return landAndRecord(ctx, ws, clone, fetch, observed), nil
}

// landAndRecord is the receive after its fetch: land the fetch on the live
// store, settle there, and record what that established. Every step from
// here touches the live store; nothing before it did.
func landAndRecord(ctx context.Context, ws workspace.Info, clone liveClone, fetch receiveFetch, observed remoteAdvertisement) syncReceiveOutcome {
	outcome, closeLive := performSyncReceive(ctx, ws, clone, fetch, ownUnsettledRun(ws.DatabasePath))
	// The live store stays open through the record below, so its workspace
	// hold keeps a rotation of the Dolt directory from landing between the
	// settle and the record that describes it.
	defer func() {
		if err := closeLive(); err != nil {
			fmt.Fprintf(os.Stderr, "lit: automatic receive store not closed cleanly: %v\n", err)
		}
	}()
	// performSyncReceive records its own trace; surface a trace-write failure
	// rather than drop it. [LAW:no-silent-failure]
	if outcome.traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive trace not recorded: %v\n", outcome.traceErr)
	}
	// What the receive established is recorded here, after the receive and its
	// reconcile, by the one owner that also asked: a fetch that landed moves
	// the fetch-success marker, and a receive that settled cleanly makes the
	// advertisement observed before the fetch the record the next question is
	// measured against (sync_receive_ask.go).
	recordReceived(ws, outcome, observed)
	return outcome
}

// surfaceReceiveOutcome leaves a non-converging reconcile — a held free-text
// conflict or a hard backend failure — as the pending block the next command
// prints, through the one sync-failure contract, and retires any pending block
// once a receive settles cleanly: a block an earlier receive left is no longer
// true then. It keeps receiveOnce a pure orchestrator: the decision of WHETHER
// to surface lives in the outcome's data (inlineSyncFailure), and the surfacing
// itself lives here, behind a named boundary. [LAW:decomposition]
// [LAW:no-silent-failure] [LAW:single-enforcer] one contract, whether the
// failure flows out as a returned error or waits here for a command to print.
//
// The receive runs every interval on every checkout, which makes this seam the
// owner channel's workhorse (links-sync-pgct.4): a surfaced divergence notifies
// the owner out-of-band (de-duplicated per episode), and a receive that settled
// cleanly against the remote ends the divergence episode. The hook is passed the
// worker's ctx, not the receive's timeoutCtx: the receive fetch already
// completed, and its remaining budget must not shorten the notifier's own.
// [LAW:no-ambient-temporal-coupling]
func surfaceReceiveOutcome(ctx context.Context, ws workspace.Info, outcome syncReceiveOutcome, now time.Time) {
	if failure, ok := outcome.inlineSyncFailure(now); ok {
		block := failure.blockString()
		// The worker's own log keeps every block it reached; the pending file
		// is the copy a command prints.
		fmt.Fprintln(os.Stderr, block)
		if err := writeMarkerAtomic(receiveBlockPendingPath(ws), []byte(block+"\n")); err != nil {
			fmt.Fprintf(os.Stderr, "lit: sync-failure block not left for the next command: %v\n", err)
		}
		if ev, evOK := ownerNotifyEventForFailure(failure); evOK {
			maybeNotifyOwner(ctx, ws, ev)
		}
		return
	}
	if outcome.settledCleanly() {
		endDivergenceEpisode(ws)
	}
}

// receiveBlockProvenance is the line printed above a delivered block: when the
// receive reached it. The block was rendered then, and any age or severity in
// it is as of then; a command that runs hours later must not present it as
// current. [FRAMING:representation] The file's modification time is the
// receive's write, the one clock the block carries.
func receiveBlockProvenance(path string, now time.Time) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("lit: the automatic receive left this sync-failure block (when is unknown: %v)\n", err)
	}
	return fmt.Sprintf("lit: the automatic receive reached this sync-failure block %s ago (%s); anything it dates is as of then\n",
		humanizeCoarseDuration(now.Sub(info.ModTime())), info.ModTime().UTC().Format(time.RFC3339))
}

// endDivergenceEpisode is what every surface does when a divergence has
// converged — this receive, `lit sync pull`, `lit sync reconcile` and its
// take and combine: the owner-notify markers for divergence reset, so the next
// divergence is a new episode, and a sync-failure block still waiting for a
// command is retired, since the divergence it describes is gone.
// [LAW:single-enforcer] one ending, whichever surface converged.
func endDivergenceEpisode(ws workspace.Info) {
	clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
	if err := os.Remove(receiveBlockPendingPath(ws)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "lit: stale sync-failure block not retired: %v\n", err)
	}
}

// deliverPendingReceiveBlock prints, to w, the sync-failure block the last
// automatic receive left, and removes it, so each block a receive reaches is
// printed by exactly one command — the next one to finish on this checkout,
// as the receive once printed it on the command that ran it. The block
// is claimed by renaming it aside first: a receive that leaves a newer block
// between this read and the removal keeps it for the command after.
// [LAW:no-silent-failure] a block that cannot be claimed or read says so.
func deliverPendingReceiveBlock(w io.Writer, ws workspace.Info) {
	pending := receiveBlockPendingPath(ws)
	claimed := fmt.Sprintf("%s.%d", pending, os.Getpid())
	if err := os.Rename(pending, claimed); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "lit: pending sync-failure block not claimed: %v\n", err)
		}
		return
	}
	block, err := os.ReadFile(claimed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lit: pending sync-failure block unreadable: %v\n", err)
	} else if _, err := w.Write(append([]byte(receiveBlockProvenance(claimed, time.Now())), block...)); err != nil {
		fmt.Fprintf(os.Stderr, "lit: pending sync-failure block not printed: %v\n", err)
	}
	if err := os.Remove(claimed); err != nil {
		fmt.Fprintf(os.Stderr, "lit: printed sync-failure block not removed: %v\n", err)
	}
}

// ranThrough reports whether the receive ran and did not stop at or before
// step. [LAW:one-source-of-truth] every reading of how far a receive got is a
// comparison against the one step it stopped at.
func (o syncReceiveOutcome) ranThrough(step receiveStep) bool {
	return o.skip == syncTargetReady && (o.stopped == receiveStepNone || o.stopped > step)
}

// fetched reports whether the receive's DOLT_FETCH returned without error —
// true for every post-fetch state, including a receive that then stopped at
// the landing, the open, the live remotes or the settle; false for a skipped
// target or a failed fetch. It is what the fetch-success marker is
// conditioned on, so a fetch that worked is never blamed by the staleness
// banner for a step after it.
func (o syncReceiveOutcome) fetched() bool {
	return o.ranThrough(receiveStepFetch)
}

// settledCleanly reports whether the receive ran to the end and left no
// unresolved divergence — the condition that ends a divergence episode. A
// skipped receive (no remote, empty remote) and a stopped one carry no such
// information and leave the episode standing. [LAW:dataflow-not-control-flow]
// the outcome's values decide; the caller runs unconditionally.
func (o syncReceiveOutcome) settledCleanly() bool {
	if !o.ranThrough(receiveStepReconcile) {
		return false
	}
	if o.reconcile == nil {
		return true
	}
	return o.reconcile.state == storage.SyncReconcileLinearized || o.reconcile.state == storage.SyncReconcileNotDiverged
}

// access is what the receive did to the branch as the cadence owner reads a
// command: AccessWrite when its settle left commits the remote lacks — a
// re-root follow that replayed local work onto the remote head (ahead), or a
// reconcile that linearized a divergence onto it — and AccessRead otherwise.
// [LAW:types-are-the-program] read off the settle's closed state sets: a
// reconcile reports linearized only once the data branch has advanced (the
// store returns the zero result with any error), and its own counts are taken
// before its replay.
func (o syncReceiveOutcome) access() app.AccessMode {
	if !o.ranThrough(receiveStepSettle) {
		return app.AccessRead
	}
	if o.state == storage.SyncReceiveAhead ||
		(o.reconcile != nil && o.reconcile.state == storage.SyncReconcileLinearized) {
		return app.AccessWrite
	}
	return app.AccessRead
}

// inlineSyncFailure derives the sync-failure contract for a receive that
// could not converge, or reports ok=false when there is nothing to surface.
// Every surfaced outcome routes through the one contract, with the error
// carried as the block's trailing cause rather than as an ignorable headline:
//   - a cause that is the remote's schema running ahead of this binary, at
//     any step, is its own class — it never clears by retrying;
//   - a stop at the reconcile is a divergence the reconcile could not resolve;
//   - a run of post-fetch stops that has lasted (unsettledLasting) is an
//     unsettled receive;
//   - a reconcile that held (prose, an id collision, unrelated histories) is
//     its own class;
//   - anything else, a single post-fetch stop included, surfaces nothing: the
//     trace and the event carry it.
//
// The ages are computed here, at the boundary that holds the clock, from the
// timestamps the receive recorded. [LAW:dataflow-not-control-flow]
func (o syncReceiveOutcome) inlineSyncFailure(now time.Time) (SyncFailure, bool) {
	// [LAW:single-enforcer] one adapter, every surface, every step.
	if failure, ok := remoteSchemaAheadFailure(o.err); ok {
		return failure, true
	}
	base := SyncFailure{
		Remote:    o.remote.String(),
		Branch:    o.branch,
		Ahead:     o.ahead,
		Behind:    o.behind,
		Age:       ageFromOldestDivergedUnix(o.oldestDivergedUnix, now),
		BuildNote: resolveBuildStatusNote(now),
	}
	switch {
	case o.stopped == receiveStepReconcile:
		base.Class = syncFailureDivergedUnresolved
		base.Cause = o.err
		return base, true
	case o.unsettledLasting(now):
		base.Class = syncFailureReceiveUnsettled
		base.Cause = o.err
		base.StoppedAt = o.stopped
		base.UnsettledSince = o.unsettledSince
		base.Age = now.Sub(o.unsettledSince)
		return base, true
	case o.stopped != receiveStepNone || o.reconcile == nil:
		return SyncFailure{}, false
	}
	switch o.reconcile.state {
	case storage.SyncReconcileProsePending:
		base.Class = syncFailureProseHeld
		base.Fields = o.reconcile.pending
		return base, true
	case storage.SyncReconcileIDCollision:
		base.Class = syncFailureIDCollision
		base.Collisions = o.reconcile.collisions
		return base, true
	case storage.SyncReconcileUnrelated:
		// A no-common-ancestor divergence is non-transient like a held prose conflict —
		// it will NOT clear on the next receive — so it routes through the same contract
		// rather than falling to the ok=false "settled cleanly" default, which would
		// silently report the auto-sync as fine while the store stayed unmergeably
		// diverged. [LAW:no-silent-failure]
		base.Class = syncFailureUnrelatedHistories
		base.Inventory = o.reconcile.unrelated
		return base, true
	default:
		return SyncFailure{}, false
	}
}

// syncReceiveOutcome is the result of one receive attempt, independent of how it
// was triggered. [LAW:decomposition] Resolving remotes, fetching, and fast-
// forwarding are one part; the inline scheduling that invokes it is another.
type syncReceiveOutcome struct {
	// skip is the typed no-op discriminator shared with push and pull:
	// syncTargetReady means the receive ran; a non-empty skip names why it did
	// not. [LAW:types-are-the-program]
	skip               syncTargetSkip
	remote             storage.RemoteName
	branch             string
	state              storage.SyncReceiveState
	ahead              int64
	behind             int64
	oldestDivergedUnix int64 // Unix time the divergence began; 0 unless diverged
	traceErr           error
	// stopped is the step the receive stopped at, receiveStepNone when it ran
	// to the end, and err is that step's failure; the trace is already
	// recorded when it is set. [LAW:one-source-of-truth] the one record of
	// where a receive stopped, the reconcile included.
	stopped receiveStep
	err     error
	// unsettledSince is when the current run of post-fetch stops began, zero
	// when no run is current; unsettledPreviousStop is the latest stop in that
	// run before this receive's, zero when none.
	unsettledSince        time.Time
	unsettledPreviousStop time.Time
	// reconcile carries the inline reconcile that runs when the receive found a
	// divergence the fast-forward could not absorb. Zero-valued unless state was
	// SyncReceiveDiverged. [LAW:decomposition] Receiving and reconciling are
	// distinct steps; the receive owns fetch+ff, the reconcile owns the field-
	// aware three-way merge.
	reconcile *reconcileOutcome
}

// reconcileOutcome is the inline reconcile result the diverged receive triggers.
type reconcileOutcome struct {
	state      storage.SyncReconcileState
	pending    []merge.ProsePending
	unrelated  *storage.UnrelatedInventory // the both-sides partition; set only for SyncReconcileUnrelated
	collisions []merge.Collision           // both rows of each colliding id; set only for SyncReconcileIDCollision
}

// performSyncReceive takes the fetch that ran on the clone to the live store:
// land it (store.LandFetchedHead), then open the live store and settle —
// fast-forward when the local branch is strictly behind — and reconcile a
// divergence, recording an automation trace for the attempt. A receive that
// ran and stopped — at the fetch, the landing, the open, the live remotes,
// the settle or the reconcile — says where in outcome.stopped, with that
// step's error in outcome.err and its trace already recorded, leaving local
// data untouched. The sync target is the one the fetch resolved on the clone
// through the shared prologue, so push, pull, receive and reconcile never
// disagree about it. [LAW:single-enforcer]
//
// observeRun is the receive's part in the run of post-fetch stops
// (sync_receive_run.go); it runs on every receive that reached its target.
//
// closeLive closes the live store when the settle opened it and is a no-op
// otherwise; it is the caller's to run once it has recorded the outcome.
func performSyncReceive(ctx context.Context, ws workspace.Info, clone liveClone, fetch receiveFetch, observeRun observeUnsettledRun) (outcome syncReceiveOutcome, closeLive func() error) {
	closeLive = func() error { return nil }
	// A receive that skips its target settles nothing, and its
	// receive.outcomes reads [] rather than absent. [LAW:nothing-unseen]
	telemetry.Append(ctx, "receive.outcomes")
	target := fetch.target
	if target.skip != syncTargetReady {
		return syncReceiveOutcome{skip: target.skip, remote: target.remote}, closeLive
	}
	remoteName, syncBranch := target.remote, target.branch

	// Each step runs while no step before it stopped, and the first that
	// fails is where the receive stopped.
	stopped, stepErr := receiveStepNone, error(nil)
	stopAt := func(step receiveStep, err error) {
		if err != nil && stopped == receiveStepNone {
			stopped, stepErr = step, err
		}
	}
	stopAt(receiveStepFetch, fetch.fetchErr)
	var landed store.LandedFetch
	if stopped == receiveStepNone {
		var err error
		landed, err = landFetch(ctx, ws, clone, target)
		stopAt(receiveStepLand, err)
	}
	var session syncSession
	if stopped == receiveStepNone {
		var close func() error
		var err error
		session, close, err = openSyncSession(ctx, ws)
		if err != nil {
			err = fmt.Errorf("open sync store: %w", err)
		}
		stopAt(receiveStepOpen, err)
		if err == nil {
			closeLive = close
		}
	}
	if stopped == receiveStepNone {
		// The clone's copy of the remotes was reconciled from git when the
		// target was resolved there; the live store's is reconciled here, so
		// a re-pointed git remote reaches it without an explicit sync command.
		// Local only: git's remote config and the store's remote table.
		if _, err := syncDoltRemotesFromGit(ctx, session, ws); err != nil {
			stopAt(receiveStepLiveRemotes, fmt.Errorf("reconcile the live store's remotes from git: %w", err))
		}
	}
	var result storage.SyncReceiveResult
	if stopped == receiveStepNone {
		var err error
		result, err = session.syncer.SyncSettleReceived(ctx, remoteName, syncBranch)
		stopAt(receiveStepSettle, err)
	}
	outcome = syncReceiveOutcome{
		remote:             remoteName,
		branch:             syncBranch,
		state:              result.State,
		ahead:              result.Ahead,
		behind:             result.Behind,
		oldestDivergedUnix: result.OldestDivergedUnix,
		stopped:            stopped,
		err:                stepErr,
	}
	run := observeRun(outcome, time.Now())
	outcome.unsettledSince = run.since
	outcome.unsettledPreviousStop = run.previousStop
	if run.err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive's unsettled-run records not read or written: %v\n", run.err)
	}
	traceMetadata := map[string]string{
		"remote":                  remoteName.String(),
		"sync_branch":             syncBranch,
		"state":                   string(result.State),
		"ahead":                   strconv.FormatInt(result.Ahead, 10),
		"behind":                  strconv.FormatInt(result.Behind, 10),
		"base":                    string(result.Base),
		"landed":                  landed.Record.String(),
		"fork_point":              landed.ForkPoint.String(),
		"land_held":               landed.Held.Round(time.Millisecond).String(),
		"stopped":                 stopped.String(),
		"unsettled_since":         strconv.FormatInt(unixOrZero(run.since), 10),
		"unsettled_previous_stop": strconv.FormatInt(unixOrZero(run.previousStop), 10),
		"settled_last":            strconv.FormatInt(unixOrZero(run.settledLast), 10),
	}
	if run.err != nil {
		traceMetadata["unsettled_record_error"] = run.err.Error()
	}
	traceStatus := "ok"
	traceReason := receiveReasonForState(result.State)
	receiveDecision := string(result.State)
	if stepErr != nil {
		traceStatus = "error"
		traceReason = stepErr.Error()
		traceMetadata["error"] = stepErr.Error()
		receiveDecision = "error"
	}
	outcome.traceErr = recordReceiveTrace(ws, receiveDecision, traceStatus, traceReason, traceMetadata)
	// The reconcile runs INLINE on this same engine because embedded Dolt permits
	// only one read-write engine per path — a worker would collide with the next
	// foreground command. [LAW:no-ambient-temporal-coupling]
	if stopped == receiveStepNone && result.State == storage.SyncReceiveDiverged {
		reconciled, err := performInlineReconcile(ctx, session, ws, remoteName, syncBranch)
		outcome.reconcile = reconciled
		if err != nil {
			outcome.stopped, outcome.err = receiveStepReconcile, err
		}
	}
	// The settle's decision lands on the invocation's event too, one value per
	// receive, with where the receive stopped — the reconcile included — and the
	// run's two times. [LAW:nothing-unseen]
	telemetry.Append(ctx, "receive.outcomes", map[string]any{
		"receive.decision":                receiveDecision,
		"receive.ahead":                   result.Ahead,
		"receive.behind":                  result.Behind,
		"receive.base":                    traceMetadata["base"],
		"receive.stopped":                 outcome.stopped.String(),
		"receive.unsettled_since":         unixOrZero(run.since),
		"receive.unsettled_previous_stop": unixOrZero(run.previousStop),
		"receive.settled_last":            unixOrZero(run.settledLast),
	})
	return outcome, closeLive
}

// reconcileOnce resolves the reconcile capability and runs it.
//
// The receive that reached here has already fast-forwarded everything it could
// and found a real divergence. An engine that offers sync but declines
// reconcile therefore has nothing left to try, and the decline arrives at the
// caller as an ordinary reconcile error — traced, surfaced, and never quietly
// recorded as a settled divergence. [LAW:no-silent-failure]
//
// It is a unit of its own so that performInlineReconcile keeps one unbranched
// path: the capability question is answered here and never re-asked below.
// [LAW:parse-dont-validate]
func reconcileOnce(ctx context.Context, session syncSession, remote storage.RemoteName, branch string) (storage.SyncReconcileResult, error) {
	reconciler, err := storage.Reconcile.Of(session.engine)
	if err != nil {
		return storage.SyncReconcileResult{}, err
	}
	return reconciler.SyncReconcile(ctx, remote, branch)
}

// performInlineReconcile runs the field-aware reconcile on a diverged clone and
// records its own automation trace. A settled divergence converges to linear
// history transparently (like a fast-forward); a prose divergence is held as
// prose-pending for the agent surface, never auto-committed by picking a side.
// [LAW:single-enforcer] One reconcile entrypoint and one trace writer, whether
// the receive was inline or foreground.
func performInlineReconcile(ctx context.Context, session syncSession, ws workspace.Info, remote storage.RemoteName, branch string) (*reconcileOutcome, error) {
	result, reconcileErr := reconcileOnce(ctx, session, remote, branch)
	// "replayed" matches the explicit reconcile/take reporters: the durable
	// trace carries the provenance-replay count for every outcome, zero
	// included, so the trail can distinguish a granular fold from a no-op.
	traceMetadata := map[string]string{
		"remote":      remote.String(),
		"sync_branch": branch,
		"state":       string(result.State),
		"replayed":    strconv.Itoa(result.Replayed),
	}
	landingMetadata(traceMetadata, result.Landing)
	// The receive settles silently on success, so its moves go to the worker's
	// own log as well as the trace: a ticket moved out of sight is still named.
	if err := writeLanding(os.Stderr, "the merged backlog", result.Landing); err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic reconcile moves not reported: %v\n", err)
	}
	traceStatus := "ok"
	traceReason := reconcileReasonForState(result.State)
	if reconcileErr != nil {
		traceStatus = "error"
		traceReason = reconcileErr.Error()
		traceMetadata["error"] = reconcileErr.Error()
	} else if result.State == storage.SyncReconcileProsePending {
		traceMetadata["pending"] = strconv.Itoa(len(result.Pending))
	} else if result.State == storage.SyncReconcileIDCollision {
		traceMetadata["collisions"] = strconv.Itoa(len(result.Collisions))
	}
	if _, traceErr := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync reconcile",
		"reconcile a diverged clone into linear history with the field-aware merge engine",
		traceStatus,
		traceReason,
		traceMetadata,
	); traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic reconcile trace not recorded: %v\n", traceErr)
	}
	// The durable, unconditional counterpart — same reasoning as performSyncReceive's:
	// this reconcile is reached from the automatic receive, which commonly runs with no
	// automation trigger set, so without this call it would otherwise leave no
	// durable record of the exact decision (linearized / prose-pending / unrelated).
	reconcileDecision := string(result.State)
	if reconcileErr != nil {
		reconcileDecision = "error"
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   "lit sync reconcile",
		Decision:  reconcileDecision,
		Status:    traceStatus,
		Reason:    traceReason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  traceMetadata,
	})
	// The trace above records the attempt out-of-band; the caller (receiveOnce)
	// surfaces a non-converging reconcile — hard failure OR held free-text —
	// through the one sync-failure contract, so both classes read as the
	// unmissable block rather than a raw "will retry" line. This function stays the
	// run-and-record step; the surfacing decision lives with the caller that holds
	// the divergence's counts and age. [LAW:decomposition]
	return &reconcileOutcome{
		state:      result.State,
		pending:    result.Pending,
		unrelated:  result.Unrelated,
		collisions: result.Collisions,
	}, reconcileErr
}

// reconcileReasonForState maps a reconcile outcome to its automation-trace
// reason. [LAW:one-source-of-truth] One mapping over the closed state set.
func reconcileReasonForState(state storage.SyncReconcileState) string {
	switch state {
	case storage.SyncReconcileLinearized:
		return "automatic reconcile merged the divergence into linear history"
	case storage.SyncReconcileProsePending:
		return "automatic reconcile resolved every field but free-text diverged on both sides; held for the agent surface"
	case storage.SyncReconcileUnrelated:
		return "automatic reconcile found unrelated histories (no common ancestor); held for wholesale/union resolution"
	case storage.SyncReconcileIDCollision:
		return "automatic reconcile refused the merge: an id names a different ticket on each side; held for the operator to re-file one"
	case storage.SyncReconcileNotDiverged:
		return "automatic reconcile found the branch no longer diverged; nothing to do"
	default:
		return "automatic reconcile completed with state " + string(state)
	}
}

// receiveReasonForState maps a receive outcome to the human reason recorded on
// its automation trace, so the trace describes what actually happened rather than
// assuming a fast-forward. [LAW:one-source-of-truth] One mapping from state to
// reason; an exhaustive switch over the closed SyncReceiveState set.
func receiveReasonForState(state storage.SyncReceiveState) string {
	switch state {
	case storage.SyncReceiveFastForwarded:
		return "automatic receive landed the local store on the remote head"
	case storage.SyncReceiveUpToDate:
		return "automatic receive found the local store already up to date with the remote"
	case storage.SyncReceiveAhead:
		return "automatic receive found local ahead of the remote; nothing to receive"
	case storage.SyncReceiveDiverged:
		return "automatic receive found local diverged from the remote; left for foreground reconcile"
	case storage.SyncReceiveNeverSynced:
		return "automatic receive found no remote-tracking data on this branch yet"
	default:
		return "automatic receive completed with state " + string(state)
	}
}

// recordReceiveTrace writes the two traces every automatic-receive decision
// leaves: the LNKS_AUTOMATION_TRIGGER-gated automation trace, and the durable
// sync trace an interactive command's receive would otherwise never leave —
// maybeAutoSyncAfterCommand sets no trigger. The automation trace's write
// error is returned for the caller to surface alongside its outcome (the
// receive has no reader for a trace ref, unlike the pre-push hook); the sync
// trace reports its own. [LAW:single-enforcer] one writer, whatever the
// receive decided.
func recordReceiveTrace(ws workspace.Info, decision, status, reason string, metadata map[string]string) error {
	_, traceRecordErr := maybeRecordAutomatedCommandTrace(ws, receiveTraceCommand, receiveTraceSideEffect, status, reason, metadata)
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   receiveTraceCommand,
		Decision:  decision,
		Status:    status,
		Reason:    reason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  metadata,
	})
	return traceRecordErr
}

// recordReceiveError writes a could-not-attempt failure to both traces so an
// automatic receive that fails is loud out-of-band rather than silent.
// [LAW:no-silent-failure] A trace-write failure is not swallowed — it goes to
// stderr with the original cause beside it.
func recordReceiveError(ws workspace.Info, cause error) {
	if traceErr := recordReceiveTrace(ws, "error", "error", cause.Error(),
		map[string]string{"error": cause.Error()}); traceErr != nil {
		fmt.Fprintf(os.Stderr,
			"lit: automatic receive could not record failure trace (%v); original error: %v\n",
			traceErr, cause)
	}
}
