package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// receiveTimeout bounds the inline receive's network fetch so an offline or slow
// remote cannot hang the command's exit. The receive runs after the command has
// already produced its output, so a timeout here only abandons the fetch (the
// next interval retries); it never affects the command's result.
const receiveTimeout = 15 * time.Second

// receiveInline fetches the remote and fast-forwards the local store when behind,
// INLINE in the command process. The caller (maybeAutoSyncAfterCommand) invokes
// it only after the command's own engine has closed and only when receive is
// enabled, so it is safe to open the one read-write engine embedded Dolt permits.
//
// It is best-effort and bounded: debounced so a command burst triggers at most
// one fetch per interval, gated on a configured remote so a single-machine repo
// does no work, and time-boxed so an offline/slow fetch cannot hang the command.
// A failure is recorded as an automation trace, not printed to the command's
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
	hasRemote, err := workspaceHasGitRemote(ws)
	if err != nil {
		// Couldn't read remotes — unexpected; surface it loudly rather than treat
		// it as "no remote". [LAW:no-silent-failure]
		recordReceiveError(ws, fmt.Errorf("check git remotes: %w", err))
		return
	}
	if !hasRemote {
		return
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, receiveTimeout)
	defer cancel()
	syncStore, err := store.OpenSync(timeoutCtx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		recordReceiveError(ws, fmt.Errorf("open sync store: %w", err))
		return
	}
	defer syncStore.Close()

	outcome, err := performSyncReceive(timeoutCtx, syncStore, ws)
	if err != nil {
		// Could-not-attempt (reconcile/remote resolution): record and stop.
		recordReceiveError(ws, err)
		return
	}
	// performSyncReceive records its own trace; surface a trace-write failure
	// rather than drop it. [LAW:no-silent-failure]
	if outcome.traceErr != nil {
		fmt.Fprintf(os.Stderr, "lit: automatic receive trace not recorded: %v\n", outcome.traceErr)
	}
	// An inline reconcile that could not converge — a held free-text conflict, or a
	// hard backend failure — is surfaced NOW through the one sync-failure contract,
	// to stderr: the command's stdout is already produced and its local reads still
	// serve, so this neither corrupts output nor fails the command, but it can no
	// longer read as an ignorable line the way the raw "will retry" error once did.
	// [LAW:no-silent-failure] [LAW:single-enforcer] one contract, whether the failure
	// flows out as a returned error or is printed inline here.
	if failure, ok := outcome.inlineSyncFailure(time.Now()); ok {
		fmt.Fprintln(os.Stderr, failure.blockString())
	}
}

// inlineSyncFailure derives the sync-failure contract for an inline reconcile
// that could not converge, or reports ok=false when the receive/reconcile settled
// cleanly. A held free-text conflict and a hard reconcile error are the two
// non-converging outcomes; both route through the one contract, with the reconcile
// error carried as the block's trailing cause rather than as an ignorable
// headline. The divergence age is computed here, at the boundary that holds the
// clock, from the timestamp the receive recorded. [LAW:dataflow-not-control-flow]
func (o syncReceiveOutcome) inlineSyncFailure(now time.Time) (SyncFailure, bool) {
	if o.reconcile == nil {
		return SyncFailure{}, false
	}
	base := SyncFailure{
		Remote: o.remote,
		Branch: o.branch,
		Ahead:  o.ahead,
		Behind: o.behind,
		Age:    ageFromOldestDivergedUnix(o.oldestDivergedUnix, now),
	}
	switch {
	case o.reconcile.err != nil:
		base.Class = syncFailureDivergedUnresolved
		base.Cause = o.reconcile.err
		return base, true
	case o.reconcile.state == store.SyncReconcileProsePending:
		base.Class = syncFailureProseHeld
		base.Fields = o.reconcile.pending
		return base, true
	default:
		return SyncFailure{}, false
	}
}

// syncReceiveOutcome is the result of one receive attempt, independent of how it
// was triggered. [LAW:decomposition] Resolving remotes, fetching, and fast-
// forwarding are one part; the inline scheduling that invokes it is another.
type syncReceiveOutcome struct {
	status             string // "ok" | "skipped"
	reason             string // set when status == "skipped"
	remote             string
	branch             string
	state              store.SyncReceiveState
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
	state   store.SyncReconcileState
	pending []merge.ProsePending
	err     error // the reconcile failure; its trace is already recorded when set
}

