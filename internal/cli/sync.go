package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

const debugSyncBranchEnvVar = "LINKS_DEBUG_DOLT_SYNC_BRANCH"

// firstPushSkipMessage is emitted when lit sync is invoked against a remote
// that advertises no refs at all. This is only a legitimate state during the
// very first push to a brand-new empty repo; in every other situation it
// indicates a real problem (wrong URL, auth failure that ls-remote didn't
// surface as an error, etc.) and must not be silently ignored.
const firstPushSkipMessage = "Skipping lit sync: remote has no refs yet. " +
	"This is normal ONLY for the very first push to a brand-new empty repo. " +
	"If you have pushed to this remote before, this message means something is wrong — " +
	"check the remote URL, credentials, or run `git ls-remote <remote>`."

// syncRunFn is the handler shape for sync subcommands: every one operates on
// the workspace's open sync store.
type syncRunFn func(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error

// withSyncStore adapts a sync handler to the workspace family shape, owning
// the sync store's open/close lifecycle so no handler manages it.
// [LAW:no-ambient-temporal-coupling]
func withSyncStore(run syncRunFn) wsRunFn {
	return func(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
		syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
		if err != nil {
			return err
		}
		defer syncStore.Close()
		return run(ctx, stdout, ws, syncStore, args)
	}
}

var syncFamily = commandFamily[wsRunFn]{
	usage: "usage: lit sync <status|remote|fetch|pull|push|reconcile> ...",
	subcommands: []subcommandRow[wsRunFn]{
		{name: "status", payload: withSyncStore(runSyncStatus)},
		{name: "remote", payload: withSyncStore(runSyncRemote)},
		{name: "fetch", payload: withSyncStore(runSyncFetch)},
		{name: "pull", payload: withSyncStore(runSyncPull)},
		{name: "push", payload: withSyncStore(runSyncPush)},
		{name: "reconcile", payload: withSyncStore(runSyncReconcile)},
		// Hidden: the detached on-change mirror entrypoint. Absent from `usage`
		// above, so it never shows in help; it manages its own store lifecycle
		// (wait-for-parent, then open) and so is registered without withSyncStore.
		{name: backgroundMirrorSubcommand, payload: runBackgroundMirror, hidden: true},
	},
}

var syncRemoteFamily = commandFamily[syncRunFn]{
	usage: "usage: lit sync remote ls",
	subcommands: []subcommandRow[syncRunFn]{
		{name: "ls", payload: runSyncRemoteLs},
	},
}

func runSyncRemote(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	run, err := syncRemoteFamily.resolve(args)
	if err != nil {
		return err
	}
	return run(ctx, stdout, ws, syncStore, args[1:])
}

func runSyncRemoteLs(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync remote ls")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"git=%d dolt=%d added=%d updated=%d removed=%d\n",
		len(syncState.gitRemotes),
		len(syncState.doltRemotes),
		len(syncState.changes.Added),
		len(syncState.changes.Updated),
		len(syncState.changes.Removed),
	)
	return err
}

func runSyncFetch(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync fetch")
	remote := fs.String("remote", "origin", "Remote name")
	prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if _, err := syncDoltRemotesFromGit(ctx, syncStore, ws); err != nil {
		// A could-not-attempt failure (git-remote reconciliation itself failed,
		// before any fetch was even tried) is still a decision this command
		// reached — trace it, matching the coverage recordMirrorError/
		// recordReceiveError give this same failure class on the mirror/receive
		// paths. [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync fetch", "error", err, nil)
		return err
	}
	remoteName := strings.TrimSpace(*remote)
	fetchErr := syncStore.SyncFetch(ctx, remoteName, *prune)
	recordSyncCommandTrace(ws, "lit sync fetch", "fetched", fetchErr, map[string]string{"remote": remoteName})
	if fetchErr != nil {
		return fetchErr
	}
	if err := markFetchSuccess(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
	}
	if !*verbose {
		_, err := fmt.Fprintln(stdout, "fetched")
		return err
	}
	_, err := fmt.Fprintf(stdout, "fetched %s\n", remoteName)
	return err
}

