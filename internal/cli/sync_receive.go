package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
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

// receiveInline brings the local store up to the remote when the remote has
// moved, INLINE in the command process. The caller (maybeAutoSyncAfterCommand)
// invokes it only after the command's own engine has closed and only when
// receive is enabled, so it is safe to open the one read-write engine embedded
// Dolt permits.
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
// It is best-effort and bounded: debounced so a command burst asks at most
// once per interval, gated on a configured remote so a single-machine repo
// does no work, and time-boxed so an offline/slow remote cannot hang the
// command. A failure is recorded as a trace, not printed to the command's
// stdout (already produced) and never fails the command. [LAW:no-silent-failure]
func receiveInline(ctx context.Context, ws workspace.Info) {
	if !shouldReceiveNow(ws, time.Now(), receiveDebounceInterval) {
		return
	}
	// Debounce before the remote check and fetch so a command burst pays at most
	// one of each per interval, even when there is no remote. [LAW:single-enforcer]
	// The debounce marker has one writer — this owner.
	if err := markReceiveAttempt(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive debounce marker not written: %v\n", err)
	}
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
	// that hangs costs the command one deadline, never twice that. The fetch
	// runs on a clone, so the deadline bounds the command's wait and nothing
	// else waits on it; the live store is held only for the clone's copy, the
	// landing and the settle.
	// [LAW:no-ambient-temporal-coupling]
	timeoutCtx, cancel := context.WithTimeout(ctx, store.InlineReceiveDeadline)
	defer cancel()

	observed, askErr := askRemote(timeoutCtx, ws, gitRemotes)
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
	}

	release, acquired, err := store.TryAcquireReceiveLock(ws.DatabasePath)
	if err != nil {
		recordReceiveError(ws, fmt.Errorf("take the receive lock: %w", err))
		return
	}
	if !acquired {
		// Another command's receive is running; it fetches what this one
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
		// Could-not-attempt (the clone, the target, the live open): record and stop.
		recordReceiveError(ws, err)
		return
	}
	surfaceInlineOutcome(ctx, ws, outcome, time.Now())
}

// receiveAndRecord is the receive proper, run under the receive lock: clone
// the live store, fetch into the clone, land the fetch on the live store, and
// settle there (fast-forward, or reconcile a divergence), then record what that
// established. The live store is held only for the clone's copy, the landing,
// and the settle — none of them on the network (links-scale-t4vj). The error
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
	outcome, closeLive, err := performSyncReceive(ctx, ws, clone, fetch)
	if err != nil {
		return syncReceiveOutcome{}, err
	}
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
	return outcome, nil
}

// surfaceInlineOutcome renders a non-converging inline reconcile — a held free-text
// conflict or a hard backend failure — to stderr through the one sync-failure
// contract, and does nothing when the receive/reconcile settled cleanly. It keeps
// receiveInline a pure orchestrator: the decision of WHETHER to surface lives in
// the outcome's data (inlineSyncFailure), and the surfacing itself lives here,
// behind a named boundary. The command's stdout is already produced and its local
// reads still serve, so this neither corrupts output nor fails the command — but it
// cannot read as an ignorable line. [LAW:decomposition] [LAW:no-silent-failure]
// [LAW:single-enforcer] one contract, whether the failure flows out as a returned
// error or is printed here.
//
// The inline receive runs after nearly every command, which makes this seam the
// owner channel's workhorse (links-sync-pgct.4): a surfaced divergence notifies
// the owner out-of-band (de-duplicated per episode), and a receive that settled
// cleanly against the remote ends the divergence episode. The hook is passed the
// command's ctx, not the receive's 15s timeoutCtx: the receive fetch already
// completed, and its remaining budget must not shorten the notifier's own.
// [LAW:no-ambient-temporal-coupling]
func surfaceInlineOutcome(ctx context.Context, ws workspace.Info, outcome syncReceiveOutcome, now time.Time) {
	if failure, ok := outcome.inlineSyncFailure(now); ok {
		fmt.Fprintln(os.Stderr, failure.blockString())
		if ev, evOK := ownerNotifyEventForFailure(failure); evOK {
			maybeNotifyOwner(ctx, ws, ev)
		}
		return
	}
	if outcome.settledCleanly() {
		clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
	}
}

