package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/precedence"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
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
	return openSyncSessionAt(ctx, ws.DatabasePath, ws.WorkspaceID)
}

// openSyncSessionAt opens the sync engine on an explicit dolt root rather
// than the workspace's own: the on-change mirror pushes from a clone of the
// workspace's store, which is the same database at another path, under the
// same workspace identity. [LAW:one-type-per-behavior] one opener, the path
// is data.
func openSyncSessionAt(ctx context.Context, databasePath, workspaceID string) (syncSession, func() error, error) {
	st, err := engine.Open(ctx, engine.Sync, databasePath, workspaceID)
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
	return syncSession{engine: st, syncer: syncer}, func() error {
		reportEventValues(ctx, "reroot.follows", syncer.FollowedReRoots())
		reportEventValues(ctx, "fork_point.records", syncer.ForkPointRecords())
		reportEventValues(ctx, "remote.removals", syncer.RemoteRemovals())
		reportEventValues(ctx, "reconcile.bases", syncer.ReconcileBases())
		reportEventValues(ctx, "stranded_writes.commits", syncer.StrandedWritesCommits())
		reportEventValues(ctx, "settled_last.writes", syncer.SettledLastWrites())
		reportEventValues(ctx, "history_bound.records", syncer.HistoryBounds())
		return st.Close()
	}, nil
}

// reportEventValues appends one value per item to the event's occurrences
// under key: what a sync session's engine did, each item once, in the order it
// happened. A session that did none of it leaves the list empty but present,
// so "none happened" reads apart from "nothing looked". Every sync session
// closes through openSyncSessionAt, so no entry point has to remember to.
// [LAW:nothing-unseen] [LAW:single-enforcer]
func reportEventValues[T interface{ EventValue() map[string]any }](ctx context.Context, key string, items []T) {
	values := make([]any, len(items))
	for i, item := range items {
		values[i] = item.EventValue()
	}
	telemetry.Append(ctx, key, values...)
}

// syncScope is what a sync leaf's work runs against: the workspace plus the
// open sync session. It is the ACQUIRED half of a sync command — which is why a
// sync leaf's declaration cannot see it, and why asking one for help opens no
// sync store. [LAW:effects-at-boundaries]
type syncScope struct {
	ws      workspace.Info
	session syncSession
}

// The sync family's leaf shapes, over syncScope rather than the bare workspace.
type (
	syncLeaf   = leaf[syncScope]
	syncLeafFn = func() syncLeaf
)

// withSyncStore adapts a sync leaf to the workspace family shape, owning the
// sync store's open/close lifecycle so no handler manages it.
// [LAW:no-ambient-temporal-coupling] The declaration passes straight through —
// only the WORK is wrapped, so the store opens after the parse that a help
// request never gets past (links-cli-1lxr).
func withSyncStore(declare syncLeafFn) wsLeafFn {
	return func() wsLeaf {
		l := declare()
		return adaptLeaf[syncScope, workspace.Info](l, func(ctx context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
			session, closeStore, err := openSyncSession(ctx, ws)
			if err != nil {
				// The open boundary stamps holder contention so Run's trace can
				// tell a starved OPEN from a handler-traced mid-command contention.
				return markEngineOpenContention(err, ws)
			}
			defer closeStore()
			reportOpenMigration(ctx, storage.OpenMigration(session.engine))
			return l.work(ctx, stdout, syncScope{ws: ws, session: session}, positional)
		})
	}
}

