package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

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
}

// syncReceiveOutcome is the result of one receive attempt, independent of how it
// was triggered. [LAW:decomposition] Resolving remotes, fetching, and fast-
// forwarding are one part; the inline scheduling that invokes it is another.
type syncReceiveOutcome struct {
	status     string // "ok" | "skipped"
	reason     string // set when status == "skipped"
	remote     string
	branch     string
	state      store.SyncReceiveState
	ahead      int64
	behind     int64
	traceErr   error
	receiveErr error // the receive failure; the trace is already recorded when set
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
	// [LAW:single-enforcer] First-push detection is the shared "remote is empty"
	// definition: an empty remote has nothing to receive.
	hasRefs, refsErr := workspace.RemoteHasRefs(ws.RootDir, remoteName)
	if refsErr == nil && !hasRefs {
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
	traceReason := "automatic receive fast-forwarded from the configured git remote"
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
	return syncReceiveOutcome{
		status:     "ok",
		remote:     remoteName,
		branch:     syncBranch,
		state:      result.State,
		ahead:      result.Ahead,
		behind:     result.Behind,
		traceErr:   traceRecordErr,
		receiveErr: receiveErr,
	}, nil
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