// fetched reports whether the receive's DOLT_FETCH returned without error —
// true for every post-fetch state (up to date, fast-forwarded, ahead, diverged,
// never synced), false for a skipped target or a failed fetch. It is what the
// fetch-success marker is conditioned on, and the first condition of
// settledCleanly, which the received-refs record is conditioned on.
// [LAW:one-source-of-truth] one reading of "the fetch happened".
func (o syncReceiveOutcome) fetched() bool {
	return o.skip == syncTargetReady && o.receiveErr == nil
}

// settledCleanly reports whether the receive both contacted the remote and left
// no unresolved divergence — the condition that ends a divergence episode. A
// skipped receive (no remote, empty remote) and a failed one carry no such
// information and leave the episode standing. [LAW:dataflow-not-control-flow]
// the outcome's values decide; the caller runs unconditionally.
func (o syncReceiveOutcome) settledCleanly() bool {
	if !o.fetched() {
		return false
	}
	if o.reconcile == nil {
		return true
	}
	return o.reconcile.err == nil &&
		(o.reconcile.state == storage.SyncReconcileLinearized || o.reconcile.state == storage.SyncReconcileNotDiverged)
}

// inlineSyncFailure derives the sync-failure contract for an inline reconcile
// that could not converge, or reports ok=false when the receive/reconcile settled
// cleanly. Every non-converging outcome routes through the one contract, with the
// reconcile error carried as the block's trailing cause rather than as an
// ignorable headline. The divergence age is computed here, at the boundary that
// holds the clock, from the timestamp the receive recorded. [LAW:dataflow-not-control-flow]
func (o syncReceiveOutcome) inlineSyncFailure(now time.Time) (SyncFailure, bool) {
	if o.reconcile == nil {
		return SyncFailure{}, false
	}
	base := SyncFailure{
		Remote:    o.remote,
		Branch:    o.branch,
		Ahead:     o.ahead,
		Behind:    o.behind,
		Age:       ageFromOldestDivergedUnix(o.oldestDivergedUnix, now),
		BuildNote: resolveBuildStatusNote(now),
	}
	switch {
	case o.reconcile.err != nil:
		// A remote schema ahead of this binary is its own class — it will NOT clear
		// by retrying, so it must not read as a generic "diverged, will reconcile"
		// cause. Route it to the remote-schema-ahead contract naming `lit upgrade`.
		// [LAW:single-enforcer] one adapter, every surface.
		if failure, ok := remoteSchemaAheadFailure(o.reconcile.err); ok {
			return failure, true
		}
		base.Class = syncFailureDivergedUnresolved
		base.Cause = o.reconcile.err
		return base, true
	case o.reconcile.state == storage.SyncReconcileProsePending:
		base.Class = syncFailureProseHeld
		base.Fields = o.reconcile.pending
		return base, true
	case o.reconcile.state == storage.SyncReconcileIDCollision:
		base.Class = syncFailureIDCollision
		base.Collisions = o.reconcile.collisions
		return base, true
	case o.reconcile.state == storage.SyncReconcileUnrelated:
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
	remote             string
	branch             string
	state              storage.SyncReceiveState
	ahead              int64
	behind             int64
	oldestDivergedUnix int64 // Unix time the divergence began; 0 unless diverged
	traceErr           error
	receiveErr         error // the receive failure; the trace is already recorded when set
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
	err        error                       // the reconcile failure; its trace is already recorded when set
}

// performSyncReceive takes the fetch that ran on the clone to the live store:
// land it (store.LandFetchedHead), then open the live store and settle —
// fast-forward when the local branch is strictly behind — recording an
// automation trace for the attempt. The returned error is a "could not
// attempt" failure (the live open); a receive that ran and failed — the fetch,
// the landing, or the settle — is carried in outcome.receiveErr with its trace
// already recorded, leaving local data untouched. The sync target is the one
// the fetch resolved on the clone through the shared prologue, so push, pull,
// receive and reconcile never disagree about it. [LAW:single-enforcer]
//
// closeLive closes the live store when the settle opened it and is a no-op
// otherwise; it is the caller's to run once it has recorded the outcome.
func performSyncReceive(ctx context.Context, ws workspace.Info, clone liveClone, fetch receiveFetch) (outcome syncReceiveOutcome, closeLive func() error, err error) {
	closeLive = func() error { return nil }
	target := fetch.target
	if target.skip != syncTargetReady {
		return syncReceiveOutcome{skip: target.skip, remote: target.remote}, closeLive, nil
	}
	remoteName, syncBranch := target.remote, target.branch

	var result storage.SyncReceiveResult
	receiveErr := fetch.fetchErr
	if receiveErr == nil {
		receiveErr = landFetch(ctx, ws, clone, target)
	}
	var session syncSession
	if receiveErr == nil {
		session, closeLive, err = openSyncSession(ctx, ws)
		if err != nil {
			return syncReceiveOutcome{}, func() error { return nil }, fmt.Errorf("open sync store: %w", err)
		}
		result, receiveErr = session.syncer.SyncSettleReceived(ctx, remoteName, syncBranch)
	}
	traceMetadata := map[string]string{
		"remote":      remoteName,
		"sync_branch": syncBranch,
		"state":       string(result.State),
		"ahead":       strconv.FormatInt(result.Ahead, 10),
		"behind":      strconv.FormatInt(result.Behind, 10),
	}
	traceStatus := "ok"
	traceReason := receiveReasonForState(result.State)
	receiveDecision := string(result.State)
	if receiveErr != nil {
		traceStatus = "error"
		traceReason = receiveErr.Error()
		traceMetadata["error"] = receiveErr.Error()
		receiveDecision = "error"
	}
	traceRecordErr := recordReceiveTrace(ws, receiveDecision, traceStatus, traceReason, traceMetadata)
	outcome = syncReceiveOutcome{
		remote:             remoteName,
		branch:             syncBranch,
		state:              result.State,
		ahead:              result.Ahead,
		behind:             result.Behind,
		oldestDivergedUnix: result.OldestDivergedUnix,
		traceErr:           traceRecordErr,
		receiveErr:         receiveErr,
	}
	// The reconcile runs INLINE on this same engine because embedded Dolt permits
	// only one read-write engine per path — a worker would collide with the next
	// foreground command. [LAW:no-ambient-temporal-coupling]
	if receiveErr == nil && result.State == storage.SyncReceiveDiverged {
		outcome.reconcile = performInlineReconcile(ctx, session, ws, remoteName, syncBranch)
	}
	return outcome, closeLive, nil
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
func reconcileOnce(ctx context.Context, session syncSession, remote, branch string) (storage.SyncReconcileResult, error) {
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
func performInlineReconcile(ctx context.Context, session syncSession, ws workspace.Info, remote, branch string) *reconcileOutcome {
	result, reconcileErr := reconcileOnce(ctx, session, remote, branch)
	// "replayed" matches the explicit reconcile/take reporters: the durable
	// trace carries the provenance-replay count for every outcome, zero
	// included, so the trail can distinguish a granular fold from a no-op.
	traceMetadata := map[string]string{
		"remote":      remote,
		"sync_branch": branch,
		"state":       string(result.State),
		"replayed":    strconv.Itoa(result.Replayed),
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
	// this reconcile is reached from an inline receive that commonly runs with no
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
	// The trace above records the attempt out-of-band; the caller (receiveInline)
	// surfaces a non-converging reconcile — hard failure OR held free-text — to
	// stderr through the one sync-failure contract, so both classes read as the
	// unmissable block rather than a raw "will retry" line. This function stays the
	// run-and-record step; the surfacing decision lives with the caller that holds
	// the divergence's counts and age. [LAW:decomposition]
	return &reconcileOutcome{
		state:      result.State,
		pending:    result.Pending,
		unrelated:  result.Unrelated,
		collisions: result.Collisions,
		err:        reconcileErr,
	}
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
		return "automatic receive fast-forwarded the local store to the remote head"
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
