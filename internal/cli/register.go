package cli

import (
	"context"
	"io"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
)

// CommandSpec is the data form of a CLI subcommand. The 28-call hand registration
// in newRootCommand was [LAW:dataflow-not-control-flow] variability encoded in
// imperative call sequence; representing each subcommand as a row in a table
// lets newRootCommand run the same loop every time.
type CommandSpec struct {
	Name    string
	Summary string
	Long    string
	GroupID string
	Run     CommandRunner
}

// CommandRunner is the fully-wrapped passthrough handler. Each spec's Run
// captures the workspace/app/validation pipeline appropriate for that command,
// so the registrar loop that turns specs into cobra commands does not branch
// on command identity.
type CommandRunner func(args []string) error

// GroupSpec is a cobra group rendered into the root command's help.
type GroupSpec struct {
	ID    string
	Title string
}

// commandGroups is the canonical group list used in the root help output.
var commandGroups = []GroupSpec{
	{ID: "bootstrap", Title: "Human Bootstrap"},
	{ID: "operations", Title: "Agent Operations"},
	{ID: "structure", Title: "Dependencies & Structure"},
	{ID: "data", Title: "Sync & Data"},
	{ID: "maintenance", Title: "Setup & Maintenance"},
	{ID: "guidance", Title: "Guidance & Tooling"},
}

// appRunFn is the canonical signature for app-mode handlers. The few commands
// that need additional writers (e.g. runReady taking stderr) supply an inline
// adapter at the spec entry rather than expanding this type.
type appRunFn func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error

// wsRunFn is the canonical signature for workspace-mode handlers.
type wsRunFn func(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error

// commandRegistrar carries the entrypoint context shared by every spec's Run
// closure. Building specs through these methods absorbs the per-call variance
// (closure capture + access mode + validation) into data.
type commandRegistrar struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
}

func (r *commandRegistrar) appCmd(access appAccessMode, fn appRunFn) CommandRunner {
	return r.appCmdDynamic(func([]string) appAccessMode { return access }, fn)
}

func (r *commandRegistrar) appCmdDynamic(resolve func([]string) appAccessMode, fn appRunFn) CommandRunner {
	return func(args []string) error {
		return runWithApp(r.ctx, resolve(args), func(commandCtx context.Context, ap *app.App) error {
			return fn(commandCtx, r.stdout, ap, args)
		})
	}
}

func (r *commandRegistrar) wsCmd(fn wsRunFn) CommandRunner {
	return func(args []string) error {
		return runWithWorkspace(func(ws workspace.Info) error {
			return fn(r.ctx, r.stdout, ws, args)
		})
	}
}

func (r *commandRegistrar) transitionCmd(action string) CommandRunner {
	return r.appCmd(appAccessWrite, func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runTransition(ctx, stdout, ap, args, action)
	})
}

// withValidation prefixes a runner with a path validator. Validation is data:
// the spec carries the validator function, the runtime path is unchanged.
func withValidation(validate func([]string) error, run CommandRunner) CommandRunner {
	return func(args []string) error {
		if err := validate(args); err != nil {
			return err
		}
		return run(args)
	}
}

