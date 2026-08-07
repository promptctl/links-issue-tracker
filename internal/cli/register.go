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
	// Hidden keeps a real, dispatchable command out of the advertised surface —
	// root `--help` and the shell-completion projection — without removing it
	// from dispatch. A retired command stays invocable so it answers with a
	// documented pointer rather than cobra's bare "unknown command", but the
	// curated surface no longer lists it. Visibility is a typed property of the
	// spec, declared once here and read by both the cobra registration and the
	// completion model, so the two cannot disagree. [LAW:one-source-of-truth]
	Hidden bool
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
	// Issue Retention holds the admin quartet (archive/unarchive/delete/restore) —
	// the RetentionAction axis of the transition sum, distinct from the core
	// status lifecycle (start/done/close/open). It sits low so the high-traffic
	// lifecycle verbs stand out in Agent Operations rather than being crowded by
	// the rare retention verbs. [LAW:one-type-per-behavior] the group boundary
	// mirrors the model's StatusAction/RetentionAction partition, not an ad-hoc
	// visual grouping.
	{ID: "retention", Title: "Issue Retention"},
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
	// skipApp runs the handler WITHOUT opening a workspace. A retired subcommand
	// answers with its documented pointer, which must be reachable from anywhere —
	// like the top-level retirements — not gated behind a cwd-workspace open that
	// fails outside a git repo. When set, familyCmd calls run with a nil app.
	// [LAW:dataflow-not-control-flow] "does this subcommand need an app" is data on
	// the row, not a branch on the subcommand name.
	skipApp bool
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
		// A no-app subcommand (a retirement pointer) runs before any workspace
		// open, so its message reaches the caller even outside a git repo — the
		// break is reachable, never masked by a workspace error. [LAW:no-silent-failure]
		if sub.skipApp {
			return sub.run(r.ctx, r.stdout, nil, args[1:])
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
		// ready and queue are retired: next (one leaf) and backlog (full ranked
		// queue, blocked inline) are the only named workable views. Kept as hidden,
		// dispatchable specs so an old invocation gets the documented pointer, not
		// cobra's bare unknown-command error. [LAW:no-silent-failure]
		{Name: "ready", Summary: "(retired) use `lit backlog` or `lit next`", GroupID: "operations", Hidden: true,
			Run: retiredCommandRun("ready", workableRetirementGuidance)},
		{Name: "backlog", Summary: "List the full workable backlog in priority/rank order (blocked items inline)", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(backlogView))},
		{Name: "queue", Summary: "(retired) use `lit backlog` or `lit next`", GroupID: "operations", Hidden: true,
			Run: retiredCommandRun("queue", workableRetirementGuidance)},
		{Name: "next", Summary: "Print the next workable leaf to lit start", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableRun(nextView))},
		{Name: "orphaned", Summary: "List in_progress issues with no recent updates", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runOrphaned)},
		// ls is a raw runner (not appCmd) because `--at <store-dir>` points it at a
		// foreign store by path and must work outside the current workspace; the
		// standard appCmd wrapper would open the cwd store before the handler runs.
		// runList picks the store, then shares one query path. [LAW:one-source-of-truth]
		{Name: "ls", Summary: "List issues (rank by default; --at <store-dir> lists a discovered store read-only)", GroupID: "operations",
			Run: func(args []string) error { return runList(ctx, stdout, args) }},
		{Name: "show", Summary: "Show issue details", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runShow)},
		{Name: "history", Summary: "Show an issue's state-transition history", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, runHistory)},
		{Name: "update", Summary: "Update issue fields", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runUpdate)},
		{Name: "rank", Summary: "Reorder an issue's rank", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, runRank)},
		{Name: "start", Summary: "Claim issue work", GroupID: "operations",
			Run: r.transitionCmd(startSpec)},
		// assign is retired: reassigning is a single-field write folded into
		// `lit update --assignee`. Hidden+dispatchable so an old invocation gets the
		// documented pointer, not cobra's unknown-command error. [LAW:no-silent-failure]
		{Name: "assign", Summary: "(retired) use `lit update <id> --assignee <name>`", GroupID: "operations", Hidden: true,
			Run: retiredCommandRun("assign", assignRetirementGuidance)},
		{Name: "done", Summary: "Finish claimed work (success path; requires in_progress)", GroupID: "operations",
			Run: r.transitionCmd(doneSpec)},
		{Name: "close", Summary: "Close without finishing (wontfix / obsolete / duplicate; from any non-closed state)", GroupID: "operations",
			Run: r.transitionCmd(closeSpec)},
		{Name: "open", Summary: "Reopen issue(s)", GroupID: "operations",
			Run: r.transitionCmd(openSpec)},
		// Retention quartet: distinct RetentionAction transitions, grouped apart
		// from the status lifecycle so the core verbs stay prominent. Each stays a
		// first-class command — moved in help, unchanged in dispatch.
		{Name: "archive", Summary: "Archive issue(s)", GroupID: "retention",
			Run: r.transitionCmd(archiveSpec)},
		{Name: "unarchive", Summary: "Unarchive issue(s)", GroupID: "retention",
			Run: r.transitionCmd(unarchiveSpec)},
		{Name: "delete", Summary: "Delete issue(s)", GroupID: "retention",
			Run: r.transitionCmd(deleteSpec)},
		{Name: "restore", Summary: "Restore deleted issue(s)", GroupID: "retention",
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
		// export/backup/snapshots are three snapshot-shaped names over two distinct
		// mechanisms; the summaries below name the mechanism so a reader can tell the
		// JSON data-export family (export → backup) from the Dolt filesystem/database
		// snapshots (snapshots). The mechanisms are deliberately NOT merged.
		{Name: "export", Summary: "Write the backlog out as a portable JSON tree (the data-export primitive; `import`'s inverse)", GroupID: "data",
			Run: r.appCmd(app.AccessRead, runExport)},
		{Name: "import", Summary: "Bulk-create/update issues from a file (the one bulk-ingest home): a JSON tree spec, or a YAML file for create-or-update by id selector", GroupID: "data",
			Run: r.appCmd(app.AccessWrite, runImportTree)},
		{Name: "workspace", Summary: "Show workspace metadata", GroupID: "maintenance",
			Run: r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runWorkspace(stdout, ws, args)
			})},
		{Name: "stores", Summary: "List discovered lit store locations under the given roots (default: current directory); --counts reports each store's ready / in-flight / blocked counts instead. Readiness is store-intrinsic; per-repo required-fields policy is not applied, so counts can differ from a project's own `lit backlog` when it configures required_fields", GroupID: "maintenance",
			Run: func(args []string) error { return runStores(ctx, stdout, args) }},
		// ls-at is folded into `lit ls --at <store-dir>`; overview is folded into
		// `lit stores --counts`. Both kept hidden+dispatchable for the documented
		// pointer. [LAW:no-silent-failure]
		{Name: "ls-at", Summary: "(retired) use `lit ls --at <store-dir>`", GroupID: "maintenance", Hidden: true,
			Run: retiredCommandRun("ls-at", lsAtRetirementGuidance)},
		{Name: "overview", Summary: "(retired) use `lit stores --counts`", GroupID: "maintenance", Hidden: true,
			Run: retiredCommandRun("overview", overviewRetirementGuidance)},
		{Name: "prefix", Summary: "Manage the cosmetic issue ID prefix", GroupID: "maintenance",
			Run: r.wsCmd(func(_ context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
				return runPrefix(stdout, ws, args)
			})},
		{Name: "doctor", Summary: "Health check", GroupID: "maintenance",
			Run: r.appCmdDynamic(resolveDoctorAccessMode, runDoctor)},
		{Name: "backup", Summary: "Rotating JSON data-export backups — create/list/restore (wraps `export`; the data-recovery family, distinct from `snapshots`)", GroupID: "data",
			Run: r.familyCmd(backupFamily), Subcommands: backupFamily.visibleSubcommands()},
		{Name: "snapshots", Summary: "Dolt filesystem-level database snapshots — new/list/restore (the whole-database mechanism, distinct from JSON `backup`)", GroupID: "data",
			Run: r.wsFamilyCmd(snapshotsFamily), Subcommands: snapshotsFamily.visibleSubcommands()},
		{Name: "lifeboat", Summary: "Below-the-gate data recovery: dump a workspace's raw contents at any schema version, or recover it to a clean rebuild", GroupID: "maintenance",
			Run: r.wsFamilyCmd(lifeboatFamily), Subcommands: lifeboatFamily.visibleSubcommands()},
		{Name: "downgrade", Summary: "Reverse schema migrations and atomically install a prior lit binary", GroupID: "maintenance",
			Run: r.appCmd(app.AccessWrite, runDowngrade)},
		{Name: "upgrade", Summary: "Atomically install a newer lit binary to operate a workspace whose schema is ahead of this one", GroupID: "maintenance",
			Run: r.wsCmd(runUpgrade)},
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
		Hidden:             spec.Hidden,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return spec.Run(args)
		},
	}
}

