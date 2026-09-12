package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
)

// allPositionals lets every non-flag token reach a leaf's positionals, for the
// leaves whose argument list has no fixed arity (`rank set` takes a whole id
// sequence). splitArgs treats the count as a ceiling, so this admits any number
// rather than demanding one. [LAW:types-are-the-program] the unbounded arity is
// stated in the leaf's own declaration, not left for the body to re-derive.
const allPositionals = math.MaxInt

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
	// Retired marks a row kept only as a pointer: the command still dispatches
	// (a stale invocation gets its documented replacement, not cobra's
	// unknown-command noise) but it is withdrawn from the taught surface, so
	// shipped templates must not name it. Distinct from Hidden, which conceals
	// a live command without withdrawing it — the template dispatch gate reads
	// this field, never Hidden and never exit codes, because a retired command
	// exits 3 only at runtime while the gate runs at build time.
	// [LAW:types-are-the-program] retirement is a stated property of the spec,
	// not an inference from which runner it happens to hold. retiredSpec is
	// the one authoring path; the type cannot refuse a same-package literal
	// that states this field alone, so the registry coherence test is what
	// holds every retired row to all three retirement facets.
	Retired bool
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
	// nestedUsage, when non-empty, marks this subcommand as itself a command
	// group and carries that group's usage — always set by reference to the
	// nested family's own usage field, never restated. [LAW:one-source-of-truth]
	// It lets the OUTER resolve answer `parent sub --help` itself: the dispatch
	// pipeline between the outer resolve and the nested family's own resolve
	// acquires the workspace, store, or app the real subcommand needs, and a
	// help request must acquire nothing — under store contention the nested
	// resolve would otherwise be reached only after a blocking open, or never.
	nestedUsage string
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
// The one flag-shaped exception is a help request, which is an answer, not a
// failure: the family's usage IS its help, and recognizing -h/--help here — at
// the one resolve every family, nested families included, routes through —
// means no group can answer help with an error. [LAW:single-enforcer]
// The match is exact — argv tokens arrive verbatim from the shell, and a
// table that trimmed names would claim inputs as legal that no dispatch
// ever honored. [FRAMING:representation]
func (f commandFamily[P]) resolve(args []string) (P, error) {
	var zero P
	if len(args) == 0 {
		return zero, UsageError{Message: f.usage}
	}
	if isHelpFlag(args[0]) {
		return zero, HelpRequestedError{Usage: f.usage}
	}
	for _, s := range f.subcommands {
		if s.name == args[0] {
			if s.nestedUsage != "" && len(args) > 1 && isHelpFlag(args[1]) {
				return zero, HelpRequestedError{Usage: s.nestedUsage}
			}
			return s.payload, nil
		}
	}
	// [LAW:types-are-the-program] a typed usage refusal, not errors.New: the
	// untyped form fell through every errors.As sink to the generic
	// "command_failed" reason, whose retry-then-doctor remediation can never
	// succeed for a bad path (links-cli-zc3r).
	return zero, UsageError{Message: f.usage}
}

// isHelpFlag is the one definition of what a help-shaped argv token is; both
// recognition points in resolve read it. [LAW:one-source-of-truth]
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help"
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
// subcommand needs and the leaf that runs once the app is open in that
// mode. One row answers legality, access, and dispatch together, so the
// three can never disagree. [LAW:one-source-of-truth]
type appSubcommand struct {
	access  app.AccessMode
	declare appLeafFn
	// retired, when non-nil, is the answer a withdrawn subcommand gives every
	// invocation. A retirement pointer must be reachable from anywhere — like
	// the top-level retirements — so a row carrying one dispatches to the
	// pointer and never to a leaf, ahead of any flag parse or workspace open.
	// [LAW:dataflow-not-control-flow] retirement is data on the row, not a
	// handler that happens to ignore its app. The whole answer is carried, not
	// just its guidance half: the invoked path the message names is the family's
	// name joined to the row's, which resolve's argv cannot supply — args[0] is
	// the bare subcommand token, so an error built from it would tell a caller
	// who typed `lit bulk import` that "import" was retired.
	retired *RetiredCommandError
}