func runSyncPull(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync pull")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	progressf("sync pull", "starting: reconciling remotes and resolving the sync source")
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		// A could-not-attempt failure — traced like every other decision this
		// command reaches, matching the coverage recordMirrorError/
		// recordReceiveError give this same failure class on the mirror/receive
		// paths. [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync pull", "error", err, nil)
		return err
	}
	remoteName, remoteErr := resolveSyncRemote(
		strings.TrimSpace(*remote),
		workspace.UpstreamRemote(ctx, ws.RootDir),
		syncState.gitRemotes,
	)
	if remoteErr != nil {
		recordSyncCommandTrace(ws, "lit sync pull", "error", remoteErr, nil)
		return remoteErr
	}
	if remoteName == "" {
		payload := map[string]any{
			"status": "skipped",
			"reason": "no_sync_remote",
			"raw":    "no upstream remote and no single configured remote; skipping sync pull",
		}
		recordSyncCommandTrace(ws, "lit sync pull", "no_sync_remote", nil, nil)
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return printSyncPullPayload(stdout, payload, *verbose)
	}
	// [LAW:single-enforcer] First-push detection is centralized so pull and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ctx, ws.RootDir, remoteName)
	// [LAW:no-silent-failure] A failed refs check is not "remote empty": surface it so
	// an explicit pull reports the real ls-remote cause (a cancelled ctx yields
	// context.Canceled here) rather than falling through to the misleading "default
	// branch unavailable" that DefaultRemoteBranch's swallowed error would produce.
	// This matches the receive path, so receive/pull/push treat refsErr identically.
	if refsErr != nil {
		checkErr := fmt.Errorf("check remote refs %q: %w", remoteName, refsErr)
		recordSyncCommandTrace(ws, "lit sync pull", "error", checkErr, map[string]string{"remote": remoteName})
		return checkErr
	}
	if !hasRefs {
		payload := map[string]any{
			"status": "skipped",
			"reason": "remote_empty",
			"remote": remoteName,
			"raw":    firstPushSkipMessage,
		}
		recordSyncCommandTrace(ws, "lit sync pull", "remote_empty", nil, map[string]string{"remote": remoteName})
		return printSyncPullPayload(stdout, payload, *verbose)
	}
	resolvedBranch, err := resolveSyncBranch(ctx, ws.RootDir, remoteName)
	if err != nil {
		recordSyncCommandTrace(ws, "lit sync pull", "error", err, map[string]string{"remote": remoteName})
		return err
	}
	progressf("sync pull", "pulling lit data from %s/%s (transfer and apply may take a moment)", remoteName, resolvedBranch)
	result, err := syncStore.SyncPull(ctx, remoteName, resolvedBranch)
	pullTraceMetadata := map[string]string{"remote": remoteName, "sync_branch": resolvedBranch}
	if err != nil {
		recordSyncCommandTrace(ws, "lit sync pull", "error", err, pullTraceMetadata)
		// An explicit pull is not best-effort: a fetch or reconcile failure is
		// surfaced as a command error, not swallowed the way the background
		// receive tolerates a transient hiccup. [LAW:no-silent-failure] A remote
		// schema ahead of this binary surfaces as the one sync-failure contract
		// (exit ExitConflict, naming `lit upgrade`), not the raw store refusal.
		return asSyncFailure(err)
	}
	// SyncPull succeeding means its internal SyncReceive fetched the remote
	// successfully (the fetch is the first step of every outcome branch below),
	// so this is a real "we talked to the remote" moment regardless of state.
	if err := markFetchSuccess(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
	}
	pullTraceMetadata["state"] = string(result.State)
	// A held free-text conflict is a non-transient divergence the agent must
	// resolve: it routes through the one sync-failure contract and is RETURNED, so
	// the command exits ExitConflict — the same exit `lit sync reconcile` gives for
	// the identical state, rather than a stdout line under a success exit. Every
	// other pull outcome is a payload the printer renders. [LAW:single-enforcer]
	if failure, held := syncFailureFromPull(remoteName, resolvedBranch, result, time.Now()); held {
		recordSyncHeldTrace(ws, "lit sync pull", failure, pullTraceMetadata)
		// A held pull is a detection moment: the owner hears out-of-band, the day
		// it happens, not through later archaeology (links-sync-pgct.4).
		if ev, ok := ownerNotifyEventForFailure(failure.Failure); ok {
			maybeNotifyOwner(ctx, ws, ev)
		}
		return failure
	}
	recordSyncCommandTrace(ws, "lit sync pull", string(result.State), nil, pullTraceMetadata)
	// A pull that completed without a held state converged (or found nothing to
	// converge): the divergence episode, if one was notified, is over.
	clearOwnerNotify(ws, ownerNotifyDivergenceKinds...)
	return printSyncPullPayload(stdout, buildSyncPullPayload(remoteName, resolvedBranch, result), *verbose)
}