var syncFamily = commandFamily[wsSubcommand]{
	usage: "usage: lit sync <status|remote|fetch|pull|push|compact|reconcile> ...",
	subcommands: []subcommandRow[wsSubcommand]{
		{name: "status", payload: wsSubcommand{declare: withSyncStore(syncStatusLeaf)}},
		{name: "remote", nestedUsage: syncRemoteFamily.usage, payload: wsSubcommand{nested: &syncRemoteFamily}},
		{name: "fetch", payload: wsSubcommand{declare: withSyncStore(syncFetchLeaf)}},
		{name: "pull", payload: wsSubcommand{declare: withSyncStore(syncPullLeaf)}},
		{name: "push", payload: wsSubcommand{declare: withSyncStore(syncPushLeaf)}},
		{name: "compact", payload: wsSubcommand{declare: withSyncStore(syncCompactLeaf)}},
		// The bare `lit sync reconcile` is the reconcile action itself; a
		// subcommand name routes into the family instead.
		{name: "reconcile", nestedUsage: reconcileFamily.usage, payload: wsSubcommand{
			nested: &reconcileFamily,
			bare:   withSyncStore(syncReconcileShowLeaf),
		}},
		// Hidden: the detached on-change mirror entrypoint. Absent from `usage`
		// above, so it never shows in help; it manages its own store lifecycle
		// (wait-for-parent, then open) and so is registered without withSyncStore.
		{name: backgroundMirrorSubcommand, payload: wsSubcommand{declare: backgroundMirrorLeaf}, hidden: true},
		// Hidden: the detached automatic-receive entrypoint, registered the
		// same way and for the same reason.
		{name: backgroundReceiveSubcommand, payload: wsSubcommand{declare: backgroundReceiveLeaf}, hidden: true},
	},
}

var syncRemoteFamily = commandFamily[wsSubcommand]{
	usage: "usage: lit sync remote ls",
	subcommands: []subcommandRow[wsSubcommand]{
		{name: "ls", payload: wsSubcommand{declare: withSyncStore(syncRemoteLsLeaf)}},
	},
}

func syncRemoteLsLeaf() syncLeaf {
	fs := newCobraFlagSet("sync remote ls")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
		syncState, err := readSyncRemoteState(ctx, session, ws)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(
			stdout,
			"git=%d dolt=%d added=%d updated=%d removed=%d refused=%d\n",
			len(syncState.gitRemotes),
			len(syncState.doltRemotes),
			len(syncState.plan.add),
			len(syncState.plan.update),
			len(syncState.plan.remove),
			len(syncState.plan.refused),
		)
		if err != nil {
			return err
		}
		// A refused remote is named, since nothing syncs with it until it is
		// renamed. [LAW:no-silent-failure]
		for _, name := range syncState.plan.refused {
			if _, err := fmt.Fprintf(stdout, "refused %s: Dolt cannot hold this name; rename it (git remote rename) to sync with it\n", name); err != nil {
				return err
			}
		}
		return nil
	}}
}

func syncFetchLeaf() syncLeaf {
	fs := newCobraFlagSet("sync fetch")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
		syncState, err := syncDoltRemotesFromGit(ctx, session, ws)
		if err != nil {
			// A could-not-attempt failure (git-remote reconciliation itself failed,
			// before any fetch was even tried) is still a decision this command
			// reached — trace it, matching the coverage recordMirrorTraceError/
			// recordReceiveError give this same failure class on the mirror/receive
			// paths. [LAW:no-silent-failure]
			recordSyncCommandTrace(ws, "lit sync fetch", "error", err, nil)
			return err
		}
		// [LAW:one-source-of-truth] the remote every sync verb selects.
		remoteName, ok, err := resolveSyncRemote(*remote, workspace.UpstreamRemote(ctx, ws.RootDir), syncState.gitRemotes)
		if err != nil {
			recordSyncCommandTrace(ws, "lit sync fetch", "error", err, nil)
			return err
		}
		if !ok {
			// Said in every mode: the staleness banner sends people here, and a
			// fetch that silently fetched nothing would leave them stale with no
			// sign why. [LAW:no-silent-failure]
			recordSyncCommandTrace(ws, "lit sync fetch", string(syncTargetNoRemote), nil, nil)
			_, err := fmt.Fprintln(stdout, "skipped sync fetch: no eligible git remote; name one with --remote")
			return err
		}
		err = fetchAndFollow(ctx, session, ws, syncState.gitRemotes, remoteName, *prune)
		recordSyncCommandTrace(ws, "lit sync fetch", "fetched", err, map[string]string{"remote": remoteName.String()})
		if err != nil {
			return err
		}
		if !*verbose {
			_, err := fmt.Fprintln(stdout, "fetched")
			return err
		}
		_, err = fmt.Fprintf(stdout, "fetched %s\n", remoteName)
		return err
	}}
}