// retiredSubcommand builds the whole row for a subcommand retired from a
// family's surface, mirroring retiredSpec at the top level: the path the error
// names is this call's own two arguments joined, and the row is hidden so the
// family's advertised surface — usage, help, completion — no longer lists it.
// [LAW:single-enforcer] one authoring path for a subcommand retirement, so a
// row cannot state the pointer without also leaving the surface.
func retiredSubcommand(family, name, replacement string) subcommandRow[appSubcommand] {
	return subcommandRow[appSubcommand]{
		name:   name,
		hidden: true,
		payload: appSubcommand{retired: &RetiredCommandError{
			Command:     family + " " + name,
			Replacement: replacement,
		}},
	}
}

// leaf is a leaf command's two phases, split at the line acquisition must not
// cross. fs and positionals are the DECLARATION — the flag surface, plus how
// many leading argv tokens are the command's own positionals rather than flag
// values — and work is everything that needs the acquired resource R. Building
// this value opens nothing, which is precisely what lets the pipeline below
// render help, or reject a bad flag, with no workspace, store, or app.
//
// [LAW:decomposition] The two phases used to be fused inside each handler body:
// the handler declared its flags and parsed them, so the only way to reach the
// flag surface was to run the handler, and the only way to run the handler was
// through an acquisition help never needed (links-cli-1lxr).
// [LAW:one-type-per-behavior] app-mode and workspace-mode leaves differ only in
// which resource their work takes, so they are one type over R, not two.
type leaf[R any] struct {
	fs          *cobraFlagSet
	positionals int
	work        func(ctx context.Context, stdout io.Writer, res R, positional []string) error
}

// The two resources a leaf's work can need. A declaration function returns one
// of these; the pipeline holds it only after declaring, and calls its work only
// after acquiring. [LAW:types-are-the-program] the ordering the handlers used to
// carry as a convention is now the only order these types can be used in.
type (
	appLeaf   = leaf[*app.App]
	wsLeaf    = leaf[workspace.Info]
	appLeafFn = func() appLeaf
	wsLeafFn  = func() wsLeaf
)

// parseLeaf runs the whole pre-acquisition phase: split argv into the leaf's
// positionals and its flag tokens, then parse. A help request is answered right
// here — by the same parse every leaf already used — and comes back as
// errHelpHandled, which Run maps to exit 0, so every caller below returns before
// acquiring anything. [LAW:single-enforcer] one parse path for every leaf, at
// the one altitude that precedes acquisition.
func parseLeaf[R any](l leaf[R], args []string, stdout io.Writer) ([]string, error) {
	positional, flagArgs := splitArgs(args, l.positionals)
	if err := parseFlagSet(l.fs, flagArgs, stdout); err != nil {
		return nil, err
	}
	return positional, nil
}

// commandRegistrar carries the entrypoint context shared by every spec's Run
// closure. Building specs through these methods absorbs the per-call variance
// (closure capture + access mode + validation) into data.
type commandRegistrar struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
}

func (r *commandRegistrar) appCmd(access app.AccessMode, declare appLeafFn) CommandRunner {
	return r.appCmdDynamic(func([]string) app.AccessMode { return access }, declare)
}

func (r *commandRegistrar) appCmdDynamic(resolve func([]string) app.AccessMode, declare appLeafFn) CommandRunner {
	return r.appCmdPipeline(resolve, func(args []string) (appLeaf, []string) { return declare(), args })
}

// appCmdDispatch is appCmd for a command whose flag surface depends on a leading
// subcommand token — `lit rank <id> --top` and `lit rank set <id>...` are two
// surfaces under one name. The token picks a leaf VALUE from argv alone, with
// nothing open, so either surface answers help before acquisition.
// [LAW:dataflow-not-control-flow] the token selects a value; it does not fork
// the pipeline.
func (r *commandRegistrar) appCmdDispatch(access app.AccessMode, dispatch func(args []string) (appLeaf, []string)) CommandRunner {
	return r.appCmdPipeline(func([]string) app.AccessMode { return access }, dispatch)
}

// appCmdPipeline seals the declare→parse→open→work ordering for every app-mode
// leaf; the three entrypoints above are specializations that differ only in how
// the access mode and the leaf are chosen. Declaration and parse precede the
// open, so a help request — and a malformed flag, equally a question the
// workspace has no part in — is answered without one.
// [LAW:no-ambient-temporal-coupling] the ordering has one owner here instead of
// being whatever order each handler body happened to write it in.
// [LAW:single-enforcer] one pipeline, so no entrypoint can acquire earlier than
// another.
func (r *commandRegistrar) appCmdPipeline(resolve func([]string) app.AccessMode, dispatch func(args []string) (appLeaf, []string)) CommandRunner {
	return func(args []string) error {
		l, rest := dispatch(args)
		positional, err := parseLeaf(l, rest, r.stdout)
		if err != nil {
			return err
		}
		return runWithApp(r.ctx, r.stdout, resolve(args), func(commandCtx context.Context, ap *app.App) error {
			return l.work(commandCtx, r.stdout, ap, positional)
		})
	}
}