// syncFailureFromPull builds the sync-failure contract for a pull outcome the
// agent must resolve, or held=false for an outcome the payload printer renders. Two
// pull outcomes are agent-actionable this way: a held free-text conflict and a
// no-common-ancestor divergence — both non-transient, both routed through the one
// contract so the exit code and the block match `lit sync reconcile`. A hard pull
// error is already surfaced as a returned error upstream. It is a mostly-pure
// mapping — the clock is supplied as an argument, so the contract SHAPE is
// unit-testable without a live store — but it also reads this binary's build
// identity via resolveBuildStatusNote (link-time version vars plus the embedded
// migration registry), so it is not safe to memoize or assume side-effect-free.
// [LAW:dataflow-not-control-flow]
func syncFailureFromPull(remote, branch string, result store.SyncPullResult, now time.Time) (SyncFailureError, bool) {
	base := SyncFailure{
		Remote:    remote,
		Branch:    branch,
		Ahead:     result.Ahead,
		Behind:    result.Behind,
		Age:       ageFromOldestDivergedUnix(result.OldestDivergedUnix, now),
		BuildNote: resolveBuildStatusNote(now),
	}
	switch result.State {
	case store.SyncPullProsePending:
		base.Class = syncFailureProseHeld
		base.Fields = result.Pending
		return SyncFailureError{Failure: base}, true
	case store.SyncPullUnrelated:
		base.Class = syncFailureUnrelatedHistories
		base.Inventory = result.Unrelated
		return SyncFailureError{Failure: base}, true
	default:
		return SyncFailureError{}, false
	}
}

func runSyncPush(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync push")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
	force := fs.Bool("force", false, "Pass --force to dolt push")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	// [LAW:decomposition] The explicit `lit sync push` (and the pre-push hook it
	// backs) compacts atomically with the push; the on-change mirror pushes
	// without compaction. The choice is the push step passed as a value, so
	// performSyncPush has no compaction branch and a skipped push never compacts.
	outcome, err := performSyncPush(ctx, syncStore, ws, strings.TrimSpace(*remote), *setUpstream, *force, syncStore.SyncCompactAndPush)
	if err != nil {
		// A could-not-attempt failure (performSyncPush returned before reaching
		// its own trace-recording push attempt) — traced here, matching the
		// coverage recordMirrorError already gives this exact failure class on
		// the on-change mirror path that shares performSyncPush.
		// [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync push", "error", err, nil)
		return err
	}
	// [LAW:no-silent-failure] The push error surfaces as the command's exit
	// status only after its trace has been recorded inside performSyncPush —
	// the skipped/ok payload is never printed over a failed push.
	if outcome.pushErr != nil {
		// A remote schema ahead of this binary surfaces as the one sync-failure
		// contract (exit ExitConflict, naming `lit upgrade`) rather than the raw
		// non-fast-forward/refusal string. [LAW:single-enforcer]
		return asSyncFailure(outcome.pushErr)
	}
	return printSyncPushPayload(stdout, outcome.payload(), *verbose)
}

// syncPushOutcome is the result of one push attempt, independent of CLI
// presentation. [LAW:decomposition] Reconciling remotes, resolving
// remote+branch, pushing, and recording the trace are one part; flag parsing
// and payload rendering are another. The `lit sync push` command and the
// on-change cadence owner both consume this one orchestration so their push
// behavior cannot drift. [LAW:single-enforcer]
type syncPushOutcome struct {
	status     string // "ok" | "skipped"
	reason     string // set when status == "skipped"
	remote     string
	branch     string
	message    string
	pushStatus int64
	traceRef   *automationTraceRef
	traceErr   error
	pushErr    error // the push failure; the trace is already recorded when set
}

