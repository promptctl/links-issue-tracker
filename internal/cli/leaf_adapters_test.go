package cli

import (
	"context"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Test adapters for the leaf split (links-cli-1lxr). Splitting each handler
// into a declaration and a work phase changed how a command is INVOKED, not
// what it does, so the ~200 existing call sites keep calling the handler name
// and signature they always did and assert the same observable behavior.
// [LAW:behavior-not-structure] the contract under test is the command's output
// and errors; which phase declares its flags is the implementation's business.
//
// Every adapter routes through runLeaf, which drives the two phases in exactly
// the order the production pipelines do — declare, parse, then work. That is
// what keeps these from becoming a second dispatch path: no test can reach a
// work phase through an ordering appCmdPipeline/wsCmdPipeline would not
// produce. [LAW:single-enforcer]

// runLeaf drives a leaf the way its pipeline does, minus the acquisition the
// pipeline performs between parse and work: the resource is whatever the test
// supplies.
func runLeaf[R any](l leaf[R], ctx context.Context, stdout io.Writer, res R, args []string) error {
	positional, err := parseLeaf(l, args, stdout)
	if err != nil {
		return err
	}
	return l.work(ctx, stdout, res, positional)
}

// App-mode handlers.

func runNew(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(newLeaf(), ctx, stdout, ap, args)
}

func runFollowup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(followupLeaf(), ctx, stdout, ap, args)
}

func runShow(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(showLeaf(), ctx, stdout, ap, args)
}

func runHistory(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(historyLeaf(), ctx, stdout, ap, args)
}

func runUpdate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(updateLeaf(), ctx, stdout, ap, args)
}

func runNext(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(nextLeaf(), ctx, stdout, ap, args)
}

func runOrphaned(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(orphanedLeaf(), ctx, stdout, ap, args)
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(childrenLeaf(), ctx, stdout, ap, args)
}

func runDoctor(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(doctorLeaf(), ctx, stdout, ap, args)
}

func runImportTree(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(importTreeLeaf(), ctx, stdout, ap, args)
}

func runLabelAdd(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(labelAddLeaf(), ctx, stdout, ap, args)
}

func runParentSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(parentSetLeaf(), ctx, stdout, ap, args)
}

func runDepAdd(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(depAddLeaf(), ctx, stdout, ap, args)
}

func runBulkLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(bulkLabelLeaf(), ctx, stdout, ap, args)
}

func runBulkClose(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(bulkCloseLeaf(), ctx, stdout, ap, args)
}

// runRank keeps the `set` dispatch the old handler owned, so a test passing
// `{"set", ...}` still reaches the rank-set surface.
func runRank(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	l, rest := rankDispatch(args)
	return runLeaf(l, ctx, stdout, ap, rest)
}

func runRankSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	return runLeaf(rankSetLeaf(), ctx, stdout, ap, args)
}

// App-mode handlers parameterised by the spec or view they serve.

func runTransition(ctx context.Context, stdout io.Writer, ap *app.App, args []string, spec transitionSpec) error {
	return runLeaf(transitionLeaf(spec), ctx, stdout, ap, args)
}

func runWorkable(ctx context.Context, stdout io.Writer, ap *app.App, args []string, view workableView) error {
	return runLeaf(workableLeaf(view), ctx, stdout, ap, args)
}

// Workspace-mode handlers.

func runQuickstart(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(quickstartLeaf(), ctx, stdout, ws, args)
}

func runLifeboatRecover(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(lifeboatRecoverLeaf(), ctx, stdout, ws, args)
}

func runBackgroundMirror(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(backgroundMirrorLeaf(), ctx, stdout, ws, args)
}

// runHooksInstall never took a context: installing hooks is filesystem work
// with nothing to cancel. The leaf's work carries one because every leaf's does,
// so the adapter supplies the background context the pipeline would.
func runHooksInstall(stdout io.Writer, ws workspace.Info, args []string) error {
	return runLeaf(hooksInstallLeaf(), context.Background(), stdout, ws, args)
}

// Sync-mode handlers, over the workspace-plus-session scope their work needs.

func runSyncCompact(ctx context.Context, stdout io.Writer, ws workspace.Info, session syncSession, args []string) error {
	return runLeaf(syncCompactLeaf(), ctx, stdout, syncScope{ws: ws, session: session}, args)
}

// Version-traversal handlers, over the narrow typed dependencies that let these
// run with no workspace at all.

func runUpgradeWith(
	ctx context.Context,
	stdout io.Writer,
	schema schemaReader,
	args []string,
	current version.Info,
	resolver upgradeResolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	l := upgradeLeafWith(resolver, installer, binPathFn)
	return runLeaf(l, ctx, stdout, upgradeScope{schema: schema, current: current}, args)
}

func runDowngradeWith(
	ctx context.Context,
	stdout io.Writer,
	store schemaDowngrader,
	args []string,
	resolver release.Resolver,
	installer release.Installer,
	binPathFn func() (string, error),
) error {
	l := downgradeLeafWith(resolver, installer, binPathFn)
	return runLeaf(l, ctx, stdout, store, args)
}

// runListWithStore drives `ls` against a store the test has already opened —
// the same seam runList hands its two acquisition paths to.
func runListWithStore(ctx context.Context, stdout io.Writer, st storage.Store, policy readyPolicy, args []string) error {
	l, _ := lsLeaf() // the store is supplied here, so --at has nothing to route
	return runLeaf(l, ctx, stdout, listScope{store: st, policy: policy}, args)
}
