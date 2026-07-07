package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
	// Subcommands is the visible first-argument tree for a family command (nil
	// for a leaf command). It is the registry's authoritative answer to "what
	// can follow this command", which the shell-completion projection reads so
	// the scripts cannot enumerate a subcommand the registry doesn't.
	// [LAW:one-source-of-truth]
	Subcommands []SubcommandSpec
}

// SubcommandSpec is one legal first-argument name plus its own nested tree
// (e.g. `sync remote` carries `ls`). Names here are derived from the owning
// commandFamily table, never restated. [LAW:one-source-of-truth]
type SubcommandSpec struct {
	Name        string
	Subcommands []SubcommandSpec
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

// subcommandRow pairs one legal subcommand name with whatever that family's
// rows carry: access+handler for app families, a handler for workspace
// families, a completion script for the completion family. The routing
// behavior is identical across families, so it is written once and the
// variability lives in the payload value. [LAW:one-type-per-behavior]
type subcommandRow[P any] struct {
	name    string
	payload P
	// hidden keeps a real, dispatchable subcommand out of the advertised
	// surface (usage, help, completion) without removing it from resolve. The
	// background mirror entrypoint is the only such row. Visibility is a typed
	// property here, not a name omitted from a usage string by convention.
	// [LAW:types-are-the-program]
	hidden bool
}

// commandFamily is the single source of truth for a subcommand family: which
// first arguments are legal and what each one means.
// [LAW:one-source-of-truth] The former per-family path validators, the
// args[0] string tests selecting read vs write, and the per-family dispatch
// switches were three drifting copies of this table; each repeated the usage
// string and the legal-name set independently.
type commandFamily[P any] struct {
	usage       string
	subcommands []subcommandRow[P]
}

// resolve returns the payload of the subcommand named by args[0].
// Lookup is validation: a missing, unknown, or flag-shaped first argument
// fails with the family usage before any app opens, so resolution cannot
// depend on a validator having run earlier. [LAW:no-ambient-temporal-coupling]
// The match is exact — argv tokens arrive verbatim from the shell, and a
// table that trimmed names would claim inputs as legal that no dispatch
// ever honored. [FRAMING:representation]
func (f commandFamily[P]) resolve(args []string) (P, error) {
	var zero P
	if len(args) == 0 {
		return zero, errors.New(f.usage)
	}
	for _, s := range f.subcommands {
		if s.name == args[0] {
			return s.payload, nil
		}
	}
	return zero, errors.New(f.usage)
}

// visibleSubcommands projects the family's advertised first-argument names for
// completion. Hidden rows are dropped, so the projection cannot leak the
// background mirror, and the names come from the one table that resolve also
// reads — completion and dispatch can never disagree. [LAW:one-source-of-truth]
func (f commandFamily[P]) visibleSubcommands() []SubcommandSpec {
	subs := make([]SubcommandSpec, 0, len(f.subcommands))
	for _, s := range f.subcommands {
		if s.hidden {
			continue
		}
		subs = append(subs, SubcommandSpec{Name: s.name})
	}
	return subs
}

// nestUnder grafts a nested family's names beneath the subcommand named name,
// so a sub-subcommand surface (e.g. `sync remote ls`) is derived from that
// family rather than restated in the completion scripts. It panics when name is
// absent: the wiring topology and the family table must agree, and a silent
// miss would reintroduce exactly the drift this projection removes.
// [LAW:no-silent-failure]
func nestUnder(subs []SubcommandSpec, name string, children []SubcommandSpec) []SubcommandSpec {
	for i := range subs {
		if subs[i].Name == name {
			subs[i].Subcommands = children
			return subs
		}
	}
	panic(fmt.Sprintf("completion: no subcommand %q to nest under", name))
}

// appSubcommand is the row payload for app-mode families: the access the
// subcommand needs and the handler that runs once the app is open in that
// mode. One row answers legality, access, and dispatch together, so the
// three can never disagree. [LAW:one-source-of-truth]
type appSubcommand struct {
	access app.AccessMode
	run    appRunFn
}

// appRunFn is the canonical signature for app-mode handlers.
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

func (r *commandRegistrar) appCmd(access app.AccessMode, fn appRunFn) CommandRunner {
	return r.appCmdDynamic(func([]string) app.AccessMode { return access }, fn)
}

func (r *commandRegistrar) appCmdDynamic(resolve func([]string) app.AccessMode, fn appRunFn) CommandRunner {
	return func(args []string) error {
		return runWithApp(r.ctx, resolve(args), func(commandCtx context.Context, ap *app.App) error {
			return fn(commandCtx, r.stdout, ap, args)
		})
	}
}

// familyCmd seals the resolve→open→dispatch pipeline for an app-mode
// subcommand family: the table yields the row (or rejects the path), the app
// opens in the row's access mode, and the row's handler runs on the remaining
// arguments. Callers compose nothing; the ordering lives here.
func (r *commandRegistrar) familyCmd(f commandFamily[appSubcommand]) CommandRunner {
	return func(args []string) error {
		sub, err := f.resolve(args)
		if err != nil {
			return err
		}
		return runWithApp(r.ctx, sub.access, func(commandCtx context.Context, ap *app.App) error {
			return sub.run(commandCtx, r.stdout, ap, args[1:])
		})
	}
}

// wsFamilyCmd is familyCmd for workspace-mode families: resolve rejects bad
// paths before the workspace resolves, then the row's handler runs on the
// remaining arguments. [LAW:no-ambient-temporal-coupling] Usage failures must
// surface even outside a git repository, so resolution precedes workspace
// lookup here rather than relying on caller ordering.
func (r *commandRegistrar) wsFamilyCmd(f commandFamily[wsRunFn]) CommandRunner {
	return func(args []string) error {
		run, err := f.resolve(args)
		if err != nil {
			return err
		}
		return runWithWorkspace(func(ws workspace.Info) error {
			return run(r.ctx, r.stdout, ws, args[1:])
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

func (r *commandRegistrar) transitionCmd(spec transitionSpec) CommandRunner {
	return r.appCmd(app.AccessWrite, func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runTransition(ctx, stdout, ap, args, spec)
	})
}

// commandSpecs returns the full registry. New commands are added here as a
// single row; the runtime path in newRootCommand never grows.
func commandSpecs(ctx context.Context, stdout io.Writer, stderr io.Writer) []CommandSpec {
	r := &commandRegistrar{ctx: ctx, stdout: stdout, stderr: stderr}

	completionRun := func(args []string) error {
		return runCompletion(stdout, args)
	}

	versionRun := func(args []string) error {
		return runVersion(stdout, args)
	}

	// Nested family surfaces are grafted onto their parent subcommand so the
	// completion projection carries the full `sync remote ls`, `sync reconcile
	// <...>`, and `bulk label <...>` trees — every name still sourced from the
	// owning family table. [LAW:one-source-of-truth]
	syncSubcommands := nestUnder(syncFamily.visibleSubcommands(), "remote", syncRemoteFamily.visibleSubcommands())
	syncSubcommands = nestUnder(syncSubcommands, "reconcile", reconcileFamily.visibleSubcommands())
	bulkSubcommands := nestUnder(bulkFamily.visibleSubcommands(), "label", bulkLabelFamily.visibleSubcommands())

	return []CommandSpec{
		{Name: "init", Summary: "Initialize links", Long: humanBootstrapHelp, GroupID: "bootstrap",
			Run: r.wsCmd(runInit)},
		{Name: "quickstart", Summary: "Agent quickstart workflow", GroupID: "guidance",
			Run: r.wsCmd(runQuickstart)},
		{Name: "completion", Summary: "Generate shell completion script", GroupID: "guidance",
			Run: completionRun, Subcommands: completionFamily.visibleSubcommands()},
		{Name: "version", Summary: "Print binary version, build metadata, and supported schema range", GroupID: "guidance",
			Run: versionRun},
		{Name: "hooks", Summary: "Install git hook automation", GroupID: "maintenance",
			Run: r.wsFamilyCmd(hooksFamily), Subcommands: hooksFamily.visibleSubcommands()},
		{Name: "sync", Summary: "Mirror Dolt data through git remotes", GroupID: "data",
			Run: r.wsFamilyCmd(syncFamily), Subcommands: syncSubcommands},
		{Name: "new", Summary: "Create an issue", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runNew)},
		{Name: "followup", Summary: "File a follow-up issue parented to a just-closed ticket", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runFollowup)},
		{Name: "ready", Summary: "List open work by readiness and rank", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(readyView))},
		{Name: "backlog", Summary: "List the full workable backlog in priority/rank order (blocked items inline)", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(backlogView))},
		{Name: "queue", Summary: "List the rank-ordered pull sequence (pullable items only, terse)", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(queueView))},
		{Name: "next", Summary: "Print the next workable leaf to lit start", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(nextView))},
		{Name: "orphaned", Summary: "List in_progress issues with no recent updates", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runOrphaned)},
		{Name: "ls", Summary: "List issues (rank by default)", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runList)},
		{Name: "show", Summary: "Show issue details", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runShow)},
		{Name: "update", Summary: "Update issue fields", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runUpdate)},
		{Name: "rank", Summary: "Reorder an issue's rank", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runRank)},
		{Name: "start", Summary: "Claim issue work", GroupID: "operations",
			Run: r.transitionCmd(startSpec)},
		{Name: "assign", Summary: "Reassign an issue to a different agent (without changing status)", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runAssign)},
		{Name: "done", Summary: "Finish claimed work (success path; requires in_progress)", GroupID: "operations",
			Run: r.transitionCmd(doneSpec)},
		{Name: "close", Summary: "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)", GroupID: "operations",
			Run: r.transitionCmd(closeSpec)},
		{Name: "open", Summary: "Reopen issue(s)", GroupID: "operations",
			Run: r.transitionCmd(openSpec)},
		{Name: "archive", Summary: "Archive issue(s)", GroupID: "operations",
			Run: r.transitionCmd(archiveSpec)},
		{Name: "delete", Summary: "Delete issue(s)", GroupID: "operations",
			Run: r.transitionCmd(deleteSpec)},
		{Name: "unarchive", Summary: "Unarchive issue(s)", GroupID: "operations",
			Run: r.transitionCmd(unarchiveSpec)},
		{Name: "restore", Summary: "Restore deleted issue(s)", GroupID: "operations",
			Run: r.transitionCmd(restoreSpec)},
		{Name: "comment", Summary: "Add issue comments", GroupID: "operations",
			Run: r.familyCmd(commentFamily), Subcommands: commentFamily.visibleSubcommands()},
		{Name: "label", Summary: "Manage labels", GroupID: "operations",
			Run: r.familyCmd(labelFamily), Subcommands: labelFamily.visibleSubcommands()},
		{Name: "parent", Summary: "Manage parent relationships", GroupID: "structure",
			Run: r.familyCmd(parentFamily), Subcommands: parentFamily.visibleSubcommands()},
		{Name: "children", Summary: "List child issues by rank", GroupID: "structure",
			Run: r.appCmd(app.AccessRead, runChildren)},
		{Name: "dep", Summary: "Manage dependency edges", GroupID: "structure",
			Run: r.familyCmd(depFamily), Subcommands: depFamily.visibleSubcommands()},
		{Name: "export", Summary: "Export workspace snapshot", GroupID: "data",
			Run: r.appCmd(app.AccessRead, runExport)},
		{Name: "import", Summary: "Bulk-create issues from a JSON tree spec", GroupID: "data",
			Run: r.appCmd(app.AccessWrite, runImportTree)},
		{Name: "workspace", Summary: "Show workspace metadata", GroupID: "maintenance",
			Run: r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runWorkspace(stdout, ws, args)
			})},
		{Name: "prefix", Summary: "Manage the cosmetic issue ID prefix", GroupID: "maintenance",
			Run: r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runPrefix(stdout, ws, args)
			})},
		{Name: "doctor", Summary: "Health check", GroupID: "maintenance",
			Run: r.appCmdDynamic(resolveDoctorAccessMode, runDoctor)},
		{Name: "backup", Summary: "Backup snapshot operations", GroupID: "data",
			Run: r.familyCmd(backupFamily), Subcommands: backupFamily.visibleSubcommands()},
		{Name: "snapshots", Summary: "Filesystem-level workspace snapshots", GroupID: "data",
			Run: r.wsFamilyCmd(snapshotsFamily), Subcommands: snapshotsFamily.visibleSubcommands()},
		{Name: "lifeboat", Summary: "Below-the-gate data recovery: dump a workspace's raw contents at any schema version, or recover it to a clean rebuild", GroupID: "maintenance",
			Run: r.wsFamilyCmd(lifeboatFamily), Subcommands: lifeboatFamily.visibleSubcommands()},
		{Name: "downgrade", Summary: "Reverse schema migrations and atomically install a prior lit binary", GroupID: "maintenance",
			Run: r.appCmd(app.AccessWrite, runDowngrade)},
		{Name: "bulk", Summary: "Bulk issue operations", GroupID: "operations",
			Run: r.familyCmd(bulkFamily), Subcommands: bulkSubcommands},
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