// payload renders the outcome into the map shape printSyncPushPayload consumes.
func (o syncPushOutcome) payload() map[string]any {
	payload := map[string]any{
		"status": o.status,
		"remote": o.remote,
		"branch": o.branch,
		"raw":    o.message,
	}
	if o.status == "skipped" {
		payload["reason"] = o.reason
		return payload
	}
	payload["push_status"] = o.pushStatus
	if o.traceRef != nil {
		payload["trace_ref"] = o.traceRef.Path
	}
	if o.traceErr != nil {
		payload["trace_error"] = o.traceErr.Error()
	}
	return payload
}

// syncPushStep is the push the orchestrator runs once it has resolved the
// remote and branch — store.Store.SyncPush (push only) or
// store.Store.SyncCompactAndPush (compact + push, atomic). [LAW:dataflow-not-control-flow]
// The variant is a value the caller supplies, so performSyncPush carries no
// compaction branch and never compacts a push it skips.
type syncPushStep func(ctx context.Context, remote, branch string, setUpstream, force bool) (store.SyncPushResult, error)

// performSyncPush reconciles Dolt remotes from git, resolves the remote and
// branch, runs the supplied push step, and records an automation trace for the
// attempt. The returned error is a "could not attempt" failure (reconcile or
// remote resolution); a push that ran and failed is carried in outcome.pushErr
// with its trace already recorded, leaving the caller to decide whether that
// fails it (the command) or is best-effort (the cadence owner).
func performSyncPush(ctx context.Context, syncStore *store.Store, ws workspace.Info, remote string, setUpstream, force bool, push syncPushStep) (outcome syncPushOutcome, retErr error) {
	// Every completion — could-not-attempt, skip, pushed, push-failed — leaves
	// the push-outcome marker behind, so "are pushes working?" is answerable by
	// any later command without an engine. One deferred write over the named
	// results covers every return path by construction, and the same record
	// feeds the owner's out-of-band channel: a failed attempt notifies, a landed
	// push ends the episode. [LAW:single-enforcer]
	defer func() {
		rec := pushOutcomeOf(outcome, retErr)
		recordPushOutcome(ws, rec)
		observePushOutcomeForOwner(ctx, ws, rec, retErr, outcome.pushErr)
	}()
	syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
	if err != nil {
		return syncPushOutcome{}, err
	}
	remoteName, remoteErr := resolveSyncRemote(
		strings.TrimSpace(remote),
		workspace.UpstreamRemote(ctx, ws.RootDir),
		syncState.gitRemotes,
	)
	if remoteErr != nil {
		return syncPushOutcome{}, remoteErr
	}
	if remoteName == "" {
		recordSyncCommandTrace(ws, "lit sync push", "no_sync_remote", nil, nil)
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return syncPushOutcome{
			status:  "skipped",
			reason:  "no_sync_remote",
			message: "no upstream remote and no single configured remote; skipping sync push",
		}, nil
	}
	// [LAW:single-enforcer] First-push detection is centralized so pull and push share one definition of "remote is empty".
	hasRefs, refsErr := workspace.RemoteHasRefs(ctx, ws.RootDir, remoteName)
	// [LAW:no-silent-failure] A failed refs check is not "remote empty": surface the
	// original ls-remote cause (a cancelled ctx yields context.Canceled here) rather
	// than dropping it and letting a later Dolt-store push error mask it. Mirrors the
	// receive path so receive/pull/push treat refsErr identically.
	if refsErr != nil {
		return syncPushOutcome{}, fmt.Errorf("check remote refs %q: %w", remoteName, refsErr)
	}
	if !hasRefs {
		recordSyncCommandTrace(ws, "lit sync push", "remote_empty", nil, map[string]string{"remote": remoteName})
		return syncPushOutcome{
			status:  "skipped",
			reason:  "remote_empty",
			remote:  remoteName,
			message: firstPushSkipMessage,
		}, nil
	}
	syncBranch, err := resolveSyncBranch(ctx, ws.RootDir, remoteName)
	if err != nil {
		return syncPushOutcome{}, err
	}
	// [LAW:dataflow-not-control-flow] Sync push runs one deterministic embedded mutation path from resolved remote+branch state.
	result, pushErr := push(ctx, remoteName, syncBranch, setUpstream, force)
	traceMetadata := map[string]string{
		"remote":      remoteName,
		"sync_branch": syncBranch,
	}
	if strings.TrimSpace(result.Message) != "" {
		traceMetadata["message"] = strings.TrimSpace(result.Message)
	}
	traceStatus := "ok"
	traceReason := "managed automation requested sync push"
	if pushErr != nil {
		traceStatus = "error"
		traceReason = pushErr.Error()
		traceMetadata["error"] = pushErr.Error()
	}
	syncCommandArgs := []string{"sync", "push", "--remote", remoteName}
	if setUpstream {
		syncCommandArgs = append(syncCommandArgs, "--set-upstream")
	}
	if force {
		syncCommandArgs = append(syncCommandArgs, "--force")
	}
	// [LAW:one-source-of-truth] Hook-triggered sync traces reuse the shared automation trace writer instead of shell-local trace formats.
	traceRef, traceRecordErr := maybeRecordAutomatedCommandTrace(
		ws,
		formatCommand(syncCommandArgs),
		"mirror Dolt data to the configured git remote",
		traceStatus,
		traceReason,
		traceMetadata,
	)
	// The durable, unconditional counterpart: recorded whether or not
	// LNKS_AUTOMATION_TRIGGER is set, so an interactive `lit sync push` (and the
	// on-change mirror, which shares this function) both leave a trace behind.
	// Its own reason, not traceReason: "managed automation requested sync push"
	// is only true of the automation-gated record above (which fires only when
	// a trigger actually did request the push) — reusing it here would print
	// that false claim on every interactive `lit sync push` a human runs
	// directly. [LAW:no-silent-failure] a trace record that misattributes its
	// own cause is a lie the next reader has no way to catch.
	syncPushDecision := pushDecisionPushed
	durableReason := ""
	if pushErr != nil {
		syncPushDecision = pushDecisionError
		durableReason = pushErr.Error()
	}
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   formatCommand(syncCommandArgs),
		Decision:  syncPushDecision,
		Status:    traceStatus,
		Reason:    durableReason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  traceMetadata,
	})
	return syncPushOutcome{
		status:     "ok",
		remote:     remoteName,
		branch:     syncBranch,
		message:    result.Message,
		pushStatus: result.Status,
		traceRef:   traceRef,
		traceErr:   traceRecordErr,
		pushErr:    pushErr,
	}, nil
}