// commandSpecs returns the full registry. New commands are added here as a
// single row; the runtime path in newRootCommand never grows.
func commandSpecs(ctx context.Context, stdout io.Writer, stderr io.Writer) []CommandSpec {
	r := &commandRegistrar{ctx: ctx, stdout: stdout, stderr: stderr}

	readyRun := r.appCmd(appAccessRead, func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runReady(ctx, stdout, r.stderr, ap, args)
	})

	completionRun := func(args []string) error {
		return runCompletion(stdout, args)
	}

	depAccess := func(args []string) appAccessMode {
		if len(args) > 0 && args[0] == "ls" {
			return appAccessRead
		}
		return appAccessWrite
	}
	backupAccess := func(args []string) appAccessMode {
		if len(args) > 0 && (args[0] == "create" || args[0] == "list") {
			return appAccessRead
		}
		return appAccessWrite
	}

	return []CommandSpec{
		{Name: "init", Summary: "Initialize links", Long: humanBootstrapHelp, GroupID: "bootstrap",
			Run: r.wsCmd(runInit)},
		{Name: "quickstart", Summary: "Agent quickstart workflow", GroupID: "guidance",
			Run: r.wsCmd(runQuickstart)},
		{Name: "completion", Summary: "Generate shell completion script", GroupID: "guidance",
			Run: completionRun},
		{Name: "hooks", Summary: "Install git hook automation", GroupID: "maintenance",
			Run: withValidation(validateHooksCommandPath, r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runHooks(stdout, ws, args)
			}))},
		{Name: "sync", Summary: "Mirror Dolt data through git remotes", GroupID: "data",
			Run: withValidation(validateSyncCommandPath, r.wsCmd(runSync))},
		{Name: "new", Summary: "Create an issue", GroupID: "operations",
			Run: r.appCmd(appAccessWrite, runNew)},
		{Name: "followup", Summary: "File a follow-up issue parented to a just-closed ticket", GroupID: "operations",
			Run: r.appCmd(appAccessWrite, runFollowup)},
		{Name: "ready", Summary: "List open work by readiness and rank", GroupID: "operations",
			Run: readyRun},
		{Name: "next", Summary: "Print the next workable leaf to lit start", GroupID: "operations",
			Run: r.appCmd(appAccessRead, runNext)},
		{Name: "orphaned", Summary: "List in_progress issues with no recent updates", GroupID: "operations",
			Run: r.appCmd(appAccessRead, runOrphaned)},
		{Name: "ls", Summary: "List issues (rank by default)", GroupID: "operations",
			Run: r.appCmd(appAccessRead, runList)},
		{Name: "show", Summary: "Show issue details", GroupID: "operations",
			Run: r.appCmd(appAccessRead, runShow)},
		{Name: "update", Summary: "Update issue fields", GroupID: "operations",
			Run: r.appCmd(appAccessWrite, runUpdate)},
		{Name: "rank", Summary: "Reorder an issue's rank", GroupID: "operations",
			Run: r.appCmd(appAccessWrite, runRank)},
		{Name: "start", Summary: "Claim issue work", GroupID: "operations",
			Run: r.transitionCmd("start")},
		{Name: "done", Summary: "Finish claimed work (success path; requires in_progress)", GroupID: "operations",
			Run: r.transitionCmd("done")},
		{Name: "close", Summary: "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)", GroupID: "operations",
			Run: r.transitionCmd("close")},
		{Name: "open", Summary: "Reopen issue(s)", GroupID: "operations",
			Run: r.transitionCmd("reopen")},
		{Name: "archive", Summary: "Archive issue(s)", GroupID: "operations",
			Run: r.transitionCmd("archive")},
		{Name: "delete", Summary: "Delete issue(s)", GroupID: "operations",
			Run: r.transitionCmd("delete")},
		{Name: "unarchive", Summary: "Unarchive issue(s)", GroupID: "operations",
			Run: r.transitionCmd("unarchive")},
		{Name: "restore", Summary: "Restore deleted issue(s)", GroupID: "operations",
			Run: r.transitionCmd("restore")},
		{Name: "comment", Summary: "Add issue comments", GroupID: "operations",
			Run: withValidation(validateCommentCommandPath, r.appCmd(appAccessWrite, runComment))},
		{Name: "label", Summary: "Manage labels", GroupID: "operations",
			Run: withValidation(validateLabelCommandPath, r.appCmd(appAccessWrite, runLabel))},
		{Name: "parent", Summary: "Manage parent relationships", GroupID: "structure",
			Run: withValidation(validateParentCommandPath, r.appCmd(appAccessWrite, runParent))},
		{Name: "children", Summary: "List child issues by rank", GroupID: "structure",
			Run: r.appCmd(appAccessRead, runChildren)},
		{Name: "dep", Summary: "Manage dependency edges", GroupID: "structure",
			Run: withValidation(validateDepCommandPath, r.appCmdDynamic(depAccess, runDep))},
		{Name: "export", Summary: "Export workspace snapshot", GroupID: "data",
			Run: r.appCmd(appAccessRead, runExport)},
		{Name: "import", Summary: "Bulk-create issues from a JSON tree spec", GroupID: "data",
			Run: r.appCmd(appAccessWrite, runImportTree)},
		{Name: "workspace", Summary: "Show workspace metadata", GroupID: "maintenance",
			Run: r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runWorkspace(stdout, ws, args)
			})},
		{Name: "doctor", Summary: "Health check", GroupID: "maintenance",
			Run: r.appCmdDynamic(resolveDoctorAccessMode, runDoctor)},
		{Name: "backup", Summary: "Backup snapshot operations", GroupID: "data",
			Run: withValidation(validateBackupCommandPath, r.appCmdDynamic(backupAccess, runBackup))},
		{Name: "recover", Summary: "Recover from backup or sync", GroupID: "data",
			Run: r.appCmd(appAccessWrite, runRecover)},
		{Name: "bulk", Summary: "Bulk issue operations", GroupID: "operations",
			Run: withValidation(validateBulkCommandPath, r.appCmd(appAccessWrite, runBulk))},
	}
}

// applyRegistry installs every group and command from the registry on root.
// The loop is uniform: every spec runs through the same code path.
func applyRegistry(root *cobra.Command, groups []GroupSpec, specs []CommandSpec) {
	for _, group := range groups {
		root.AddGroup(&cobra.Group{ID: group.ID, Title: group.Title})
	}
	for _, spec := range specs {
		root.AddCommand(buildPassthroughCommand(spec))
	}
}

// buildPassthroughCommand turns a spec row into a cobra command. The Long help
// is read from the spec; commands without a Long fall back to agentCommandHelp.
func buildPassthroughCommand(spec CommandSpec) *cobra.Command {
	long := spec.Long
	if long == "" {
		long = agentCommandHelp
	}
	return &cobra.Command{
		Use:                spec.Name,
		Short:              spec.Summary,
		Long:               long,
		GroupID:            spec.GroupID,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return spec.Run(args)
		},
	}
}
