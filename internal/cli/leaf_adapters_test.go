package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Test adapters for the leaf split. Each handler is split into
// a declaration and a work phase, which is how a command is INVOKED, not what
// it does, so call sites keep calling a handler name and signature and assert
// the same observable behavior.
// [LAW:behavior-not-structure] the contract under test is the command's output
// and errors; which phase declares its flags is the implementation's business.
//
// Every adapter routes through recordLeaf (runLeaf is recordLeaf with the
// event dropped), which drives the two phases in exactly the order the
// production pipelines do — declare, parse, then work. That is what keeps
// these from becoming a second dispatch path: no test can reach a work phase
// through an ordering appCmdPipeline/wsCmdPipeline would not produce.
// [LAW:single-enforcer]

// underInvocation is ctx carrying an open invocation event, as Run hands one
// to everything an invocation runs.
func underInvocation(ctx context.Context) context.Context {
	return telemetry.With(ctx, telemetry.Begin(telemetry.Event{}))
}

// runLeaf drives a leaf the way its pipeline does, minus the acquisition the
// pipeline performs between parse and work: the resource is whatever the test
// supplies.
func runLeaf[R any](t testing.TB, l leaf[R], ctx context.Context, stdout io.Writer, res R, args []string) error {
	_, err := recordLeaf(l, ctx, dirInvocation(t, t.TempDir(), stdout, stdout, nil), res, args)
	return err
}

// recordLeaf is runLeaf over the invocation the test builds, under a recorder
// it ends the way the dispatcher ends one, so the test reads the event the run
// recorded beside its error.
func recordLeaf[R any](l leaf[R], ctx context.Context, inv Invocation, res R, args []string) (telemetry.Event, error) {
	recorder := telemetry.Begin(telemetry.Event{})
	ctx = telemetry.With(ctx, recorder)
	positional, err := parseLeaf(ctx, l, args, inv)
	if err == nil {
		err = l.work(ctx, inv, res, positional)
	}
	return recorder.Finish(invocationEnd(time.Now(), err, nil)), err
}

// App-mode handlers.

func runNew(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, newLeaf(), ctx, stdout, ap, args)
}

func runFollowup(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, followupLeaf(), ctx, stdout, ap, args)
}

func runShow(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, showLeaf(), ctx, stdout, ap, args)
}

func runHistory(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, historyLeaf(), ctx, stdout, ap, args)
}

func runUpdate(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, updateLeaf(), ctx, stdout, ap, args)
}

func runNext(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, nextLeaf(), ctx, stdout, ap, args)
}

func runOrphaned(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, orphanedLeaf(), ctx, stdout, ap, args)
}

// runChildren drives `children` against the workspace store the test opened —
// the listing leaf under its children surface, exactly as `lit children` runs it.
func runChildren(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runListLeaf(t, ctx, stdout, childrenSurface, listScope{store: ap.Store, policy: workspaceReadyPolicy(runEnv(t), ap)}, args)
}

func runDoctor(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, doctorLeaf(), ctx, stdout, ap, args)
}

func runImportTree(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, importTreeLeaf(), ctx, stdout, ap, args)
}

func runLabelAdd(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, labelAddLeaf(), ctx, stdout, ap, args)
}

func runParentSet(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, parentSetLeaf(), ctx, stdout, ap, args)
}

func runDepAdd(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, depAddLeaf(), ctx, stdout, ap, args)
}

func runBulkLabel(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, bulkLabelLeaf(), ctx, stdout, ap, args)
}

func runBulkClose(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runBulkTransition(t, ctx, stdout, ap, args, closeSpec)
}

func runBulkTransition(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string, spec transitionSpec) error {
	return runLeaf(t, bulkTransitionLeaf(spec)(), ctx, stdout, ap, args)
}

// runRank keeps the `set` dispatch, so a test passing `{"set", ...}` still
// reaches the rank-set surface.
func runRank(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	l, rest := rankDispatch(args)
	return runLeaf(t, l, ctx, stdout, ap, rest)
}

func runRankSet(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(t, rankSetLeaf(), ctx, stdout, ap, args)
}

// App-mode handlers parameterised by the spec or view they serve.

func runTransition(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string, spec transitionSpec) error {
	return runLeaf(t, transitionLeaf(spec), ctx, stdout, ap, args)
}

func runWorkable(t testing.TB, ctx context.Context, stdout io.Writer, ap *app.App, args []string, view workableView) error {
	return runLeaf(t, workableLeaf(view), ctx, stdout, ap, args)
}

// Workspace-mode handlers.

func runQuickstart(t testing.TB, ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(t, quickstartLeaf(), ctx, stdout, ws, args)
}

func runLifeboatRecover(t testing.TB, ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(t, lifeboatRecoverLeaf(), ctx, stdout, ws, args)
}

// runBackgroundMirror runs the mirror against ws's store with no handoff on
// stdin, so its workspace build fails: what it reaches is whichever of the
// mirror's endings precedes that failure's (the beacon's, a teardown's).
func runBackgroundMirror(t testing.TB, ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(t, backgroundMirrorLeaf(), ctx, stdout, struct{}{}, append([]string{"--store", ws.StorageDir}, args...))
}

// runHooksInstall never took a context: installing hooks is filesystem work
// with nothing to cancel. The leaf's work carries one because every leaf's does,
// so the adapter supplies the background context the pipeline would.
func runHooksInstall(t testing.TB, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(t, hooksInstallLeaf(), context.Background(), stdout, ws, args)
}

// Sync-mode handlers, over the workspace-plus-session scope their work needs.

func runSyncCompact(t testing.TB, ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	return runLeaf(t, syncCompactLeaf(), ctx, stdout, syncScope{ws: ws, session: session}, args)
}

// Version-traversal handlers, over the narrow typed dependencies that let these
// run with no workspace at all.

func runUpgradeWith(
	t testing.TB,
	ctx context.Context,
	stdout io.Writer,
	schema schemaReader,
	args []string,
	current version.Info,
	resolver upgradeResolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	l := upgradeLeafWith(resolver, installer, binPathFn).leaf
	return runLeaf(t, l, ctx, stdout, upgradeScope{schema: schema, current: current}, args)
}

func runDowngradeWith(
	t testing.TB,
	ctx context.Context,
	stdout io.Writer,
	scope downgradeScope,
	args []string,
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	l := downgradeLeafWith(resolver, installer, binPathFn).leaf
	return runLeaf(t, l, ctx, stdout, scope, args)
}

// runListWithStore drives `ls` against a store the test has already opened —
// the same seam runList hands its two acquisition paths to.
func runListWithStore(t testing.TB, ctx context.Context, stdout io.Writer, st storage.Store, policy readyPolicy, args []string) error {
	// the store is supplied here, so --at has nothing to route
	return runListLeaf(t, ctx, stdout, lsSurface, listScope{store: st, policy: policy}, args)
}

// runListLeaf is runList with the acquisition replaced by a supplied scope: the
// same parse and the same positional read, then the work.
func runListLeaf(t testing.TB, ctx context.Context, stdout io.Writer, surface listSurface, scope listScope, args []string) error {
	ctx = underInvocation(ctx)
	l, _ := listLeaf(surface)
	declared, err := parseLeaf(ctx, l, args, dirInvocation(t, t.TempDir(), stdout, stdout, nil))
	if err != nil {
		return err
	}
	positional, err := listPositionals(declared, surface)
	if err != nil {
		return err
	}
	return l.work(ctx, dirInvocation(t, t.TempDir(), stdout, stdout, nil), scope, positional)
}