func runSyncStatus(ctx context.Context, stdout io.Writer, ws workspace.Info, syncStore *store.Store, args []string) error {
	fs := newCobraFlagSet("sync status")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return err
	}
	report, err := syncStore.SyncStatus(ctx)
	if err != nil {
		return err
	}
	head := strings.TrimSpace(report.HeadCommit)
	if strings.TrimSpace(report.HeadMessage) != "" {
		head = strings.TrimSpace(report.HeadCommit + " " + report.HeadMessage)
	}
	_, err = fmt.Fprintf(
		stdout,
		"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
		report.DoltVersion,
		report.Branch,
		head,
		len(syncState.gitRemotes),
		len(syncState.doltRemotes),
		len(syncState.changes.Added),
		len(syncState.changes.Updated),
		len(syncState.changes.Removed),
	)
	return err
}

func resolveSyncRemote(requestedRemote string, upstreamRemote string, gitRemotes []workspace.GitRemote) (string, error) {
	validatedRequestedRemote := strings.TrimSpace(requestedRemote)
	if validatedRequestedRemote != "" {
		// [LAW:no-silent-failure] Explicit remote that doesn't exist is a configuration error, not a skip condition.
		if !syncRemoteExists(validatedRequestedRemote, gitRemotes) {
			return "", fmt.Errorf("requested remote %q not found in configured git remotes", validatedRequestedRemote)
		}
		return validatedRequestedRemote, nil
	}
	singleRemote := ""
	if len(gitRemotes) == 1 {
		singleRemote = strings.TrimSpace(gitRemotes[0].Name)
	}
	validatedUpstreamRemote := strings.TrimSpace(upstreamRemote)
	if !syncRemoteExists(validatedUpstreamRemote, gitRemotes) {
		validatedUpstreamRemote = ""
	}
	// [LAW:one-source-of-truth] Sync remote selection is derived once from ordered candidates and shared by pull/push.
	// Candidates are trimmed where they are produced, so plain precedence suffices.
	return precedence.First(validatedUpstreamRemote, singleRemote), nil
}