// familyCmd seals the same pipeline for an app-mode subcommand family: the
// table yields the row (or rejects the path), the row's leaf declares and
// parses, and only then does the app open in the row's access mode.
func (r *commandRegistrar) familyCmd(f commandFamily[appSubcommand]) CommandRunner {
	return func(args []string) error {
		sub, err := f.resolve(args)
		if err != nil {
			return err
		}
		// A retired subcommand answers before any parse or workspace open, so its
		// pointer reaches the caller even outside a git repo — the break is
		// reachable, never masked by a workspace error. [LAW:no-silent-failure]
		if sub.retired != nil {
			return *sub.retired
		}
		l := sub.declare()
		positional, err := parseLeaf(l, args[1:], r.stdout)
		if err != nil {
			return err
		}
		return runWithApp(r.ctx, r.stdout, sub.access, func(commandCtx context.Context, ap *app.App) error {
			return l.work(commandCtx, r.stdout, ap, positional)
		})
	}
}

// wsSubcommand is one workspace-family row: either the leaf that serves it, or a
// nested family that resolves one more argv token first. A nested family may
// also name the leaf for its BARE path — `lit sync reconcile` with no
// subcommand is the reconcile action itself, not a usage error.
// [LAW:types-are-the-program] Go has no sum type to say "leaf xor family", so
// resolveWsLeaf reads nested first and this table is the only author of either.
type wsSubcommand struct {
	declare wsLeafFn
	nested  *commandFamily[wsSubcommand]
	bare    wsLeafFn
}

// resolveWsLeaf walks a workspace family — to any depth — down to the leaf the
// argv names, returning it with the argv left for that leaf to parse. The whole
// walk is table lookups and pure declarations, so a help request nested two
// families deep (`lit sync reconcile take --help`) is still resolved, declared,
// and answered with no workspace, sync store, or app open. [LAW:one-way-deps]
// resolution flows strictly downward through the tables; no level reaches back
// for a resource to decide the next.
func resolveWsLeaf(f commandFamily[wsSubcommand], args []string) (wsLeaf, []string, error) {
	sub, err := f.resolve(args)
	if err != nil {
		return wsLeaf{}, nil, err
	}
	rest := args[1:]
	if sub.nested == nil {
		return sub.declare(), rest, nil
	}
	// A bare path is the nested family's default action, selected by the absence
	// of a subcommand name rather than by a flag. [LAW:dataflow-not-control-flow]
	if sub.bare != nil && (len(rest) == 0 || strings.HasPrefix(rest[0], "-")) {
		return sub.bare(), rest, nil
	}
	return resolveWsLeaf(*sub.nested, rest)
}

// wsCmdPipeline is appCmdPipeline's workspace-mode twin: it seals the
// dispatch→parse→resolve-workspace→work ordering for every workspace command,
// and the entrypoints below differ only in how argv chooses the leaf.
// [LAW:no-ambient-temporal-coupling] Usage failures and help must surface even
// outside a git repository, so dispatch and parse precede the workspace lookup
// here rather than relying on each caller to order them.
// [LAW:single-enforcer] one pipeline, so no entrypoint can acquire earlier than
// another.
func (r *commandRegistrar) wsCmdPipeline(dispatch func(args []string) (wsLeaf, []string, error)) CommandRunner {
	return func(args []string) error {
		l, rest, err := dispatch(args)
		if err != nil {
			return err
		}
		positional, err := parseLeaf(l, rest, r.stdout)
		if err != nil {
			return err
		}
		return runWithWorkspace(func(ws workspace.Info) error {
			return l.work(r.ctx, r.stdout, ws, positional)
		})
	}
}

// wsFamilyCmd is familyCmd for workspace-mode families: the family table
// rejects bad paths, and the leaf it yields declares the surface help answers.
func (r *commandRegistrar) wsFamilyCmd(f commandFamily[wsSubcommand]) CommandRunner {
	return r.wsCmdPipeline(func(args []string) (wsLeaf, []string, error) { return resolveWsLeaf(f, args) })
}