// performSyncReceive reconciles Dolt remotes from git, resolves the remote and
// branch (the same selection push uses, so the two never disagree), then fetches
// and fast-forwards when the local branch is strictly behind, recording an
// automation trace for the attempt. The returned error is a "could not attempt"
// failure (reconcile or remote resolution); a receive that ran and failed is
// carried in outcome.receiveErr with its trace already recorded, leaving local
// data untouched. [LAW:single-enforcer] Receive and push share remote/branch
// resolution and the trace writer so they cannot drift.
func performSyncReceive(ctx context.Context, syncStore *store.Store, ws workspace.Info) (syncReceiveOutcome, error) {
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return syncReceiveOutcome{}, err
	}
	remoteName, remoteErr := resolveSyncRemote(
		"",
		workspace.UpstreamRemote(ws.RootDir),
		syncState.gitRemotes,
	)
	if remoteErr != nil {
		return syncReceiveOutcome{}, remoteErr
	}
	if remoteName == "" {
		return syncReceiveOutcome{status: "skipped", reason: "no_sync_remote"}, nil
	}
	// First-push detection: an empty remote has nothing to receive. A read error
	// here is unexpected and must not be misread as "empty" — surface it as a
	// could-not-attempt failure so the caller records a trace. [LAW:no-silent-failure]
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remoteName)
	if refsErr != nil {
		return syncReceiveOutcome{}, fmt.Errorf("check remote refs %q: %w", remoteName, refsErr)
	}
	if !hasRefs {
		return syncReceiveOutcome{status: "skipped", reason: "remote_empty", remote: remoteName}, nil
	}
	syncBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
	if err != nil {
		return syncReceiveOutcome{}, err
	}

	result, receiveErr := syncStore.SyncReceive(ctx, remoteName, syncBranch)
	traceMetadata := map[string]string{
		"remote":      remoteName,
		"sync_branch": syncBranch,
		"state":       string(result.State),
		"ahead":       strconv.FormatInt(result.Ahead, 10),
		"behind":      strconv.FormatInt(result.Behind, 10),
	}
	traceStatus := "ok"
	traceReason := receiveReasonForState(result.State)
	if receiveErr != nil {
		traceStatus = "error"
		traceReason = receiveErr.Error()
		traceMetadata["error"] = receiveErr.Error()
	}
	// The receive has no reader for a trace ref (unlike the pre-push hook), so
	// only the trace-write error is kept. [LAW:no-silent-failure]
	_, traceRecordErr := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync receive",
		"receive Dolt data from the configured git remote",
		traceStatus,
		traceReason,
		traceMetadata,
	)
	outcome := syncReceiveOutcome{
		status:             "ok",
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
	if receiveErr == nil && result.State == store.SyncReceiveDiverged {
		outcome.reconcile = performInlineReconcile(ctx, syncStore, ws, remoteName, syncBranch)
	}
	return outcome, nil
}

// performInlineReconcile runs the field-aware reconcile on a diverged clone and
// records its own automation trace. A settled divergence converges to linear
// history transparently (like a fast-forward); a prose divergence is held as
// prose-pending for the agent surface, never auto-committed by picking a side.
// [LAW:single-enforcer] One reconcile entrypoint and one trace writer, whether
// the receive was inline or foreground.
func performInlineReconcile(ctx context.Context, syncStore *store.Store, ws workspace.Info, remote, branch string) *reconcileOutcome {
	result, reconcileErr := syncStore.SyncReconcile(ctx, remote, branch)
	traceMetadata := map[string]string{
		"remote":      remote,
		"sync_branch": branch,
		"state":       string(result.State),
	}
	traceStatus := "ok"
	traceReason := reconcileReasonForState(result.State)
	if reconcileErr != nil {
		traceStatus = "error"
		traceReason = reconcileErr.Error()
		traceMetadata["error"] = reconcileErr.Error()
	} else if result.State == store.SyncReconcileProsePending {
		traceMetadata["pending"] = strconv.Itoa(len(result.Pending))
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
	// The trace above records the attempt out-of-band; the caller (receiveInline)
	// surfaces a non-converging reconcile — hard failure OR held free-text — to
	// stderr through the one sync-failure contract, so both classes read as the
	// unmissable block rather than a raw "will retry" line. This function stays the
	// run-and-record step; the surfacing decision lives with the caller that holds
	// the divergence's counts and age. [LAW:decomposition]
	return &reconcileOutcome{state: result.State, pending: result.Pending, err: reconcileErr}
}

// reconcileReasonForState maps a reconcile outcome to its automation-trace
// reason. [LAW:one-source-of-truth] One mapping over the closed state set.
func reconcileReasonForState(state store.SyncReconcileState) string {
	switch state {
	case store.SyncReconcileLinearized:
		return "automatic reconcile merged the divergence into linear history"
	case store.SyncReconcileProsePending:
		return "automatic reconcile resolved every field but free-text diverged on both sides; held for the agent surface"
	case store.SyncReconcileNotDiverged:
		return "automatic reconcile found the branch no longer diverged; nothing to do"
	default:
		return "automatic reconcile completed with state " + string(state)
	}
}

// receiveReasonForState maps a receive outcome to the human reason recorded on
// its automation trace, so the trace describes what actually happened rather than
// assuming a fast-forward. [LAW:one-source-of-truth] One mapping from state to
// reason; an exhaustive switch over the closed SyncReceiveState set.
func receiveReasonForState(state store.SyncReceiveState) string {
	switch state {
	case store.SyncReceiveFastForwarded:
		return "automatic receive fast-forwarded the local store to the remote head"
	case store.SyncReceiveUpToDate:
		return "automatic receive found the local store already up to date with the remote"
	case store.SyncReceiveAhead:
		return "automatic receive found local ahead of the remote; nothing to receive"
	case store.SyncReceiveDiverged:
		return "automatic receive found local diverged from the remote; left for foreground reconcile"
	case store.SyncReceiveNeverSynced:
		return "automatic receive found no remote-tracking data on this branch yet"
	default:
		return "automatic receive completed with state " + string(state)
	}
}

// recordReceiveError writes a could-not-attempt failure to the shared automation
// trace so an automatic receive that fails is loud out-of-band rather than
// silent. [LAW:no-silent-failure] A trace-write failure is not swallowed — it
// goes to stderr.
func recordReceiveError(ws workspace.Info, cause error) {
	if _, traceErr := maybeRecordAutomatedCommandTrace(
		ws,
		"lit sync receive",
		"receive Dolt data from the configured git remote",
		"error",
		cause.Error(),
		map[string]string{"error": cause.Error()},
	); traceErr != nil {
		fmt.Fprintf(os.Stderr,
			"lit: automatic receive could not record failure trace (%v); original error: %v\n",
			traceErr, cause)
	}
}