func syncRemoteExists(name string, gitRemotes []workspace.GitRemote) bool {
	normalizedName := strings.TrimSpace(name)
	if normalizedName == "" {
		return false
	}
	for _, remote := range gitRemotes {
		if strings.TrimSpace(remote.Name) == normalizedName {
			return true
		}
	}
	return false
}

func resolveSyncBranch(ctx context.Context, rootDir string, remote string) (string, error) {
	debugOverride := strings.TrimSpace(os.Getenv(debugSyncBranchEnvVar))
	defaultBranch := strings.TrimSpace(workspace.DefaultRemoteBranch(ctx, rootDir, remote))
	// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks consume one canonical branch decision.
	resolvedBranch := precedence.First(debugOverride, defaultBranch)
	if resolvedBranch == "" {
		// [LAW:no-silent-failure] DefaultRemoteBranch swallows its git errors — an
		// empty branch is a legitimate "remote advertises no default" result, not an
		// error — so a cancelled ctx that kills its network ls-remote is
		// indistinguishable here from a genuine absence. This is the single point
		// that turns an empty branch into a diagnostic, so it is where the two are
		// told apart: surface the cancellation as its true cause rather than the
		// misleading "default branch unavailable". This holds for every caller and
		// does not lean on the receive/pull/push RemoteHasRefs check to have caught
		// the cancellation first — closing the window where a cancel arriving
		// between that check and DefaultRemoteBranch's fallback ls-remote would
		// otherwise lie about the reason.
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("resolve sync branch for remote %q: %w", strings.TrimSpace(remote), err)
		}
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			strings.TrimSpace(remote),
			debugSyncBranchEnvVar,
		)
	}
	return resolvedBranch, nil
}

// buildSyncPullPayload renders a completed pull into the structured payload the
// printer consumes. The outcome variance lives entirely in the result STATE, not
// in error-string parsing: a branch the remote has never seen is the typed
// never_synced state (not a raw "not found on remote" backend string). The one
// agent-actionable outcome — a held free-text conflict — never reaches here: the
// caller intercepts it into the sync-failure contract (syncFailureFromPull) before
// building a payload, so this builder renders only the non-blocking outcomes.
// [LAW:types-are-the-program] the state is the discriminator; [LAW:dataflow-not-control-flow]
// one builder, the state selects the fields.
func buildSyncPullPayload(remote string, branch string, result store.SyncPullResult) map[string]any {
	switch result.State {
	case store.SyncPullNeverSynced:
		return map[string]any{
			"status":        "skipped",
			"reason":        "remote_branch_missing",
			"remote":        remote,
			"branch":        branch,
			"next_command":  fmt.Sprintf("lit sync push --remote %s --set-upstream", remote),
			"retry_command": fmt.Sprintf("lit sync pull --remote %s", remote),
		}
	case store.SyncPullUpToDate, store.SyncPullFastForwarded, store.SyncPullLinearized, store.SyncPullAhead:
		return map[string]any{
			"status": "ok",
			"state":  string(result.State),
			"remote": remote,
			"branch": branch,
		}
	default:
		// A SyncPullState this renderer does not enumerate must not masquerade as
		// "ok" — that would hide a new state behind a bland success, the same gap
		// SyncPull's loud store-side default guards against. Surface it. The states
		// are enumerated, not lumped into a default, so adding one here is a
		// deliberate act. [LAW:no-silent-failure]
		return map[string]any{
			"status": "unknown",
			"state":  string(result.State),
			"remote": remote,
			"branch": branch,
		}
	}
}