// workableRetirementGuidance names where the retired workable views' intent now
// lives, so `lit ready` and `lit queue` both point the caller at the curated
// surface. [LAW:one-source-of-truth] one pointer, shared by both retirements.
const workableRetirementGuidance = "use `lit backlog` for the full ranked queue (blocked items shown inline) or `lit next` for the single leaf to start"

// Retirement pointers for the single-purpose commands folded into flags in this
// pass. Each names the surviving flag so a stale invocation is redirected, never
// silently broken. [LAW:no-silent-failure] The intent stays reachable; only the
// standalone verb is gone.
const (
	assignRetirementGuidance     = "reassigning is a field write: use `lit update <id> --assignee <name>` (with an optional `--reason`)"
	lsAtRetirementGuidance       = "use `lit ls --at <store-dir>` — listing a discovered store read-only is now a flag on `ls`, not a separate command"
	overviewRetirementGuidance   = "use `lit stores --counts` — the cross-project ready / in-flight / blocked rollup is now a flag on `stores`"
	bulkImportRetirementGuidance = "use `lit backup restore --path <export.json>` — it owns the same export-restore mechanism `bulk import` duplicated"
)

// retiredCommandRun builds the handler for a command retired from the surface:
// it runs nothing and returns a RetiredCommandError naming its replacement. The
// command stays registered (Hidden) so the invocation yields this documented
// pointer instead of cobra's bare unknown-command error — the break is
// deliberate and explained, never silent. [LAW:no-silent-failure]
func retiredCommandRun(command, replacement string) CommandRunner {
	return func([]string) error {
		return RetiredCommandError{Command: command, Replacement: replacement}
	}
}
