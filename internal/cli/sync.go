package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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

// syncSession is an open sync engine together with the sync capability every
// `lit sync` subcommand needs, resolved once before any handler runs.
//
// The capability is carried as a field rather than re-derived because
// [storage.Sync.Of] is a parser: what it returns is a type that could not have
// existed before the engine was asked, so nothing downstream re-asks and no
// handler can hold an engine it cannot sync with. [LAW:parse-dont-validate]
//
// The engine travels beside it so that `lit sync reconcile` can ask for
// reconcile — a separate capability, because an engine whose arrivals cannot
// conflict offers sync and stops (design.md §sync) — without re-asking for the
// one already in hand.
type syncSession struct {
	engine storage.Store
	syncer storage.Syncer
}

// openSyncSession is the one place a sync engine is opened and its capability
// resolved. [LAW:single-enforcer] The returned close is the caller's to defer;
// it releases the workspace lock, so a caller that drops it strands the lock.
func openSyncSession(ctx context.Context, ws workspace.Info) (syncSession, func() error, error) {
	st, err := engine.Open(ctx, engine.Sync, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return syncSession{}, nil, err
	}
	syncer, err := storage.Sync.Of(st)
	if err != nil {
		// An engine that cannot sync cannot serve any subcommand of this
		// family, so it is refused here rather than at each verb — and the
		// store is closed because no session is returned to close it.
		// [LAW:no-silent-failure]
		return syncSession{}, nil, errors.Join(err, st.Close())
	}
	return syncSession{engine: st, syncer: syncer}, st.Close, nil
}