// fetchAndFollow is `lit sync fetch`'s work, traced by its caller as one
// decision: fetch remote, then follow a re-root on the remote and branch the
// freshness reads report on (freshnessSyncTarget), so none of them reports
// that re-root as a divergence. The follow is for those reads alone, so it
// targets what they read whichever remote was fetched, and contacts no
// network: a workspace with no sync remote, one Dolt cannot hold, or none
// whose default branch is known here, has no freshness a re-root could
// misreport, and follows nothing.
func fetchAndFollow(ctx context.Context, session syncSession, ws workspace.Info, gitRemotes []workspace.GitRemote, remote storage.RemoteName, prune bool) error {
	if err := session.syncer.SyncFetch(ctx, remote, prune); err != nil {
		return err
	}
	if err := markFetchSuccess(ws); err != nil {
		fmt.Fprintf(os.Stderr, "lit: fetch-success marker not written: %v\n", err)
	}
	syncRemote, branch, ok, err := freshnessSyncTarget(ctx, ws, gitRemotes)
	// Only a branch nothing here names, or a remote lit cannot sync with,
	// skips the follow; a record that would name the branch but cannot be
	// read is not that error, and fails the fetch.
	notKnown, refused := syncBranchNotKnownError{}, storage.RefusedRemoteNameError{}
	if errors.As(err, &notKnown) || errors.As(err, &refused) || (err == nil && !ok) {
		return nil
	}
	if err != nil {
		return err
	}
	return session.syncer.SyncFollowReRoot(ctx, syncRemote, branch, storage.SyncEntryFetch)
}

func syncPullLeaf() syncLeaf {
	fs := newCobraFlagSet("sync pull")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
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
		pullTraceMetadata := map[string]string{"remote": remoteName.String(), "sync_branch": resolvedBranch}
		if err != nil {
			recordSyncCommandTrace(ws, "lit sync pull", "error", err, pullTraceMetadata)
			// An explicit pull is not best-effort: a fetch or reconcile failure is
			// surfaced as a command error, not swallowed the way the background
			// receive tolerates a transient hiccup. [LAW:no-silent-failure] A remote
			// schema ahead of this binary surfaces as the one sync-failure contract
			// (exit ExitValidation as the schema-ahead refusal it wraps, naming
			// `lit upgrade`), not the raw store refusal.
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
		landingMetadata(pullTraceMetadata, result.Landing)
		recordSyncCommandTrace(ws, "lit sync pull", string(result.State), nil, pullTraceMetadata)
		// A pull that completed without a held state converged (or found nothing to
		// converge): the divergence episode, if one was notified, is over.
		endDivergenceEpisode(ws)
		return printSyncPullOutcome(stdout, syncPullOutcome{remote: remoteName, branch: resolvedBranch, state: result.State, landing: result.Landing}, *verbose)
	}}
}

// syncFailureFromPull builds the sync-failure contract for a pull outcome the
// agent must resolve, or held=false for an outcome the outcome printer renders. Three
// pull outcomes are agent-actionable this way: a held free-text conflict, a
// no-common-ancestor divergence, and an id naming two different tickets — none
// transient, all routed through the one contract so the exit code and the block
// match `lit sync reconcile`. A hard pull error is already surfaced as a returned
// error upstream. It is a mostly-pure mapping — the clock is supplied as an
// argument, so the contract SHAPE is unit-testable without a live store — but it
// also reads this binary's build identity via resolveBuildStatusNote (link-time
// version vars plus the embedded migration registry), so it is not safe to
// memoize or assume side-effect-free.
// [LAW:dataflow-not-control-flow]
func syncFailureFromPull(remote storage.RemoteName, branch string, result storage.SyncPullResult, now time.Time) (SyncFailureError, bool) {
	base := SyncFailure{
		Remote:    remote.String(),
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
	case storage.SyncPullIDCollision:
		base.Class = syncFailureIDCollision
		base.Collisions = result.Collisions
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
func syncCompactLeaf() syncLeaf {
	fs := newCobraFlagSet("sync compact")
	full := fs.Bool("full", false, "Rewrite the old generation too — reclaims what earlier passes archived, at a cost proportional to the whole store")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
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
			// spelling one fact. [LAW:one-source-of-truth]
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
	}}
}