func (r *commandRegistrar) wsCmd(declare wsLeafFn) CommandRunner {
	return r.wsCmdPipeline(func(args []string) (wsLeaf, []string, error) { return declare(), args, nil })
}

func (r *commandRegistrar) transitionCmd(spec transitionSpec) CommandRunner {
	return r.appCmd(app.AccessWrite, func() appLeaf { return transitionLeaf(spec) })
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
			Run: r.wsCmd(initLeaf)},
		{Name: "quickstart", Summary: "Agent quickstart workflow", GroupID: "guidance",
			Run: r.wsCmd(quickstartLeaf)},
		{Name: "workflows", Summary: "See the work lifecycle and the guidance active at each point (`workflows show <id>` resolved, `edit <id-or-point>` to customize, `dry-run` to explain a hypothetical)", GroupID: "guidance",
			Run: r.wsCmdPipeline(workflowsDispatch), Subcommands: workflowsFamily.visibleSubcommands()},
		{Name: "completion", Summary: "Generate shell completion script", GroupID: "guidance",
			Run: completionRun, Subcommands: completionFamily.visibleSubcommands()},
		{Name: "version", Summary: "Print binary version, build metadata, and supported schema range", GroupID: "guidance",
			Run: versionRun},
		{Name: "hooks", Summary: "Install git hook automation", GroupID: "maintenance",
			Run: r.wsFamilyCmd(hooksFamily), Subcommands: hooksFamily.visibleSubcommands()},
		{Name: "sync", Summary: "Mirror Dolt data through git remotes", GroupID: "data",
			Run: r.wsFamilyCmd(syncFamily), Subcommands: syncSubcommands},
		{Name: "new", Summary: "Create an issue", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, newLeaf)},
		{Name: "followup", Summary: "File a follow-up issue parented to a just-closed ticket", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, followupLeaf)},
		// ready and queue are retired: next (one leaf) and backlog (the ranked
		// queue, blocked inline) are the only named workable views. Kept as hidden,
		// dispatchable specs so an old invocation gets the documented pointer, not
		// cobra's bare unknown-command error. [LAW:no-silent-failure]
		retiredSpec("ready", "operations", "use `lit backlog` or `lit next`", workableRetirementGuidance),
		{Name: "backlog", Summary: "List the workable backlog in priority/rank order (blocked items inline)", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, workableLeafFn(backlogView))},
		retiredSpec("queue", "operations", "use `lit backlog` or `lit next`", workableRetirementGuidance),
		{Name: "next", Summary: "Print the next workable leaf to lit start", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, nextLeaf)},
		{Name: "orphaned", Summary: "List in_progress issues with no recent updates", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, orphanedLeaf)},
		// ls is a raw runner (not appCmd) because `--at <store-dir>` points it at a
		// foreign store by path and must work outside the current workspace; the
		// standard appCmd wrapper would open the cwd store before the handler runs.
		// runList picks the store, then shares one query path. [LAW:one-source-of-truth]
		{Name: "ls", Summary: "List issues (rank by default; --at <store-dir> lists a discovered store read-only)", GroupID: "operations",
			Run: func(args []string) error { return runList(ctx, stdout, args) }},
		{Name: "show", Summary: "Show issue details", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, showLeaf)},
		{Name: "history", Summary: "Show an issue's state-transition history", GroupID: "operations",
			Run: r.appCmd(app.AccessRead, historyLeaf)},
		{Name: "update", Summary: "Update issue fields", GroupID: "operations",
			Run: r.appCmd(app.AccessWrite, updateLeaf)},
		{Name: "rank", Summary: "Reorder an issue's rank", GroupID: "operations",
			Run: r.appCmdDispatch(app.AccessWrite, rankDispatch), Subcommands: []SubcommandSpec{{Name: rankSetSubcommand}}},
		{Name: "start", Summary: "Claim issue work", GroupID: "operations",
			Run: r.transitionCmd(startSpec)},
		// assign is retired: reassigning is a single-field write folded into
		// `lit update --assignee`. Hidden+dispatchable so an old invocation gets the
		// documented pointer, not cobra's unknown-command error. [LAW:no-silent-failure]
		retiredSpec("assign", "operations", "use `lit update <id> --assignee <name>`", assignRetirementGuidance),
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
			Run: r.appCmd(app.AccessRead, childrenLeaf)},
		{Name: "dep", Summary: "Manage dependency edges", GroupID: "structure",
			Run: r.familyCmd(depFamily), Subcommands: depFamily.visibleSubcommands()},
		// export/backup/snapshots are three snapshot-shaped names over two distinct
		// mechanisms; the summaries below name the mechanism so a reader can tell the
		// JSON data-export family (export → backup) from the Dolt filesystem/database
		// snapshots (snapshots). The mechanisms are deliberately NOT merged.
		{Name: "export", Summary: "Write the backlog out as a portable JSON tree (the data-export primitive; `import`'s inverse)", GroupID: "data",
			Run: r.appCmd(app.AccessRead, exportLeaf)},
		{Name: "import", Summary: "Bulk-create/update issues from a file (the one bulk-ingest home): a JSON tree spec, or a YAML file for create-or-update by id selector", GroupID: "data",
			Run: r.appCmd(app.AccessWrite, importTreeLeaf)},
		{Name: "workspace", Summary: "Show workspace metadata", GroupID: "maintenance",
			Run: r.wsCmd(workspaceLeaf)},
		{Name: "stores", Summary: "List discovered lit store locations under the given roots (default: current directory); --counts reports each store's ready / in-flight / blocked counts instead. Readiness is store-intrinsic; per-repo required-fields policy is not applied, so counts can differ from a project's own `lit backlog` when it configures required_fields", GroupID: "maintenance",
			Run: func(args []string) error { return runStores(ctx, stdout, args) }},
		// ls-at is folded into `lit ls --at <store-dir>`; overview is folded into
		// `lit stores --counts`. Both kept hidden+dispatchable for the documented
		// pointer. [LAW:no-silent-failure]
		retiredSpec("ls-at", "maintenance", "use `lit ls --at <store-dir>`", lsAtRetirementGuidance),
		retiredSpec("overview", "maintenance", "use `lit stores --counts`", overviewRetirementGuidance),
		{Name: "prefix", Summary: "Manage the cosmetic issue ID prefix", GroupID: "maintenance",
			Run: r.wsFamilyCmd(prefixFamily), Subcommands: prefixFamily.visibleSubcommands()},
		{Name: "doctor", Summary: "Health check", GroupID: "maintenance",
			Run: r.appCmdDynamic(resolveDoctorAccessMode, doctorLeaf)},
		{Name: "backup", Summary: "Rotating JSON data-export backups — create/list/restore (wraps `export`; the data-recovery family, distinct from `snapshots`)", GroupID: "data",
			Run: r.familyCmd(backupFamily), Subcommands: backupFamily.visibleSubcommands()},
		{Name: "snapshots", Summary: "Dolt filesystem-level database snapshots — new/list/restore (the whole-database mechanism, distinct from JSON `backup`)", GroupID: "data",
			Run: r.wsFamilyCmd(snapshotsFamily), Subcommands: snapshotsFamily.visibleSubcommands()},
		{Name: "lifeboat", Summary: "Below-the-gate data recovery: dump a workspace's raw contents at any schema version, or recover it to a clean rebuild", GroupID: "maintenance",
			Run: r.wsFamilyCmd(lifeboatFamily), Subcommands: lifeboatFamily.visibleSubcommands()},
		{Name: "downgrade", Summary: "Reverse schema migrations and atomically install a prior lit binary", GroupID: "maintenance",
			Run: r.appCmd(app.AccessWrite, downgradeLeaf)},
		{Name: "upgrade", Summary: "Atomically install a newer lit binary to operate a workspace whose schema is ahead of this one", GroupID: "maintenance",
			Run: r.wsCmd(upgradeLeaf)},
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
const workableRetirementGuidance = "use `lit backlog` for the ranked queue (blocked items shown inline) or `lit next` for the single leaf to start"

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

// retiredSpec builds the whole registry row for a retired command. Retirement
// has three inseparable facets — hidden from the advertised surface, marked
// Retired for the template dispatch gate, dispatchable only to a pointer at
// its replacement — assembled here and nowhere else. [LAW:single-enforcer] Go
// cannot stop a same-package literal from stating one facet without the
// others, so the registry coherence test enforces the coupling the type
// cannot. The summary is the short pointer shown to a reader browsing hidden
// help; replacement is the full guidance the runner returns on invocation.
func retiredSpec(name, groupID, summary, replacement string) CommandSpec {
	return CommandSpec{
		Name:    name,
		Summary: "(retired) " + summary,
		GroupID: groupID,
		Hidden:  true,
		Retired: true,
		Run:     retiredCommandRun(name, replacement),
	}
}

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