func printSyncPullPayload(w io.Writer, payload map[string]any, verbose bool) error {
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	switch status {
	case "skipped":
		reason := strings.TrimSpace(fmt.Sprintf("%v", payload["reason"]))
		if reason == "no_sync_remote" {
			if !verbose {
				return nil
			}
			_, err := fmt.Fprintln(w, "skipped sync pull: no eligible git remote")
			return err
		}
		if reason == "remote_empty" {
			// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
			_, err := fmt.Fprintln(w, firstPushSkipMessage)
			return err
		}
		nextCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["next_command"]))
		retryCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["retry_command"]))
		if !verbose {
			_, err := fmt.Fprintf(
				w,
				"sync pull skipped; run `%s`, then retry `%s`\n",
				nextCommand,
				retryCommand,
			)
			return err
		}
		_, err := fmt.Fprintf(
			w,
			"skipped pull %s/%s: remote branch missing; run `%s`, then retry `%s`\n",
			remote,
			branch,
			nextCommand,
			retryCommand,
		)
		return err
	case "unknown":
		// buildSyncPullPayload emits this only for a SyncPullState it does not
		// enumerate — a real gap, surfaced always (never suppressed by non-verbose)
		// so a new state cannot slip out as a bland "pulled". [LAW:no-silent-failure]
		state := strings.TrimSpace(fmt.Sprintf("%v", payload["state"]))
		_, err := fmt.Fprintf(w, "sync pull produced an unrecognized state %q on %s/%s; this is a bug — please report it\n", state, remote, branch)
		return err
	default:
		if !verbose {
			_, err := fmt.Fprintln(w, "pulled")
			return err
		}
		state := strings.TrimSpace(fmt.Sprintf("%v", payload["state"]))
		if branch != "" {
			_, err := fmt.Fprintf(w, "pulled %s/%s (%s)\n", remote, branch, state)
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s (%s)\n", remote, state)
		return err
	}
}

func printSyncPushPayload(w io.Writer, payload map[string]any, verbose bool) error {
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	raw, hasRaw := payload["raw"].(string)
	reason := strings.TrimSpace(fmt.Sprintf("%v", payload["reason"]))
	if status == "skipped" && reason == "remote_empty" {
		// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
		_, err := fmt.Fprintln(w, firstPushSkipMessage)
		return err
	}
	if !verbose && status == "skipped" {
		return nil
	}
	if !verbose {
		_, err := fmt.Fprintln(w, "pushed")
		return err
	}
	if hasRaw && strings.TrimSpace(raw) != "" {
		_, err := fmt.Fprintln(w, strings.TrimSpace(raw))
		return err
	}
	if status == "skipped" {
		_, err := fmt.Fprintln(w, "skipped sync push: no eligible git remote")
		return err
	}
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	if branch != "" {
		_, err := fmt.Fprintf(w, "pushed %s/%s\n", remote, branch)
		return err
	}
	_, err := fmt.Fprintf(w, "pushed %s\n", remote)
	return err
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []store.SyncRemote
	changes     remoteSyncChanges
}

func readSyncRemoteState(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ctx, ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: doltRemotes,
		changes:     buildRemoteSyncChanges(gitRemotes, doltRemotes),
	}, nil
}

func syncDoltRemotesFromGit(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	state, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return remoteSyncState{}, err
	}
	gitRemotes := state.gitRemotes
	doltRemotes := state.doltRemotes
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := buildRemoteSyncChanges(gitRemotes, doltRemotes)

	for _, remote := range gitRemotes {
		desiredURL := store.GitBackedRemoteURL(remote.URL)
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			if err := syncStore.SyncRemoveRemote(ctx, remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if err := syncStore.SyncRemoveRemote(ctx, name); err != nil {
			return remoteSyncState{}, err
		}
	}
	finalRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: finalRemotes,
		changes:     changes,
	}, nil
}

func buildRemoteSyncChanges(gitRemotes []workspace.GitRemote, doltRemotes []store.SyncRemote) remoteSyncChanges {
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := remoteSyncChanges{
		Added:   []string{},
		Updated: []string{},
		Removed: []string{},
	}
	for _, remote := range gitRemotes {
		desiredURL := store.GitBackedRemoteURL(remote.URL)
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			changes.Added = append(changes.Added, remote.Name)
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			changes.Updated = append(changes.Updated, remote.Name)
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; !keep {
			changes.Removed = append(changes.Removed, name)
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Updated)
	sort.Strings(changes.Removed)
	return changes
}

func mapGitRemotesByName(remotes []workspace.GitRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		out[remote.Name] = remote.URL
	}
	return out
}

func mapRemotesByName(remotes []store.SyncRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		name := strings.TrimSpace(remote.Name)
		url := strings.TrimSpace(remote.URL)
		if name == "" || url == "" {
			continue
		}
		out[name] = url
	}
	return out
}