func syncPushLeaf() syncLeaf {
	fs := newCobraFlagSet("sync push")
	remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
	setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
	force := fs.Bool("force", false, "Pass --force to dolt push")
	verbose := fs.Bool("verbose", false, "Include detailed remote output")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
		// This push answers for every mutation committed before this engine
		// session opened: clear the mirror-pending marker so their commands (and
		// later ones observing it) stop counting on a further mirror
		// (links-sync-pgct.12). Sound only because session IS the live path's
		// one read-write engine — every command that could have observed the
		// marker committed and closed before this session opened, so its commit
		// is inside this push's HEAD. Cleared at entry, not on success: if the
		// attempt then skips or fails, that ending is loudly recorded, and the
		// next mutation re-claims and re-spawns rather than a stale "covered"
		// promise papering over a push that never landed.
		// [LAW:no-ambient-temporal-coupling]
		clearMirrorPending(ws)
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
		recordPushedAdvertisement(ws, outcome.proven)
		if outcome.pushErr != nil {
			// A remote schema ahead of this binary surfaces as the one sync-failure
			// contract (exit ExitValidation as the schema-ahead refusal it wraps,
			// naming `lit upgrade`) rather than the raw non-fast-forward/refusal
			// string. [LAW:single-enforcer]
			return asSyncFailure(outcome.pushErr)
		}
		return printSyncPushOutcome(stdout, outcome, *verbose)
	}}
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
	// [LAW:types-are-the-program] "skipped" and its reason are one fact; the
	// skip carries both. [LAW:one-source-of-truth]
	skip    syncTargetSkip
	remote  storage.RemoteName
	branch  string
	message string // the engine's verbatim push output; empty on a skip
	// maintenance is what the engine reclaimed locally while servicing this
	// push, rendered as its own line so message stays the engine's verbatim
	// push output. [LAW:one-source-of-truth]
	maintenance string
	head        string // the commit the push sent as HEAD; empty on a skip or a failure
	// reRoot is how far the push's history bound got its re-root toward the
	// remote (SyncPushResult.Bound.Publication); ReRootNotPushed when the
	// bound wrote none.
	reRoot storage.ReRootPublication
	// proven is the advertisement this push proved it left the remote showing
	// (provePushedAdvertisement); zero when it proved none.
	proven remoteAdvertisement
	// receiveMerges is whether this workspace's config runs the automatic
	// receive that merges the peer's commits a refused push was refused over;
	// false when the attempt did not reach the push.
	receiveMerges bool
	traceErr      error
	pushErr       error // the push failure; the trace is already recorded when set
}

// syncPushStep is the push the orchestrator runs once it has resolved the
// remote and branch — store.Store.SyncPushFromClone (the mirror's push from
// its clone) or store.Store.SyncCompactAndPush (compact + push, atomic).
// [LAW:dataflow-not-control-flow]
// The variant is a value the caller supplies, so performSyncPush carries no
// compaction branch and never compacts a push it skips.
type syncPushStep func(ctx context.Context, remote storage.RemoteName, branch string, setUpstream, force bool) (storage.SyncPushResult, error)

// keepHistoryBound keeps the history bound of a push that landed, inside the
// window the workspace's config names. The bound fails nothing, so a config
// Load refuses fails only the bound: the push stays landed, its maintenance
// line says the bound was not kept and why, and the event's
// history_bound.window_error carries the refusal, null when the window was
// read. [LAW:no-silent-failure]
func keepHistoryBound(ctx context.Context, session syncSession, cfg config.Config, err error, remote storage.RemoteName, branch string, pushed storage.SyncPushResult) storage.SyncPushResult {
	telemetry.Set(ctx, "history_bound.window_error", telemetry.ErrorText(err))
	if err != nil {
		notKept := "history bound not kept: " + err.Error()
		if pushed.Maintenance != "" {
			notKept = pushed.Maintenance + "; " + notKept
		}
		pushed.Maintenance = notKept
		return pushed
	}
	return session.syncer.SyncKeepHistoryBound(ctx, remote, branch, cfg.Sync.HistoryWindow, pushed)
}