// syncRunFn is the handler shape for sync subcommands: every one operates on
// the workspace's open sync session.
type syncRunFn func(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error

// withSyncStore adapts a sync handler to the workspace family shape, owning
// the sync store's open/close lifecycle so no handler manages it.
// [LAW:no-ambient-temporal-coupling]
func withSyncStore(run syncRunFn) wsRunFn {
	return func(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
		session, closeStore, err := openSyncSession(ctx, ws)
		if err != nil {
			// The open boundary stamps holder contention so Run's trace can
			// tell a starved OPEN from a handler-traced mid-command contention.
			return markEngineOpenContention(err, ws)
		}
		defer closeStore()
		return run(ctx, stdout, ws, session, args)
	}
}

var syncFamily = commandFamily[wsRunFn]{
	usage: "usage: lit sync <status|remote|fetch|pull|push|compact|reconcile> ...",
	subcommands: []subcommandRow[wsRunFn]{
		{name: "status", payload: withSyncStore(runSyncStatus)},
		{name: "remote", nestedUsage: syncRemoteFamily.usage, payload: withSyncStore(runSyncRemote)},
		{name: "fetch", payload: withSyncStore(runSyncFetch)},
		{name: "pull", payload: withSyncStore(runSyncPull)},
		{name: "push", payload: withSyncStore(runSyncPush)},
		{name: "compact", payload: withSyncStore(runSyncCompact)},
		{name: "reconcile", nestedUsage: reconcileFamily.usage, payload: withSyncStore(runSyncReconcile)},
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

func runSyncRemote(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	run, err := syncRemoteFamily.resolve(args)
	if err != nil {
		return err
	}
	return run(ctx, stdout, ws, session, args[1:])
}

func runSyncRemoteLs(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	fs := newCobraFlagSet("sync remote ls")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, session, ws)
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

func runSyncFetch(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	fs := newCobraFlagSet("sync fetch")
	remote := fs.String("remote", "origin", "Remote name")
	prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if _, err := syncDoltRemotesFromGit(ctx, session, ws); err != nil {
		// A could-not-attempt failure (git-remote reconciliation itself failed,
		// before any fetch was even tried) is still a decision this command
		// reached — trace it, matching the coverage recordMirrorTraceError/
		// recordReceiveError give this same failure class on the mirror/receive
		// paths. [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync fetch", "error", err, nil)
		return err
	}
	remoteName := strings.TrimSpace(*remote)
	fetchErr := session.syncer.SyncFetch(ctx, remoteName, *prune)
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

func runSyncPull(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	fs := newCobraFlagSet("sync pull")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	progressf("sync pull", "starting: reconciling remotes and resolving the sync source")
	target, err := resolveSyncTarget(ctx, session, ws, *remote)
	if err != nil {
		// A could-not-attempt failure — traced like every other decision this
		// command reaches, matching the coverage recordMirrorTraceError/
		// recordReceiveError give this same failure class on the mirror/receive
		// paths. [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync pull", "error", err, target.traceMetadata())
		return err
	}
	if target.skip != syncTargetReady {
		// traceMetadata is nil before a remote was selected, so the no-remote and
		// empty-remote skips trace exactly what resolution established.
		recordSyncCommandTrace(ws, "lit sync pull", string(target.skip), nil, target.traceMetadata())
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return printSyncPullOutcome(stdout, syncPullOutcome{skip: target.skip, remote: target.remote}, *verbose)
	}
	remoteName, resolvedBranch := target.remote, target.branch
	progressf("sync pull", "pulling lit data from %s/%s (transfer and apply may take a moment)", remoteName, resolvedBranch)
	result, err := session.syncer.SyncPull(ctx, remoteName, resolvedBranch)
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
	// other pull outcome is rendered by the outcome printer. [LAW:single-enforcer]
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
	return printSyncPullOutcome(stdout, syncPullOutcome{remote: remoteName, branch: resolvedBranch, state: result.State}, *verbose)
}

// syncFailureFromPull builds the sync-failure contract for a pull outcome the
// agent must resolve, or held=false for an outcome the outcome printer renders. Two
// pull outcomes are agent-actionable this way: a held free-text conflict and a
// no-common-ancestor divergence — both non-transient, both routed through the one
// contract so the exit code and the block match `lit sync reconcile`. A hard pull
// error is already surfaced as a returned error upstream. It is a mostly-pure
// mapping — the clock is supplied as an argument, so the contract SHAPE is
// unit-testable without a live store — but it also reads this binary's build
// identity via resolveBuildStatusNote (link-time version vars plus the embedded
// migration registry), so it is not safe to memoize or assume side-effect-free.
// [LAW:dataflow-not-control-flow]
func syncFailureFromPull(remote, branch string, result storage.SyncPullResult, now time.Time) (SyncFailureError, bool) {
	base := SyncFailure{
		Remote:    remote,
		Branch:    branch,
		Ahead:     result.Ahead,
		Behind:    result.Behind,
		Age:       ageFromOldestDivergedUnix(result.OldestDivergedUnix, now),
		BuildNote: resolveBuildStatusNote(now),
	}
	switch result.State {
	case storage.SyncPullProsePending:
		base.Class = syncFailureProseHeld
		base.Fields = result.Pending
		return SyncFailureError{Failure: base}, true
	case storage.SyncPullUnrelated:
		base.Class = syncFailureUnrelatedHistories
		base.Inventory = result.Unrelated
		return SyncFailureError{Failure: base}, true
	default:
		return SyncFailureError{}, false
	}
}

// runSyncCompact reclaims local storage without contacting any remote. It is
// the explicit, schedulable form of the maintenance the backstop performs on a
// threshold, and the only way to reach the deep pass on demand.
//
// It deliberately requires no remote: a solo workspace that never pushes is
// exactly the one with nothing else to collect its store, and gating
// maintenance on a remote would leave that workspace no path at all.
func runSyncCompact(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	fs := newCobraFlagSet("sync compact")
	full := fs.Bool("full", false, "Rewrite the old generation too — reclaims what earlier passes archived, at a cost proportional to the whole store")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] The flag selects a depth value; there is
	// one compaction call, not one per depth.
	mode := storage.GCNewGen
	if *full {
		mode = storage.GCFull
	}

	outcome, err := session.syncer.SyncCompact(ctx, mode)
	if err != nil {
		// The depth rides along here too: a scheduled deep pass that keeps
		// failing is indistinguishable in the trail from a failing shallow one
		// unless the record says which was asked for, and the exit code that
		// would have said so is not durable. [LAW:no-silent-failure]
		//
		// It comes off the OUTCOME rather than from the local mode, even though
		// this command knows the depth it asked for. The engine reports the
		// depth it attempted, and it also reports a pass that ran and then hit a
		// failure — which this call site cannot see and would record as a bare
		// error. Handing the recorder the local mode would leave two places
		// spelling one fact, and that is the drift this file has already had
		// twice. [LAW:one-source-of-truth]
		recordCompactFailure(ws, syncCompactTraceCommand, outcome, err)
		return err
	}
	// Recorded before the write, so a stdout that has gone away cannot erase the
	// record of a pass that really ran. Every sibling here traces success as
	// well as failure, and the backstop traces its own, so a compact that stayed
	// silent on success would split "when did compaction last succeed" across
	// two half-populated trails — the manual one holding only failures, the
	// automatic one only successes. [LAW:one-source-of-truth]
	//
	// The depth and the reclaim ride along because a shallow pass and a deep one
	// answer different questions later, and the trail cannot recover either once
	// the outcome is gone. This records through the same seam the automatic pass
	// uses, so the two cannot describe one event differently — only the command
	// name distinguishes them. [LAW:one-source-of-truth]
	recordCompactionSuccess(ws, syncCompactTraceCommand, outcome)
	// The engine reports what it reclaimed in its own vocabulary; this renders
	// that account rather than re-deriving it from a storage layout the command
	// layer has no business reading. [LAW:decomposition]
	//
	// The write's own failure is the command's failure, as in every sibling
	// handler: a store that was compacted but could not say so is not a
	// successful run. [LAW:no-silent-failure]
	_, err = fmt.Fprintf(stdout, "compacted (%s): %s\n", outcome.Depth, outcome.Detail)
	return err
}

func runSyncPush(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
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
	outcome, err := performSyncPush(ctx, ctx, session, ws, strings.TrimSpace(*remote), *setUpstream, *force, session.syncer.SyncCompactAndPush)
	if err != nil {
		// A could-not-attempt failure (performSyncPush returned before reaching
		// its own trace-recording push attempt) — traced here, matching the
		// coverage recordMirrorTraceError already gives this exact failure class on
		// the on-change mirror path that shares performSyncPush.
		// [LAW:no-silent-failure]
		recordSyncCommandTrace(ws, "lit sync push", "error", err, nil)
		return err
	}
	// [LAW:no-silent-failure] The push error surfaces as the command's exit
	// status only after its trace has been recorded inside performSyncPush —
	// the skipped/ok outcome is never printed over a failed push.
	if outcome.pushErr != nil {
		// A remote schema ahead of this binary surfaces as the one sync-failure
		// contract (exit ExitConflict, naming `lit upgrade`) rather than the raw
		// non-fast-forward/refusal string. [LAW:single-enforcer]
		return asSyncFailure(outcome.pushErr)
	}
	return printSyncPushOutcome(stdout, outcome, *verbose)
}

// syncPushOutcome is the result of one push attempt, independent of CLI
// presentation. [LAW:decomposition] Reconciling remotes, resolving
// remote+branch, pushing, and recording the trace are one part; flag parsing
// and outcome rendering are another. The `lit sync push` command and the
// on-change cadence owner both consume this one orchestration so their push
// behavior cannot drift. [LAW:single-enforcer]
type syncPushOutcome struct {
	// skip is the typed no-op discriminator: syncTargetReady means the push ran
	// (or failed running — see pushErr); a non-empty skip names why it did not.
	// [LAW:types-are-the-program] "skipped" and its reason were one fact spelled
	// as two fields; the skip carries both. [LAW:one-source-of-truth]
	skip    syncTargetSkip
	remote  string
	branch  string
	message string // the engine's verbatim push output; empty on a skip
	// maintenance is what the engine reclaimed locally while servicing this
	// push, rendered as its own line so message stays the engine's verbatim
	// push output. [LAW:one-source-of-truth]
	maintenance string
	traceErr    error
	pushErr     error // the push failure; the trace is already recorded when set
}

// syncPushStep is the push the orchestrator runs once it has resolved the
// remote and branch — store.Store.SyncPush (push only) or
// store.Store.SyncCompactAndPush (compact + push, atomic). [LAW:dataflow-not-control-flow]
// The variant is a value the caller supplies, so performSyncPush carries no
// compaction branch and never compacts a push it skips.
type syncPushStep func(ctx context.Context, remote, branch string, setUpstream, force bool) (storage.SyncPushResult, error)

// syncPushTraceMetadata is everything the durable record says about one push
// attempt beyond its decision: where it went, what the engine said, what local
// maintenance rode along, and what went wrong.
//
// It is a function rather than a block inside performSyncPush because that is
// what makes the question "does the trace carry this?" answerable. Inline, the
// only way to ask was to stand up a workspace, a ref-carrying remote and a live
// engine session and drive a real push — which is why the maintenance key
// arrived untested. [LAW:decomposition] the job had no name, so it had no test.
func syncPushTraceMetadata(remoteName, syncBranch string, result storage.SyncPushResult, pushErr error) map[string]string {
	metadata := map[string]string{
		"remote":      remoteName,
		"sync_branch": syncBranch,
	}
	if message := strings.TrimSpace(result.Message); message != "" {
		metadata["message"] = message
	}
	// The prune's report reaches the durable trace as well as stdout, because
	// this command backs the pre-push hook, and in a hook stdout is routinely
	// swallowed or never watched. A refusal that exists to be loud reaching only
	// a stream nobody reads is the failure it was written to prevent.
	// [LAW:no-silent-failure]
	if maintenance := strings.TrimSpace(result.Maintenance); maintenance != "" {
		metadata["maintenance"] = maintenance
	}
	if pushErr != nil {
		metadata["error"] = pushErr.Error()
	}
	return metadata
}

// performSyncPush reconciles Dolt remotes from git, resolves the remote and
// branch, runs the supplied push step, and records an automation trace for the
// attempt. The returned error is a "could not attempt" failure (reconcile or
// remote resolution); a push that ran and failed is carried in outcome.pushErr
// with its trace already recorded, leaving the caller to decide whether that
// fails it (the command) or is best-effort (the cadence owner).
//
// Precondition: the caller holds the path's one read-write engine (session
// is that engine). The mirror-pending clear below is only sound inside an
// engine session — that is what puts every commit whose command could have
// observed the marker strictly before this session's HEAD read.
// performSyncPush runs under two lifetimes, and the signature names both: ctx
// bounds the push work (the mirror caps it with its hold budget), while
// completionCtx bounds the completion effects — the outcome marker and the
// owner-notify hook — which must not inherit an operation budget that has, by
// definition, already expired whenever a cut push needs them most.
// [LAW:no-ambient-temporal-coupling] A foreground caller passes the same
// context twice: its one lifetime is both.
func performSyncPush(ctx, completionCtx context.Context, session syncSession, ws workspace.Info, remote string, setUpstream, force bool, push syncPushStep) (outcome syncPushOutcome, retErr error) {
	// This attempt now answers for every mutation committed before this engine
	// session opened: clear the mirror-pending marker so their commands (and
	// later ones observing it) stop counting on a further mirror
	// (links-sync-pgct.12). Cleared at entry, not on success — if the attempt
	// then skips or fails, that ending is loudly recorded just below, and the
	// next mutation re-claims and re-spawns rather than a stale "covered"
	// promise papering over a push that never landed. [LAW:no-ambient-temporal-coupling]
	clearMirrorPending(ws)
	// Every completion — could-not-attempt, skip, pushed, push-failed — leaves
	// the push-outcome marker behind, so "are pushes working?" is answerable by
	// any later command without an engine. One deferred write over the named
	// results covers every return path by construction, and the same record
	// feeds the owner's out-of-band channel: a failed attempt notifies, a landed
	// push ends the episode. [LAW:single-enforcer]
	// A panic mid-attempt (an embedded-engine panic during the push) must not
	// unwind through this defer with zero-valued results — that would fabricate
	// a "pushed" record, clear a live failure episode, and (the marker having
	// been consumed above) leave the evaporated attempt covered by nothing.
	// Record the panic as the attempt's ending, then let it continue.
	// [LAW:no-silent-failure]
	defer func() {
		if r := recover(); r != nil {
			completePushAttempt(completionCtx, ws, outcome, fmt.Errorf("sync push panicked: %v", r))
			panic(r)
		}
		completePushAttempt(completionCtx, ws, outcome, retErr)
	}()
	target, err := resolveSyncTarget(ctx, session, ws, remote)
	if err != nil {
		return syncPushOutcome{}, err
	}
	if target.skip != syncTargetReady {
		// traceMetadata is nil before a remote was selected, so the no-remote and
		// empty-remote skips trace exactly what resolution established.
		recordSyncCommandTrace(ws, "lit sync push", string(target.skip), nil, target.traceMetadata())
		// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
		return syncPushOutcome{skip: target.skip, remote: target.remote}, nil
	}
	remoteName, syncBranch := target.remote, target.branch
	// [LAW:dataflow-not-control-flow] Sync push runs one deterministic embedded mutation path from resolved remote+branch state.
	result, pushErr := push(ctx, remoteName, syncBranch, setUpstream, force)
	// A push killed by the operation lifetime while the completion lifetime
	// lives is a hold-budget cut, and this attempt's record is the event's one
	// trace owner — so the explanation folds in here, never as a second record
	// upstream. Foreground callers pass one lifetime twice, so the predicate
	// can never hold for them. [LAW:single-enforcer]
	if pushErr != nil && ctx.Err() != nil && completionCtx.Err() == nil {
		pushErr = fmt.Errorf("%w: %w", holdBudgetCutExplanation(), pushErr)
	}
	traceMetadata := syncPushTraceMetadata(remoteName, syncBranch, result, pushErr)
	traceStatus := "ok"
	traceReason := "managed automation requested sync push"
	if pushErr != nil {
		traceStatus = "error"
		traceReason = pushErr.Error()
	}
	syncCommandArgs := []string{"sync", "push", "--remote", remoteName}
	if setUpstream {
		syncCommandArgs = append(syncCommandArgs, "--set-upstream")
	}
	if force {
		syncCommandArgs = append(syncCommandArgs, "--force")
	}
	// [LAW:one-source-of-truth] Hook-triggered sync traces reuse the shared automation trace writer instead of shell-local trace formats.
	// The pre-push hook reads the trace ref through the ref-file the writer
	// itself maintains, so only the write error is kept. [LAW:no-silent-failure]
	_, traceRecordErr := maybeRecordAutomatedCommandTrace(
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
		remote:      remoteName,
		branch:      syncBranch,
		message:     result.Message,
		maintenance: result.Maintenance,
		traceErr:    traceRecordErr,
		pushErr:     pushErr,
	}, nil
}

func runSyncStatus(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	fs := newCobraFlagSet("sync status")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	syncState, err := readSyncRemoteState(ctx, session, ws)
	if err != nil {
		return err
	}
	report, err := session.syncer.SyncStatus(ctx)
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

// syncTargetSkip names the two "nothing to sync against" outcomes of
// resolveSyncTarget. Its non-empty values are the canonical reason strings the
// verbs trace and print, so a skip's identity and its spelling cannot drift
// apart. [LAW:one-source-of-truth]
type syncTargetSkip string

const (
	syncTargetReady       syncTargetSkip = ""
	syncTargetNoRemote    syncTargetSkip = "no_sync_remote"
	syncTargetRemoteEmpty syncTargetSkip = "remote_empty"
)

// syncTarget is what every sync verb starts from: either a remote+branch ready
// to sync against (skip == syncTargetReady), or a typed skip naming why there
// is nothing to sync. remote is set on every outcome that got past remote
// selection — including errors and the remote_empty skip — so callers can
// attach it to their traces. [LAW:types-are-the-program] the skip is the
// discriminator; a caller that forgets to check it holds an empty branch, not
// a plausible-looking wrong one.
type syncTarget struct {
	remote string
	branch string
	skip   syncTargetSkip
}

// traceMetadata renders the resolved remote as trace metadata — nil before one
// was selected — so traces carry exactly what resolution had established.
func (t syncTarget) traceMetadata() map[string]string {
	if t.remote == "" {
		return nil
	}
	return map[string]string{"remote": t.remote}
}

// resolveSyncTarget is the one prologue push, pull, receive, and reconcile all
// run: reconcile Dolt remotes from git, select the sync remote, detect an
// empty remote, and resolve the sync branch. It is a single implementation so
// the four verbs cannot disagree about what they sync against, and so a new
// skip or error state added here reaches all of them at once.
// [LAW:single-enforcer]
//
// A failed refs check is not "remote empty": it surfaces as an error naming
// the real ls-remote cause (a cancelled ctx yields context.Canceled here)
// rather than falling through to the misleading "default branch unavailable"
// that DefaultRemoteBranch's swallowed error would produce.
// [LAW:no-silent-failure] That ls-remote is also the wedge point a SIGTERM
// must be able to abandon: ctx flows to the subprocess so a network-hung fetch
// cancels here rather than outliving the interrupt until the grace-timer
// hard-exit. [LAW:no-ambient-temporal-coupling]
func resolveSyncTarget(ctx context.Context, session syncSession, ws workspace.Info, requestedRemote string) (syncTarget, error) {
	syncState, err := syncDoltRemotesFromGit(ctx, session, ws)
	if err != nil {
		return syncTarget{}, err
	}
	remoteName, err := resolveSyncRemote(requestedRemote, workspace.UpstreamRemote(ctx, ws.RootDir), syncState.gitRemotes)
	if err != nil {
		return syncTarget{}, err
	}
	if remoteName == "" {
		return syncTarget{skip: syncTargetNoRemote}, nil
	}
	hasRefs, refsErr := workspace.RemoteHasRefs(ctx, ws.RootDir, remoteName)
	if refsErr != nil {
		return syncTarget{remote: remoteName}, fmt.Errorf("check remote refs %q: %w", remoteName, refsErr)
	}
	if !hasRefs {
		return syncTarget{remote: remoteName, skip: syncTargetRemoteEmpty}, nil
	}
	branch, err := resolveSyncBranch(ctx, ws.RootDir, remoteName)
	if err != nil {
		return syncTarget{remote: remoteName}, err
	}
	return syncTarget{remote: remoteName, branch: branch}, nil
}

// syncPullOutcome is the result of one pull attempt, independent of CLI
// presentation — pull's counterpart to syncPushOutcome, so neither sync verb
// re-reads its own output through an untyped map. [LAW:types-are-the-program]
// the target skip and the pull state are the discriminators the printer
// switches on; there is no spelled-out status string to re-parse. The one
// agent-actionable outcome — a held free-text conflict — never becomes an
// outcome: the caller intercepts it into the sync-failure contract
// (syncFailureFromPull) first, so the printer renders only non-blocking states.
type syncPullOutcome struct {
	// skip is the typed no-op discriminator shared with push: syncTargetReady
	// means the pull ran and state carries its result.
	skip   syncTargetSkip
	remote string
	branch string
	state  storage.SyncPullState
}

// printSyncPullOutcome renders a pull outcome. The variance lives entirely in
// the typed discriminators, not in error-string parsing: a branch the remote
// has never seen is the typed never_synced state (not a raw "not found on
// remote" backend string). [LAW:dataflow-not-control-flow] one printer, the
// state selects the line.
func printSyncPullOutcome(w io.Writer, o syncPullOutcome, verbose bool) error {
	switch o.skip {
	case syncTargetNoRemote:
		if !verbose {
			return nil
		}
		_, err := fmt.Fprintln(w, "skipped sync pull: no eligible git remote")
		return err
	case syncTargetRemoteEmpty:
		// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
		_, err := fmt.Fprintln(w, firstPushSkipMessage)
		return err
	}
	switch o.state {
	case storage.SyncPullNeverSynced:
		nextCommand := fmt.Sprintf("lit sync push --remote %s --set-upstream", o.remote)
		retryCommand := fmt.Sprintf("lit sync pull --remote %s", o.remote)
		if !verbose {
			_, err := fmt.Fprintf(w, "sync pull skipped; run `%s`, then retry `%s`\n", nextCommand, retryCommand)
			return err
		}
		_, err := fmt.Fprintf(w, "skipped pull %s/%s: remote branch missing; run `%s`, then retry `%s`\n", o.remote, o.branch, nextCommand, retryCommand)
		return err
	case storage.SyncPullUpToDate, storage.SyncPullFastForwarded, storage.SyncPullLinearized, storage.SyncPullAhead:
		if !verbose {
			_, err := fmt.Fprintln(w, "pulled")
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s/%s (%s)\n", o.remote, o.branch, o.state)
		return err
	default:
		// A SyncPullState this printer does not enumerate must not masquerade as
		// "pulled" — that would hide a new state behind a bland success, the same
		// gap SyncPull's loud store-side default guards against. Surfaced always
		// (never suppressed by non-verbose). The states are enumerated, not lumped
		// into a default, so adding one here is a deliberate act.
		// [LAW:no-silent-failure]
		_, err := fmt.Fprintf(w, "sync pull produced an unrecognized state %q on %s/%s; this is a bug — please report it\n", string(o.state), o.remote, o.branch)
		return err
	}
}

func printSyncPushOutcome(w io.Writer, o syncPushOutcome, verbose bool) error {
	// The engine's local-maintenance report is independent of which push line is
	// chosen below, and every branch below returns — so it is emitted once, here,
	// ahead of the cascade, rather than repeated into each arm where a future arm
	// would forget it. It prints in both modes deliberately: the message this
	// carries is a prune declining because its key derivation disagrees with the
	// disk, and a warning reachable only behind --verbose is still silent where
	// it counts. The engine leaves it empty when it found nothing worth saying,
	// so an ordinary push gains no line. [LAW:no-silent-failure]
	if maintenance := strings.TrimSpace(o.maintenance); maintenance != "" {
		if _, err := fmt.Fprintln(w, maintenance); err != nil {
			return err
		}
	}
	switch o.skip {
	case syncTargetNoRemote:
		if !verbose {
			return nil
		}
		_, err := fmt.Fprintln(w, "no upstream remote and no single configured remote; skipping sync push")
		return err
	case syncTargetRemoteEmpty:
		// [LAW:dataflow-not-control-flow] exception: first-push skip message must always reach the caller so agents/humans see why sync did nothing.
		_, err := fmt.Fprintln(w, firstPushSkipMessage)
		return err
	}
	if !verbose {
		_, err := fmt.Fprintln(w, "pushed")
		return err
	}
	if raw := strings.TrimSpace(o.message); raw != "" {
		_, err := fmt.Fprintln(w, raw)
		return err
	}
	_, err := fmt.Fprintf(w, "pushed %s/%s\n", o.remote, o.branch)
	return err
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []storage.SyncRemote
	changes     remoteSyncChanges
}

func readSyncRemoteState(ctx context.Context, session syncSession, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ctx, ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltRemotes, err := session.syncer.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: doltRemotes,
		changes:     buildRemoteSyncChanges(gitRemotes, doltRemotes),
	}, nil
}

func syncDoltRemotesFromGit(ctx context.Context, session syncSession, ws workspace.Info) (remoteSyncState, error) {
	state, err := readSyncRemoteState(ctx, session, ws)
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
			if err := session.syncer.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			if err := session.syncer.SyncRemoveRemote(ctx, remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if err := session.syncer.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if err := session.syncer.SyncRemoveRemote(ctx, name); err != nil {
			return remoteSyncState{}, err
		}
	}
	finalRemotes, err := session.syncer.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: finalRemotes,
		changes:     changes,
	}, nil
}

func buildRemoteSyncChanges(gitRemotes []workspace.GitRemote, doltRemotes []storage.SyncRemote) remoteSyncChanges {
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

func mapRemotesByName(remotes []storage.SyncRemote) map[string]string {
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