// historyBoundDeadline is the deadline a landed push's history bound runs
// under. The bound crosses the network three times — the re-root's lease
// push, the remote's GC, the fetch that learns what the remote carries — and
// each leg is a transport of a push's kind, so each gets the push deadline.
// The local legs ride in the stall factor's margin: the re-roots, whose cost
// is reroot.elapsed_ms, and the follow's full GC, measured at 2.0s on an APFS
// clone of this repository's 437 MB store. The remote GC's upload has not
// been measured against a hosted remote; history_bound.elapsed_ms on the
// push's event is the figure to re-measure from. A cut bound fails nothing:
// the push landed. A cut before the re-root reaches the remote leaves the
// next push past the bound to run it again; a cut after it leaves the
// remote's history below the cut to the next re-root's GC, which collects it
// with its own, since the remote's GC replaces nothing once a push lands on
// top of the re-root. A function because the push deadline it derives from
// is the store's variable. [LAW:one-source-of-truth]
func historyBoundDeadline() time.Duration { return 3 * store.MirrorPushDeadline }

// syncPushTraceMetadata is everything the durable record says about one push
// attempt beyond its decision: where it went, what the engine said, what local
// maintenance rode along, and what went wrong.
//
// It is a function rather than a block inside performSyncPush because that is
// what makes the question "does the trace carry this?" answerable. Inline, the
// only way to ask would be to stand up a workspace, a ref-carrying remote and a
// live engine session and drive a real push. [LAW:decomposition] a job with no
// name has no test.
func syncPushTraceMetadata(remoteName storage.RemoteName, syncBranch string, result storage.SyncPushResult, pushErr error) map[string]string {
	metadata := map[string]string{
		"remote":      remoteName.String(),
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
	if head := strings.TrimSpace(result.Head); head != "" {
		metadata["head"] = head
	}
	// A superseded push is a landed goal with a rejected attempt inside it;
	// the rejection is kept in the record, under its own key, so the trail
	// shows the race and not a push that quietly succeeded.
	// [LAW:no-silent-failure]
	if result.Superseded != nil {
		metadata["superseded"] = result.Superseded.Error()
	}
	// Why a landed push carries no advertisement proof, and whether the
	// remote may carry the bound's re-root in the pushed head's place.
	// [LAW:nothing-unseen]
	if bound := result.Bound; bound.Publication != storage.ReRootNotPushed {
		metadata["after_push"] = fmt.Sprintf("the history bound wrote re-root %s of %s after the push: %s", bound.ReRooted.Head, bound.Head, bound.Publication)
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
// Precondition: the caller has already taken custody of every commit in
// session's HEAD — it cleared the mirror-pending marker at a point strictly
// ordered before any commit that session cannot see. Two callers, two ways of
// meeting it: the foreground `lit sync push` holds the live path's one
// read-write engine and clears inside that session (every command that could
// have observed the marker committed before the session opened, so it is
// inside the HEAD read); the mirror clears while holding the live store's
// journal and commit locks around the clone it pushes from (no commit can
// land between the clone and the clear). Clearing HERE, after either caller
// has released those holds, would erase a claim whose commit this session
// cannot see, which is why the clear is the caller's and not this function's.
// [LAW:no-ambient-temporal-coupling]
// performSyncPush runs under two lifetimes, and the signature names both: ctx
// bounds the push work (the mirror caps it with its push deadline), while
// completionCtx bounds the completion effects — the outcome marker and the
// owner-notify hook — which must not inherit an operation budget that has, by
// definition, already expired whenever a cut push needs them most.
// A foreground caller passes the same context twice: its one lifetime is both.
func performSyncPush(ctx, completionCtx context.Context, session syncSession, ws workspace.Info, remote string, setUpstream, force bool, push syncPushStep) (outcome syncPushOutcome, retErr error) {
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
	// lives is a push-deadline cut, and this attempt's record is the event's
	// one trace owner — so the explanation folds in here, never as a second
	// record upstream. Foreground callers pass one lifetime twice, so the
	// predicate can never hold for them. [LAW:single-enforcer]
	if pushErr != nil && ctx.Err() != nil && completionCtx.Err() == nil {
		pushErr = fmt.Errorf("%w: %w", pushDeadlineCutExplanation(), pushErr)
	}
	// One read of the workspace's config serves the bound's window and the
	// receive question a refusal is classified by. [LAW:one-source-of-truth]
	cfg, cfgErr := config.Load(pathspec.New(ws.RootDir))
	// Every push that lands keeps the history bound, whichever step landed
	// it: `lit sync push` and the pre-push hook from the live store, the
	// on-change mirror from its clone. A clone's bound publishes the re-root
	// as any peer would, and the mirror's live store then follows it
	// (receivePushedReRoot), so no network leg ever holds the live store.
	// [LAW:single-enforcer] The bound runs under its own deadline off the
	// completion lifetime, as the proof below does: the push's deadline
	// is spent, and the bound's network legs need their own.
	if pushErr == nil {
		boundCtx, cancelBound := context.WithTimeout(completionCtx, historyBoundDeadline())
		result = keepHistoryBound(boundCtx, session, cfg, cfgErr, remoteName, syncBranch, result)
		cancelBound()
	}
	traceMetadata := syncPushTraceMetadata(remoteName, syncBranch, result, pushErr)
	// A push that landed on its own moved the remote to its own write, and the
	// next receive would fetch to learn nothing unless the push records that.
	// Proving it costs one ls-remote. A failed or superseded push proves
	// nothing, nor does one whose history bound took its re-root to the
	// remote (any publication but ReRootNotPushed): the mirror may hold a
	// re-root the store's branch never followed, or a peer's push the bound's
	// fetch brought in. The trace says which way the proof went.
	// [LAW:nothing-unseen]
	// The proof runs under its own deadline off the completion lifetime: the
	// push has done its work, so the push's deadline is not the proof's.
	var proven remoteAdvertisement
	if pushErr == nil && result.Superseded == nil && result.Bound.Publication == storage.ReRootNotPushed {
		proofCtx, cancelProof := context.WithTimeout(completionCtx, pushProofDeadline)
		var unproven string
		proven, unproven = provePushedAdvertisement(proofCtx, session.syncer, ws, remoteName, target.gitRemotes)
		cancelProof()
		traceMetadata["advertisement"] = advertisementProofTrace(unproven)
	}
	traceStatus := "ok"
	traceReason := "managed automation requested sync push"
	if pushErr != nil {
		traceStatus = "error"
		traceReason = pushErr.Error()
	}
	syncCommandArgs := []string{"sync", "push", "--remote", remoteName.String()}
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
	// own cause is a lie the next reader has no way to catch. Its decision is
	// classifyPushOutcome's, the one the push-outcome marker records, so the
	// two never spell one attempt two ways. [LAW:one-source-of-truth]
	outcome = syncPushOutcome{
		remote:      remoteName,
		branch:      syncBranch,
		message:     result.Message,
		maintenance: result.Maintenance,
		head:        result.Head,
		reRoot:      result.Bound.Publication,
		proven:      proven,
		// The workspace's config, not this process's kill switch: the marker
		// this classifies is read by every later command, and their receives
		// are what merge the refusal. An unreadable config schedules no
		// receive (syncOnPolicy), and every command that reads it says so.
		receiveMerges: cfgErr == nil && cfg.Sync.Receive,
		traceErr:      traceRecordErr,
		pushErr:       pushErr,
	}
	decided := classifyPushOutcome(outcome, nil)
	recordSyncTraceLogged(ws, syncTraceRecord{
		Command:   formatCommand(syncCommandArgs),
		Decision:  decided.Decision,
		Status:    traceStatus,
		Reason:    decided.Reason,
		BuildNote: resolveBuildStatusNote(time.Now()),
		Metadata:  traceMetadata,
	})
	return outcome, nil
}

// advertisementProofTrace is how a push's trace reads the proof: proven, or
// unproven with the reason provePushedAdvertisement gave.
func advertisementProofTrace(unproven string) string {
	if unproven == "" {
		return "proven"
	}
	return "unproven: " + unproven
}

func syncStatusLeaf() syncLeaf {
	fs := newCobraFlagSet("sync status")
	return syncLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, stdout io.Writer, scope syncScope, positional []string) error {
		ws, session := scope.ws, scope.session
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
			"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d refused=%d\n",
			report.EngineVersion,
			report.Branch,
			head,
			len(syncState.gitRemotes),
			len(syncState.doltRemotes),
			len(syncState.plan.add),
			len(syncState.plan.update),
			len(syncState.plan.remove),
			len(syncState.plan.refused),
		)
		return err
	}}
}

// resolveSyncRemote picks the remote a sync verb targets: the requested one,
// else the upstream's, else the only one, else none (ok false). A chosen
// remote Dolt cannot hold is refused, never skipped for another, which would
// sync somewhere unmeant. The remote it returns is parsed, so nothing it
// reaches checks the name again. [LAW:parse-dont-validate]
func resolveSyncRemote(requestedRemote string, upstreamRemote string, gitRemotes []workspace.GitRemote) (remote storage.RemoteName, ok bool, err error) {
	chosen := strings.TrimSpace(requestedRemote)
	// [LAW:no-silent-failure] Explicit remote that doesn't exist is a configuration error, not a skip condition.
	if chosen != "" && !syncRemoteExists(chosen, gitRemotes) {
		return storage.RemoteName{}, false, fmt.Errorf("requested remote %q not found in configured git remotes", chosen)
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
	chosen = precedence.First(chosen, validatedUpstreamRemote, singleRemote)
	if chosen == "" {
		return storage.RemoteName{}, false, nil
	}
	remote, err = storage.ParseRemoteName(chosen)
	if err != nil {
		return storage.RemoteName{}, false, err
	}
	return remote, true, nil
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
	remote storage.RemoteName
	branch string
	skip   syncTargetSkip
	// gitRemotes is the git remote list the target was resolved from, so a
	// step after the push reads the same list rather than a second one.
	gitRemotes []workspace.GitRemote
}

// traceMetadata renders the resolved remote as trace metadata — nil before one
// was selected — so traces carry exactly what resolution had established.
func (t syncTarget) traceMetadata() map[string]string {
	if t.remote == (storage.RemoteName{}) {
		return nil
	}
	return map[string]string{"remote": t.remote.String()}
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
// rather than being read as a remote with nothing on it.
// [LAW:no-silent-failure] That ls-remote is also the wedge point a SIGTERM
// must be able to abandon: ctx flows to the subprocess so a network-hung fetch
// cancels here rather than outliving the interrupt until the grace-timer
// hard-exit. [LAW:no-ambient-temporal-coupling]
func resolveSyncTarget(ctx context.Context, session syncSession, ws workspace.Info, requestedRemote string) (syncTarget, error) {
	syncState, err := syncDoltRemotesFromGit(ctx, session, ws)
	if err != nil {
		return syncTarget{}, err
	}
	remoteName, ok, err := resolveSyncRemote(requestedRemote, workspace.UpstreamRemote(ctx, ws.RootDir), syncState.gitRemotes)
	if err != nil {
		return syncTarget{}, err
	}
	if !ok {
		return syncTarget{skip: syncTargetNoRemote}, nil
	}
	hasRefs, refsErr := workspace.RemoteHasRefs(ctx, ws.RootDir, remoteName.String())
	if refsErr != nil {
		return syncTarget{remote: remoteName}, fmt.Errorf("check remote refs %q: %w", remoteName, refsErr)
	}
	if !hasRefs {
		return syncTarget{remote: remoteName, skip: syncTargetRemoteEmpty}, nil
	}
	branch, err := resolveSyncBranch(ctx, ws, remoteName)
	if err != nil {
		return syncTarget{remote: remoteName}, err
	}
	return syncTarget{remote: remoteName, branch: branch, gitRemotes: syncState.gitRemotes}, nil
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
	remote storage.RemoteName
	branch string
	state  storage.SyncPullState
	// landing is what the pull's reconcile changed on the way in; empty for
	// every pull that merged nothing.
	landing storage.Landing
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
		line := "pulled\n"
		if verbose {
			line = fmt.Sprintf("pulled %s/%s (%s)\n", o.remote, o.branch, o.state)
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		// Printed whatever the verbosity: a moved ticket or a shared rank key is
		// never routine.
		return writeLanding(w, "the merged backlog", o.landing)
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

// remoteSyncPlan is the one decision of how Dolt's remotes follow git's:
// every git remote Dolt can hold is added or re-pointed, every Dolt remote git
// does not name that way is removed (one an older lit cloned under a name the
// parse refuses included), and a git remote Dolt cannot hold is refused,
// never added, so one such name stops no sync with the others.
// syncDoltRemotesFromGit executes it and the reports count it.
// [LAW:one-source-of-truth]
type remoteSyncPlan struct {
	add     []plannedRemote
	update  []plannedRemote
	remove  []string
	refused []string
}

// plannedRemote is a git remote the plan adds or re-points: the name as
// parsed and the URL Dolt is to hold for it.
type plannedRemote struct {
	name storage.RemoteName
	url  string
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []storage.SyncRemote
	plan        remoteSyncPlan
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
	plan := planRemoteSync(gitRemotes, doltRemotes)
	telemetry.Set(ctx, "remotes.refused", plan.refused)
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: doltRemotes,
		plan:        plan,
	}, nil
}

func syncDoltRemotesFromGit(ctx context.Context, session syncSession, ws workspace.Info) (remoteSyncState, error) {
	state, err := readSyncRemoteState(ctx, session, ws)
	if err != nil {
		return remoteSyncState{}, err
	}
	// Removals run first. A removal takes its refs with it only while their
	// owner names no other Dolt remote (store.SyncRemoveRemote), so a remote
	// this pass adds must not exist yet when the removal asks: git renaming
	// team/origin to team must leave team none of team/origin's refs. A pass
	// that fails partway leaves the rest to the next reconcile, which plans
	// again from what Dolt then holds. [LAW:no-ambient-temporal-coupling]
	for _, name := range state.plan.remove {
		if err := session.syncer.SyncRemoveRemote(ctx, name); err != nil {
			return remoteSyncState{}, err
		}
	}
	for _, remote := range state.plan.update {
		if err := session.syncer.SyncSetRemoteURL(ctx, remote.name, remote.url); err != nil {
			return remoteSyncState{}, err
		}
	}
	for _, remote := range state.plan.add {
		if err := session.syncer.SyncAddRemote(ctx, remote.name, remote.url); err != nil {
			return remoteSyncState{}, err
		}
	}
	state.doltRemotes, err = session.syncer.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return state, nil
}

func planRemoteSync(gitRemotes []workspace.GitRemote, doltRemotes []storage.SyncRemote) remoteSyncPlan {
	doltByName := mapRemotesByName(doltRemotes)
	// refused is never nil: the event's remotes.refused is [] when none.
	plan := remoteSyncPlan{refused: []string{}}
	// wanted is every name git asks Dolt to hold.
	wanted := map[string]bool{}
	for _, remote := range gitRemotes {
		name, err := storage.ParseRemoteName(remote.Name)
		if err != nil {
			plan.refused = append(plan.refused, remote.Name)
			continue
		}
		desired := plannedRemote{name: name, url: store.GitBackedRemoteURL(remote.URL)}
		wanted[name.String()] = true
		currentURL, exists := doltByName[name.String()]
		if !exists {
			plan.add = append(plan.add, desired)
			continue
		}
		if strings.TrimSpace(currentURL) != desired.url {
			plan.update = append(plan.update, desired)
		}
	}
	for name := range doltByName {
		if !wanted[name] {
			plan.remove = append(plan.remove, name)
		}
	}
	byName := func(a, b plannedRemote) int { return strings.Compare(a.name.String(), b.name.String()) }
	slices.SortFunc(plan.add, byName)
	slices.SortFunc(plan.update, byName)
	sort.Strings(plan.remove)
	sort.Strings(plan.refused)
	return plan
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
