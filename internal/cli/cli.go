package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/interrupt"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/query"
	"github.com/promptctl/links-issue-tracker/internal/statusline"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Run is one lit invocation, and the one place every invocation passes
// through, so it is where each one's event opens and closes: the command
// running under it adds facts through ctx, and the finished event is written
// whether the command succeeded, failed, panicked, or was hard-exited by the
// interrupt guard, which never unwinds this stack and so is handed the
// recording to do itself.
// [LAW:nothing-unseen] [LAW:single-enforcer]
func Run(ctx context.Context, inv Invocation) (err error) {
	recorder := beginInvocation(time.Now(), inv.Dir)
	record := &invocationRecord{recorder: recorder, stderr: inv.Stderr, env: inv.Env, gitSpawnsBefore: inv.Env.GitSpawns()}
	interrupt.BeforeHardExit(ctx, func(signal string, code int) { record.close(interruptedEnd(time.Now(), signal, code)) })
	defer func() {
		panicked := recover()
		record.close(invocationEnd(time.Now(), err, panicked))
		if panicked != nil {
			panic(panicked)
		}
	}()
	// Every store lock this run takes records this run's command line as its
	// holder, not the process's, which one process running several runs shares.
	ctx = store.WithHolderCommand(invocationContext(ctx, recorder), strings.Join(append([]string{"lit"}, inv.Args...), " "))
	return dispatch(ctx, inv)
}

func dispatch(ctx context.Context, inv Invocation) error {
	// An invocation built without an environment is refused as one, as Path
	// refuses one built without a working directory, rather than run in the
	// empty environment its zero Env would read as. [LAW:no-silent-failure]
	if inv.Env.IsZero() {
		return errors.New("the invocation was given no environment")
	}
	stdout := inv.Stdout
	normalizedArgs, err := parseGlobalArgs(inv.Args)
	if err != nil {
		return err
	}
	root := newRootCommand(ctx, inv)
	normalizedArgs = rewriteHelpCommand(normalizedArgs)
	// Never nil: cobra reads os.Args in place of a nil argument list, and
	// parseGlobalArgs hands back nil for `lit --`.
	root.SetArgs(append([]string{}, normalizedArgs...))
	root.SetOut(stdout)
	root.SetErr(inv.Stderr)
	root.SilenceErrors = true
	root.SilenceUsage = true
	err = root.ExecuteContext(ctx)
	if errors.Is(err, pflag.ErrHelp) || errors.Is(err, errHelpHandled) {
		return nil
	}
	// A family help request surfaces here as a typed value because resolve has
	// no writer; this seam owns stdout, so it performs the print the type
	// describes and completes the request as the success it is — no error
	// framing, no remediation, exit 0. [LAW:effects-at-boundaries]
	var helpRequested HelpRequestedError
	if errors.As(err, &helpRequested) {
		_, printErr := io.WriteString(stdout, helpRequested.Error())
		return printErr
	}
	// A command starved by a co-resident store holder leaves a durable record
	// alongside the sync traces the holder itself writes, so the two events can
	// be correlated afterwards.
	// Recorded here because every command's error returns through this seam;
	// the mirror never reaches it — its busy errors complete through the
	// push-outcome seam and it exits 0. [LAW:single-enforcer]
	recordEngineOpenContentionTrace(ctx, inv.Env, err)
	return err
}

func newRootCommand(ctx context.Context, inv Invocation) *cobra.Command {
	var helpRequested *bool
	root := &cobra.Command{
		Use:  "lit",
		Long: "Agent-native issue tracker",
		Args: cobra.ArbitraryArgs,
		// The root parses its own argv, like every registered command. Cobra's
		// parse answers a help flag before RunE runs, which would print the
		// root help at exit 0 for `lit nosuchcmd --help` — a mistyped command
		// reported as a success.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			parseErr := cmd.Flags().Parse(args)
			telemetry.Flags(cmd.Context(), setFlagNames(cmd.Flags()))
			if parseErr != nil {
				return cmd.FlagErrorFunc()(cmd, parseErr)
			}
			// [LAW:single-enforcer] the one place a name that is no command is
			// refused. It outranks a help flag on either side of it, because
			// there is no command to describe, and it answers exactly as the
			// same name without the flag does.
			if cmd.Flags().NArg() > 0 {
				return UnknownCommandError{Command: cmd.Flags().Arg(0)}
			}
			if *helpRequested {
				return cmd.Help()
			}
			// [LAW:one-source-of-truth] Default command reuses renderQuickstartGuidance
			// so the output is always identical to `lit quickstart`.
			ws, wsErr := resolveWorkspace(cmd.Context(), inv.Env, inv.Dir, workspace.PrefixRequest{})
			// [LAW:dataflow-not-control-flow] Only the "outside git repo" case routes to help;
			// other failures (getcwd, template load) surface so they remain diagnosable.
			var outside OutsideWorkspaceError
			if errors.As(wsErr, &outside) {
				return cmd.Help()
			}
			if wsErr != nil {
				return wsErr
			}
			guidance, guidanceErr := renderQuickstartGuidance(inv.Env, ws.RootDir)
			if guidanceErr != nil {
				return guidanceErr
			}
			_, printErr := fmt.Fprintln(cmd.OutOrStdout(), guidance)
			return printErr
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	// The root declares its help flag itself, so the flag exists before cobra
	// routes: cobra's Find strips flags to reach the command name, and it strips
	// `--help` as the boolean it is only once the flag is declared. Cobra's own
	// declaration runs later, inside execute, and adds nothing when one exists.
	helpRequested = root.Flags().BoolP("help", "h", false, "help for lit")
	// Flags belong to the root only up to the first positional: `lit <command>
	// [args]`, where everything from the command name on is that command's.
	root.Flags().SetInterspersed(false)
	// [CLI] Exit codes are a contract: an unknown flag, whether in the root's
	// own parse or on cobra's `help` command, is a usage error, the same
	// ExitUsage lit's parsers return, not a generic failure. [LAW:single-enforcer]
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError{Message: err.Error()}
	})
	root.SetOut(inv.Stdout)
	root.SetErr(inv.Stderr)
	applyRegistry(root, commandGroups, commandSpecs(ctx, inv))
	return root
}

func runWithApp(ctx context.Context, inv Invocation, accessMode app.AccessMode, run func(context.Context, *app.App) error) error {
	ws, err := resolveWorkspace(ctx, inv.Env, inv.Dir, workspace.PrefixRequest{})
	if err != nil {
		return err
	}
	// [LAW:single-enforcer] Cobra command registration owns app access selection so startup mode is declared once per entrypoint.
	ap, err := app.Open(ctx, inv.Env, ws, accessMode)
	if err != nil {
		// The open boundary stamps holder contention so Run's trace can tell a
		// starved OPEN from a handler-traced mid-command contention, filed under
		// the workspace it opened in. Open's other failures are not a store
		// acquisition, so they pass through unstamped.
		var storeErr app.StoreOpenError
		if errors.As(err, &storeErr) {
			return markEngineOpenContention(err, storeErr.Workspace)
		}
		return err
	}
	// The engine closes inside runThenMeasure (including on panic), so the close
	// always precedes the automatic sync below, which needs only the workspace.
	runErr := runThenMeasure(ctx, ws.DatabasePath, ap.Close, func() error {
		reportOpenMigration(ctx, storage.OpenMigration(ap.Store))
		return run(ctx, ap)
	})
	// After the close, so the write happens outside the workspace lock, and
	// before the error check, because a fact recorded before a failure is still
	// true: `lit next` reports finding nothing as an error, and that is the fact.
	recordStatusline(ctx, ws, ap.Statusline)
	// Beside the statusline for the same two reasons: it needs no engine, and a
	// clock reading a failed command took was still taken. A write another
	// process left staged that this command committed is reported the same way,
	// as a sync session reports it. [LAW:nothing-unseen]
	reportStampCorrections(ctx, ap.Store)
	reportStrandedWrites(ctx, ap.Store)
	if runErr != nil {
		return runErr
	}
	// The staleness banner for mutating commands lives HERE, at the one seam
	// every mutating command flows through — not per-handler like the read
	// commands' banner, because per-handler wiring would make every mutating
	// command remember the call, and one that forgot would miss it. A command
	// that only mutates hears about failing pushes. [LAW:locality-or-seam]
	// It runs after the engine close on purpose: it reads only storage-dir
	// markers and git's remote config, needing no engine, and precedes the
	// auto-sync below so the banner reports the last COMPLETED attempt rather
	// than racing the one about to spawn.
	if accessMode == app.AccessWrite {
		printMutationSyncStalenessWarning(ctx, inv.Env, inv.Stdout, ws, time.Now())
	}
	// [LAW:single-enforcer] One owner consults the auto-sync policy after a
	// successful command, AND after that command's engine is closed: the on-change
	// push mirror and the receive are detached workers that open their own engines
	// only once this process exits — so at no point are this command's engine and
	// a worker's open on the path together, which embedded Dolt forbids. Command
	// handlers stay unaware of any of this.
	// [LAW:no-ambient-temporal-coupling]
	maybeAutoSyncAfterCommand(ctx, inv.Env, accessMode, ws)
	return nil
}

// resolveWorkspace acquires the workspace for the invocation's working
// directory, carrying whatever issue prefix the caller's flags asked for — the
// zero request for every command but `lit init --prefix`.
// [LAW:single-enforcer] Every command acquires its workspace here, so this is
// the one translation of ErrNotGitRepo into the typed CLI error and the one
// place the workspace's config keys are checked.
func resolveWorkspace(ctx context.Context, env environ.Env, dir WorkingDir, requested workspace.PrefixRequest) (workspace.Info, error) {
	cwd, err := dir.Path()
	if err != nil {
		return workspace.Info{}, fmt.Errorf("get cwd: %w", err)
	}
	ws, err := workspace.ResolveWithPrefix(env, cwd, requested)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			// [LAW:types-are-the-program] Typed error carries classification; message preserved for surface display.
			return workspace.Info{}, OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}
		}
		return workspace.Info{}, err
	}
	reportUnknownConfigKeys(ctx, env, ws.RootDir)
	return ws, nil
}

// rewriteHelpCommand turns `lit help <path...>` into `lit <path...> --help`, so
// a command's help has exactly one renderer no matter which spelling the caller
// reached for. Cobra's own help command renders a subcommand from its Long and
// its cobra-level flag set, and every command here sets DisableFlagParsing and
// declares its flags on its own leaf — so that path would print a description
// with a flag table claiming `lit import` accepts only `--help`, while `lit
// import --help` prints the real flags and no description. Two maps of one
// territory, each missing the half the other has. [LAW:one-source-of-truth]
//
// Bare `lit help` still names no command, so it keeps reaching cobra's root
// help — the one page cobra does render correctly, because the root is where
// the group listing lives. A flag-shaped topic is cobra's business too.
//
// A topic naming no command is rewritten like any other: `lit nosuchcmd --help`
// reaches the root, which refuses the name with lit's typed error — the exit
// code and remediation cobra's bare "Unknown help topic" never carried.
//
// The --help lands ahead of any `--` terminator, the last place it is still a
// flag: `lit help ls -- x` asks for ls's help, not for a positional "--help".
func rewriteHelpCommand(args []string) []string {
	if len(args) < 2 || args[0] != "help" || strings.HasPrefix(args[1], "-") {
		return args
	}
	topic := slices.Clone(args[1:])
	terminator := slices.Index(topic, "--")
	if terminator < 0 {
		terminator = len(topic)
	}
	return slices.Insert(topic, terminator, "--help")
}

func parseGlobalArgs(args []string) ([]string, error) {
	// [LAW:single-enforcer] Legacy --output rejection lives in one global parser path.
	// The root's own flags run up to the first token that is not flag-shaped.
	for index, arg := range args {
		switch {
		case arg == "--output" || strings.HasPrefix(arg, "--output="):
			return nil, unsupportedOutputFlagError()
		case arg == "--":
			// Consumed: every root flag is a boolean, so a `--` here guards no
			// flag value, and cobra's routing stops at one — left in place it
			// would strand the command name after it at the root, refused as
			// unknown (`lit --help -- ls`).
			return slices.Concat(args[:index], args[index+1:]), nil
		case !strings.HasPrefix(arg, "-"):
			return args, nil
		}
	}
	return args, nil
}

func unsupportedOutputFlagError() error {
	return UnsupportedError{Message: "--output is no longer supported; omit it for text output"}
}

// rankPlacement translates the CLI's --top boolean into the domain placement
// value at the boundary. The unflagged arm names no placement of its own — it
// hands back the zero value, so the CLI cannot state a default that drifts
// from the one storage.RankPlacement defines for every other creation surface.
// [LAW:one-source-of-truth]
func rankPlacement(top bool) storage.RankPlacement {
	if top {
		return storage.RankTop
	}
	var unflagged storage.RankPlacement
	return unflagged
}

// planUsage is the one help line for --plan, on every command that writes a
// plan.
const planUsage = "How the work will be done, for a law review before it starts: the mechanism it touches, what it adds there, what it removes. Each edit is a new version; `lit verdict` records a review of one"

func newLeaf() appLeaf {
	fs := newCobraFlagSet("new")
	title := fs.String("title", "", "Issue title")
	description := registerLongText(fs, "description", "Issue description")
	prompt := registerLongText(fs, "prompt", "Reusable agent prompt for the work this issue captures")
	plan := registerLongText(fs, "plan", planUsage)
	issueType := fs.String("type", string(model.TypeTask), "Issue type: "+issueTypeChoices())
	topic := fs.String("topic", "", "Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')")
	parentID := fs.String("parent", "", "Epic the ticket lives inside — required for every type but epic, which may be top-level or inside another epic; child IDs become parentID.<hash>")
	priority := fs.String("priority", model.PriorityNormal.String(), "Priority: "+priorityChoices())
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	lane := fs.String("lane", "", "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes")
	top := fs.Bool("top", false, "Promote the new issue to the top of its frame (the default appends it to the bottom)")
	resolveActor := registerActor(fs)
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		issueTypeValue, err := model.ParseIssueType(*issueType)
		if err != nil {
			return err
		}
		priorityValue, err := model.ParsePriorityName(*priority)
		if err != nil {
			return err
		}
		prefix, err := ap.Workspace.IssuePrefix.Mintable()
		if err != nil {
			return err
		}
		issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
			Title: *title, Description: description.value(), Prompt: prompt.value(), Plan: plan.value(), IssueType: issueTypeValue, Topic: *topic, ParentID: *parentID, Priority: priorityValue, Assignee: strings.TrimSpace(*assignee), Labels: splitCSV(*labels), Lane: *lane,
			Placement: rankPlacement(*top),
			Prefix:    prefix.Value(),
			Actor:     resolveActor(inv.Env),
		})
		if err != nil {
			return err
		}
		if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, ticketCreatedOccasion(issue)); err != nil {
			return err
		}
		if err := printIssueSummary(inv.Stdout, issue); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "new")
	}}
}

// followupLeaf files a follow-up of --on: the next piece of the same effort,
// intended for the capture-at-close moment when a closing agent has fresh
// context about work the close surfaced. The follow-up lands beside the ticket
// it follows up — under that ticket's own parent, ranked among the same epic's
// children — never under it: nesting new work inside finished work puts the
// open follow-up out of its epic's rank order and turns the closed ticket into
// an accidental container. Beside a top-level epic is the top level, where
// only an epic may live, so a follow-up of one is filed as an epic
// (`--type epic`) or in the epic it belongs to (`--parent`), and the store
// refuses anything else. --parent files any follow-up in the epic it names
// instead of beside --on, and the follow-up keeps its link either way. The
// link to the ticket it follows up is the row's FollowsUp, which `lit show`
// renders on both ends. Topic inherits from that ticket when omitted;
// description defaults to a reference back to it.
//
// See design-docs/preparing-the-next-loop.md for the principle this
// implements (capture-at-close affordances).
func followupLeaf() appLeaf {
	fs := newCobraFlagSet("followup")
	on := fs.String("on", "", "Required id of the ticket this follows up (typically the just-closed ticket)")
	title := fs.String("title", "", "Required follow-up title")
	description := registerLongText(fs, "description", "Optional description; defaults to a reference back to --on")
	prompt := registerLongText(fs, "prompt", "Optional reusable agent prompt for the follow-up")
	issueType := fs.String("type", string(model.TypeTask), "Issue type: "+issueTypeChoices())
	parent := fs.String("parent", "", "Epic to file the follow-up in; defaults to the parent of --on, beside it")
	topic := fs.String("topic", "", "Topic slug; inherits from --on when omitted")
	priority := fs.String("priority", model.PriorityNormal.String(), "Priority: "+priorityChoices())
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	top := fs.Bool("top", false, "Promote the follow-up to the top of its frame (the default appends it to the bottom)")
	resolveActor := registerActor(fs)
	// One sentence for both of followup's usage failures — a missing --on/--title,
	// and a value typed where no positional is taken. It is declared outside the
	// work because the seam's arity check runs first and cannot reach a sentence
	// written inside it. [LAW:one-source-of-truth]
	usage := "usage: lit followup --on <id> --title <text> [--description <text> | --description-file <path|->] [--prompt <text> | --prompt-file <path|->] [--topic <slug>] [--type <task|feature|bug|chore|epic>] [--parent <epic-id>] [--priority <" + priorityChoices() + ">] [--assignee <user>] [--labels <csv>] [--top] [--by <name>]"
	return appLeaf{fs: fs, positionals: 0, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		onID := strings.TrimSpace(*on)
		titleValue := strings.TrimSpace(*title)
		if onID == "" || titleValue == "" {
			return UsageError{Message: usage}
		}
		// The detail, not the bare issue: the follow-up's parent is the
		// followed-up ticket's parent, which only the hierarchy knows.
		origin, err := ap.Store.GetIssueDetail(ctx, onID)
		if err != nil {
			return err
		}
		// The follow-up lands where --parent names, or beside --on: under its
		// parent, or at the top level for a top-level --on, the empty parent id
		// being the value a top-level create carries. The store judges the
		// parent against the follow-up's type as it judges every create.
		// [LAW:dataflow-not-control-flow] [LAW:single-enforcer]
		parentID := strings.TrimSpace(*parent)
		if parentID == "" && origin.Parent != nil {
			parentID = origin.Parent.ID
		}
		resolvedTopic := strings.TrimSpace(*topic)
		if resolvedTopic == "" {
			resolvedTopic = origin.Issue.Topic
		}
		resolvedDescription := strings.TrimSpace(description.value())
		if resolvedDescription == "" {
			resolvedDescription = fmt.Sprintf("Follow-up surfaced at the close of %s: %s", origin.Issue.ID, origin.Issue.Title)
		}
		issueTypeValue, err := model.ParseIssueType(*issueType)
		if err != nil {
			return err
		}
		priorityValue, err := model.ParsePriorityName(*priority)
		if err != nil {
			return err
		}
		prefix, err := ap.Workspace.IssuePrefix.Mintable()
		if err != nil {
			return err
		}
		issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
			Title:       titleValue,
			Description: resolvedDescription,
			Prompt:      strings.TrimSpace(prompt.value()),
			IssueType:   issueTypeValue,
			Topic:       resolvedTopic,
			ParentID:    parentID,
			FollowsUp:   origin.Issue.ID,
			Priority:    priorityValue,
			Assignee:    strings.TrimSpace(*assignee),
			Labels:      splitCSV(*labels),
			Placement:   rankPlacement(*top),
			Prefix:      prefix.Value(),
			Actor:       resolveActor(inv.Env),
		})
		if err != nil {
			return err
		}
		if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, ticketCreatedOccasion(issue)); err != nil {
			return err
		}
		if err := printIssueSummary(inv.Stdout, issue); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "new")
	}}
}

// listScope is what a listing queries: the store, and the ready policy that store can
// answer for. Two acquisition paths produce it — the current workspace's own
// store, and a foreign store named by --at — and everything after is the same
// query, which is why the pair is a VALUE the work receives rather than a fork
// inside it. [LAW:dataflow-not-control-flow]
type listScope struct {
	store  storage.Store
	policy readyPolicy
}

// listSurface is one command name over the listing leaf. `ls` and `children`
// are the same listing — one flag surface, one filter, one renderer — and differ
// only in the positionals they take, each of which names a parent whose direct
// children the listing selects. `lit children <id>` is therefore exactly
// `lit ls --parent <id>`, and a fix to either is a fix to both.
// [LAW:one-type-per-behavior]
type listSurface struct {
	name        string
	positionals []string
}

// usageLine names what this surface takes, in the surface's own words. Both the
// too-few refusal in listPositionals and the surplus refusal parseLeaf raises
// read it, so one command cannot describe itself two ways.
// [LAW:one-source-of-truth]
func (s listSurface) usageLine() string {
	return strings.Join(append([]string{"usage: lit", s.name}, s.positionals...), " ") + " [flags]"
}

var (
	lsSurface       = listSurface{name: "ls"}
	childrenSurface = listSurface{name: "children", positionals: []string{"<parent-id>"}}
)

// runList is the entrypoint for every listing surface. It declares and parses the
// flag surface, then chooses which store to query and runs the shared query
// (listLeaf's work) against it. With no --at it opens the current workspace's
// store read-only; with --at <dir> it opens a foreign store by its storage
// directory (one of the paths `lit stores` prints), WITHOUT depending on the
// current directory being a lit workspace.
//
// The store choice is routed here rather than through the standard appCmd wrapper
// because appCmd opens the cwd workspace before the handler runs, which would make
// `ls --at <foreign>` fail outside a workspace even though it never needed the cwd
// store. The parse precedes BOTH openings, so `ls --help` and a malformed flag are
// answered with no store at all, and the routing reads --at from that parse
// rather than rescanning argv — pflag is the one reader of the flag grammar, so
// the `--` terminator and every other flag's arity are its rules alone.
// [LAW:one-source-of-truth]
func runList(ctx context.Context, inv Invocation, surface listSurface, args []string) error {
	l, atDir := listLeaf(surface)
	declared, err := parseLeaf(ctx, l, args, inv)
	if err != nil {
		return err
	}
	positional, err := listPositionals(declared, surface)
	if err != nil {
		return err
	}
	if l.fs.Changed(lsAtFlag) {
		loc := workspace.LocationFromStorageDir(atDir.path())
		st, err := app.OpenLocationForRead(ctx, inv.Env, loc)
		if err != nil {
			// [LAW:no-silent-failure] Name the path so a wrong or un-initialized
			// store dir is an actionable error, not an empty list. The open
			// boundary stamps holder contention (inside the wrap — errors.As
			// reaches through it) bound to the --at TARGET, so the trace files
			// beside the traces of whatever holds that store — from any cwd,
			// including no workspace at all.
			return fmt.Errorf("open store at %q read-only: %w", atDir.path(), markEngineOpenContention(err, infoForLocation(loc)))
		}
		return runThenMeasure(ctx, loc.DatabasePath, st.Close, func() error {
			reportOpenMigration(ctx, storage.OpenMigration(st))
			// noReadyPolicy, for the reason gatherCrossProjectRollup states at its own
			// foreign-store call: a Location carries no repo root, so there is no ready
			// policy to read. It opts out of ONLY the field-presence gate; every
			// store-intrinsic annotation — blockers, the lane gate, reserved blocking
			// labels — still runs, so `--columns blocked` over a foreign store answers the
			// registry's question minus the one input that store cannot supply.
			return l.work(ctx, inv, listScope{store: st, policy: noReadyPolicy}, positional)
		})
	}
	return runWithApp(ctx, inv, app.AccessRead, func(ctx context.Context, ap *app.App) error {
		return l.work(ctx, inv, listScope{store: ap.Store, policy: workspaceReadyPolicy(inv.Env, ap)}, positional)
	})
}

// listPositionals trims a listing's positionals and refuses a count the surface
// does not take. It reads the leaf's DECLARED positionals.
//
// Too many ids is refused like too few — a silently ignored parent would list
// the wrong set with exit 0 — and parseLeaf already refuses the extra token for
// every leaf, so the count test here is what catches too FEW. A positional is a
// parent id, so it is trimmed like a --parent id, and a blank one names no
// parent and is refused like an empty --parent.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func listPositionals(raw []string, surface listSurface) ([]string, error) {
	positional := make([]string, len(raw))
	for i, arg := range raw {
		positional[i] = strings.TrimSpace(arg)
	}
	if len(positional) != len(surface.positionals) || slices.Contains(positional, "") {
		return nil, UsageError{Message: fmt.Sprintf("%s  (got %d positional arguments: %q)", surface.usageLine(), len(raw), raw)}
	}
	return positional, nil
}

// listLeaf declares the listing flag surface. It returns the --at value alongside the
// leaf because --at selects the store the work runs against, so runList must read
// it between the parse and the work — the one flag whose value is routing rather
// than query. Every other flag is read inside the work closure.
// lsAtFlag names the routing flag in the two places that must agree: the
// declaration that registers it and the presence check that reads it back off
// the parse. Spelled once, so renaming it cannot leave the check silently
// answering false and routing every --at to the workspace store.
// [LAW:one-source-of-truth]
const lsAtFlag = "at"

func listLeaf(surface listSurface) (leaf[listScope], *pathFlag) {
	fs := newCobraFlagSet(surface.name)
	// No backquotes in usage text: pflag reads the first backquoted span as the
	// flag's value placeholder, so help would print `--at lit stores`.
	at := registerPath(fs, lsAtFlag, "List a discovered store by its storage directory (from lit stores), read-only, instead of the current workspace")
	// StringArray, not String: the filter this feeds is a set both engines OR
	// together, so a second --status has to widen the listing rather than
	// silently keep the last value and drop the first.
	// [LAW:no-silent-failure] [LAW:types-are-the-program]
	// StringArray rather than StringSlice deliberately: pflag's slice flag would
	// split on commas itself, leaving two splitters for one grammar. Handing the
	// raw occurrences to model.ParseStates keeps that rule in one place, shared
	// with the query term. [LAW:single-enforcer]
	status := fs.StringArray("status", "Filter by status, comma-separated or repeated: open|in_progress|closed")
	// Every set-valued filter below is a StringArray for the reason --status is
	// one: a second occurrence widens the set rather than replacing the first.
	// [LAW:no-silent-failure]
	issueType := fs.StringArray("type", "Filter by issue type, comma-separated or repeated: "+issueTypeChoices())
	assignee := fs.StringArray("assignee", assigneeFlagUsage)
	// Repeated, never comma-split: search text may itself contain a comma, and
	// each occurrence is one more term every match must contain.
	search := fs.StringArray("search", "Search title and description text; repeat to require every term")
	ids := fs.StringArray("ids", "Issue IDs, comma-separated or repeated")
	parent := fs.StringArray("parent", "Only direct children of these issue IDs, comma-separated or repeated (lit children <id> is lit ls --parent <id>)")
	labels := fs.StringArray("labels", "Labels all of which must match, comma-separated or repeated")
	hasComments := fs.Bool("has-comments", false, "Only include issues with comments")
	includeArchived := fs.Bool("include-archived", false, "Include archived issues")
	includeDeleted := fs.Bool("include-deleted", false, "Include deleted issues")
	updatedAfter := fs.String("updated-after", "", "Only include issues updated at or after RFC3339 timestamp")
	updatedBefore := fs.String("updated-before", "", "Only include issues updated at or before RFC3339 timestamp")
	queryExpr := fs.String("query", "", "Query language: status:closed,in_progress resolution:wontfix type:task has:comments sort:rank:asc limit:5 archived deleted text")
	sortExpr := fs.String("sort", "", "Sort fields, e.g. rank:asc,updated_at:desc")
	columnsExpr := fs.String("columns", "", columnsFlagUsage())
	format := fs.String("format", "lines", "Output format: "+strings.Join(sortedListFormatNames(), "|"))
	limit := fs.Int("limit", 0, "Limit results")
	// The surface's own arity, declared.
	// [LAW:one-source-of-truth] one positional channel, not two.
	// A surface that takes positionals names them; one that does not (`ls`)
	// leaves usage empty and takes the derived sentence, which names its
	// value-taking flags — strictly more than "usage: lit ls [flags]" says.
	usage := ""
	if len(surface.positionals) > 0 {
		usage = surface.usageLine()
	}
	return leaf[listScope]{fs: fs, positionals: len(surface.positionals), usage: usage, work: func(ctx context.Context, inv Invocation, scope listScope, positional []string) error {
		st, policy := scope.store, scope.policy
		// Parsed before the query runs and before anything prints: a rejection that
		// had already emitted rows would be a partial answer, which is the silent
		// drop this boundary exists to prevent, wearing an error message.
		formatName, render, err := parseListFormat(*format, *columnsExpr)
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "ls.format", formatName)
		visited := map[string]bool{}
		fs.Visit(func(f *pflag.Flag) { visited[f.Name] = true })
		// A bad member of a set filter is a validation refusal: it repeats on
		// every retry, so it must not draw retry advice. [LAW:no-silent-failure]
		statuses, err := model.ParseStates(*status...)
		if err != nil {
			return model.ValidationError{Message: "parse --status: " + err.Error()}
		}
		issueTypes, err := parseIssueTypeSet(*issueType)
		if err != nil {
			return err
		}
		assignees, err := parseAssigneeSet(*assignee)
		if err != nil {
			return err
		}
		searchTerms, err := parseSearchTerms(*search)
		if err != nil {
			return err
		}
		issueIDs, err := parseIDSet(*ids)
		if err != nil {
			return err
		}
		// Parent ids are read before the surface's positional joins the set:
		// `--parent ""` names no parent whichever command carries it.
		parentIDs, err := storage.ParseNames("--parent needs an issue id in every slot, e.g. --parent <epic-id>", *parent...)
		if err != nil {
			return err
		}
		labelNames, err := parseLabelSet(*labels)
		if err != nil {
			return err
		}
		// A surface's positionals are parent ids too, so `children <id>` and
		// `ls --parent <id>` build the same filter. [LAW:one-source-of-truth]
		parentIDs = append(parentIDs, positional...)
		limitValue, err := query.ParseLimit(*limit)
		if err != nil {
			return fmt.Errorf("parse --limit: %w", err)
		}
		filter := storage.ListIssuesFilter{
			Statuses:        statuses,
			IssueTypes:      issueTypes,
			IDs:             issueIDs,
			ParentIDs:       parentIDs,
			LabelsAll:       labelNames,
			Assignees:       assignees,
			SearchTerms:     searchTerms,
			IncludeArchived: *includeArchived,
			IncludeDeleted:  *includeDeleted,
			Limit:           limitValue,
		}
		if strings.TrimSpace(*sortExpr) != "" {
			// [LAW:one-source-of-truth] Reuse the one store sort parser; the
			// --query sort: token routes through the same function.
			sortSpecs, err := storage.ParseSortSpecs(*sortExpr)
			if err != nil {
				return err
			}
			filter.SortBy = sortSpecs
		}
		if visited["has-comments"] {
			value := *hasComments
			filter.HasComments = &value
		}
		if visited["updated-after"] {
			parsed, err := query.ParseTimestamp(*updatedAfter)
			if err != nil {
				return fmt.Errorf("parse --updated-after: %w", err)
			}
			filter.UpdatedAfter = &parsed
		}
		if visited["updated-before"] {
			parsed, err := query.ParseTimestamp(*updatedBefore)
			if err != nil {
				return fmt.Errorf("parse --updated-before: %w", err)
			}
			filter.UpdatedBefore = &parsed
		}
		// [LAW:dataflow-not-control-flow] An empty query parses to an empty
		// filter, so the merge always runs and the flag-built filter always
		// meets the whole-filter rules, query or no query.
		parsed, err := query.Parse(*queryExpr)
		if err != nil {
			return err
		}
		filter, err = query.Merge(filter, parsed.Filter)
		if err != nil {
			return err
		}
		// [LAW:dataflow-not-control-flow] Default status filter is data, not a branch
		// around ListIssues. Apply the active-work default (exclude closed) only when
		// the user has expressed no intent implying closed issues. Two filter fields
		// carry that intent: an explicit status selection (--status / --query
		// status:..., which also covers status:closed), and a resolution filter —
		// resolutions exist ONLY on closed issues, so `--query resolution:wontfix` is a
		// request to see closed work.
		// [LAW:no-silent-failure] Without the resolution guard a closed-only filter
		// clamps to open+in_progress and returns silently empty.
		if len(filter.Statuses) == 0 && len(filter.Resolutions) == 0 {
			filter.Statuses = []model.State{model.StateOpen, model.StateInProgress}
		}
		issues, err := st.ListIssues(ctx, filter)
		if err != nil {
			return err
		}
		return render(ctx, inv.Stdout, st, policy, issues)
	}}, at
}

// The set-valued flags the listing and bulk commands share parse through the
// functions below, so `lit ls`, `lit backlog`, `lit next`, `lit orphaned` and
// `lit bulk` agree on what each flag accepts and on how a bad occurrence
// fails. A bad member is a validation refusal: it repeats on every retry, so it
// must not draw retry advice. [LAW:single-enforcer] [LAW:no-silent-failure]

const assigneeFlagUsage = "Filter by assignee, comma-separated or repeated"

func parseIssueTypeSet(raw []string) ([]model.IssueType, error) {
	types, err := model.ParseIssueTypes(raw...)
	if err != nil {
		return nil, model.ValidationError{Message: "parse --type: " + err.Error()}
	}
	return types, nil
}

func parseIDSet(raw []string) ([]string, error) {
	return storage.ParseNames("--ids needs an issue id in every slot, e.g. --ids <issue-id>", raw...)
}

func parseAssigneeSet(raw []string) ([]string, error) {
	return storage.ParseNames("--assignee needs a user in every slot, e.g. --assignee <user>", raw...)
}

func parseLabelSet(raw []string) ([]string, error) {
	return storage.ParseNames("--labels needs a label in every slot, e.g. --labels <label>", raw...)
}

// parseSearchTerms keeps each --search occurrence whole: unlike the name sets
// above it never splits on a comma, because a comma is ordinary search text.
func parseSearchTerms(raw []string) ([]string, error) {
	var terms []string
	for _, occurrence := range raw {
		term := strings.TrimSpace(occurrence)
		if term == "" {
			return nil, model.ValidationError{Message: "--search needs text in every occurrence, e.g. --search <text>"}
		}
		terms = append(terms, term)
	}
	return terms, nil
}

// listDerivedColumns builds the per-issue cells the projected columns render,
// loading exactly the rung of the ladder the projection asks for and no more.
// The default and every issue-only projection load nothing; naming `parent`
// buys the relation graph; naming `blocked` buys the annotation pipeline, which
// is what makes this surface's answer the registry's answer rather than a
// shorter one.
// A nil result means "no derived column asked for", and every formatter lookup
// then yields the zero derivedColumns.
//
// policy is the repo's ready policy, resolved HERE rather than by the caller and
// only on the rung that consults it — `lit ls --columns id,title` never reads
// config at all, which is what keeps a defect in an unrelated setting from
// taking out the plain listing. `lit ls --at <dir>` supplies noReadyPolicy: a
// foreign store has no repo root and so no config, and that opts out of ONLY the
// field-presence gate, leaving every store-intrinsic annotation to run.
//
// [LAW:dataflow-not-control-flow] which data to load is a value (the column
// set's maximum rung); the switch below branches on that domain enum alone,
// which is the entire point of having it. The policy is a value for the same
// reason — the foreign-store case differs by what it was handed, not by a branch
// here.
// [LAW:no-silent-failure] the default arm panics rather than returning nil: a
// fourth rung added without a loader would otherwise render "-" in a column it
// could not compute, and "-" is shaped exactly like a true answer.
func listDerivedColumns(ctx context.Context, st storage.Store, policy readyPolicy, columns []columnSpec, issues []model.Issue) (map[string]derivedColumns, error) {
	switch source := columnSourceFor(columns); source {
	case sourceIssue:
		return nil, nil
	case sourceRelations:
		relations, err := fetchIssueRelations(ctx, st, issues)
		if err != nil {
			return nil, err
		}
		return parentColumnsFor(relations), nil
	case sourceReadiness:
		// annotateIssues is the one annotator set `lit next`, `lit backlog` and
		// the epic plan route on, and it returns the relations it fetched to
		// compute them — so this rung pays one pipeline, not a graph load plus a
		// classification. [LAW:single-enforcer]
		requiredFields, err := policy()
		if err != nil {
			return nil, err
		}
		annotated, err := annotateIssues(ctx, st, requiredFields, issues)
		if err != nil {
			return nil, err
		}
		return readinessColumnsFor(annotated.rows, annotated.details), nil
	default:
		panic(fmt.Sprintf("cli: no loader for column source %d", source))
	}
}

// parentColumnsFor projects a relation graph down to the cells that graph alone
// can answer. It cannot set blocked, and that is the point: a `blocked` derived
// from dependency edges alone is the shorter list, so no function in the
// package is able to produce one.
func parentColumnsFor(relations map[string]storage.IssueRelations) map[string]derivedColumns {
	out := make(map[string]derivedColumns, len(relations))
	for id, rel := range relations {
		out[id] = derivedColumns{parentID: parentIDOf(rel)}
	}
	return out
}

// parentIDOf reads an issue's parent/epic id off the canonical graph, "" for a
// top-level issue. One reading of that edge, shared by both producers of
// derivedColumns.parentID.
func parentIDOf(rel storage.IssueRelations) string {
	if rel.Parent == nil {
		return ""
	}
	return rel.Parent.ID
}

// workableFilter carries the user-supplied narrowing options for the
// shared workable pipeline. Empty fields mean "no narrowing"; the
// workable definition (open/in_progress, leaves only) is layered on top
// by gatherWorkableAnnotated. Every field is an already-parsed value, never
// a raw flag string — workableFilterFlags.parse is the one seam that builds
// it, so unvalidated input cannot reach the query.
// [LAW:types-are-the-program] Assignees, IssueTypes and Labels are sets
// because the storage criteria they feed are sets; a single value here could
// hold only one occurrence of a repeated flag.
type workableFilter struct {
	Assignees  []string
	IssueTypes []model.IssueType
	Status     model.State
	Labels     []string
}

// narrows reports whether any filter flag was given.
func (f workableFilter) narrows() bool {
	return len(f.Assignees) > 0 || len(f.IssueTypes) > 0 || f.Status != "" || len(f.Labels) > 0
}

// criteria expresses this filter in the storage layer's own selection
// vocabulary, so the pipeline narrows rows it already holds with the rule
// storage defines rather than a second similar-looking one written here.
// [LAW:one-source-of-truth]
//
// What that unifies is this pipeline with storage.IssueCriteria, and not the
// two store implementations with each other. The memory engine narrows with
// this rule directly; the SQL store expresses the same selection in its own
// WHERE clauses and is not held to this type by anything. They agree today,
// but by review rather than by construction, so the single source of truth
// claimed here reaches the store boundary and stops.
//
// Canonicalizing the labels is the only part that can fail, and it fails here,
// before a single row is read. [LAW:parse-dont-validate]
func (f workableFilter) criteria() (storage.IssueCriteria, error) {
	return storage.ParseIssueCriteria(storage.ListIssuesFilter{
		Statuses:   toSlice(f.Status),
		IssueTypes: f.IssueTypes,
		Assignees:  f.Assignees,
		LabelsAll:  f.Labels,
	})
}

// gatherWorkableAnnotated runs the shared workable pipeline: list workable
// leaves, fetch details, annotate, sort into canonical priority/rank order,
// enrich with parent epic refs. Returns the prepared rows and the details
// map so callers that need extra row context (e.g. claim routing's epic-lane
// lookups) avoid a second fetch round-trip.
//
// The returned order is the canonical backlog order: priority desc, then
// rank order (tree order) asc. Ready-specific presentation (e.g. pushing blocked items
// to the bottom) is applied by the caller, not here, so consumers that want the
// unmodified ranking (`lit backlog`) see it as ordered.
//
// The focus scope (stamped by focusQueue) comes back BESIDE the rows rather
// than pre-applied to them, because the two views narrow by it at different
// points: `lit backlog` scopes the rows it prints, while `lit next` scopes only
// its global pool and leaves this checkout's own claimed lanes reachable
// regardless (see routeNext).
//
// [LAW:single-enforcer] `lit next` and `lit backlog` both
// read from this single pipeline so their "what is workable, in what
// order" model cannot drift.
//
// The filter is parsed before a single row is read, so a label the vocabulary
// cannot express fails here rather than after the queue is materialized.
// [LAW:parse-dont-validate]
func gatherWorkableAnnotated(ctx context.Context, env environ.Env, ap *app.App, rf workableFilter) (focusedQueue, error) {
	criteria, err := rf.criteria()
	if err != nil {
		return focusedQueue{}, err
	}
	queue, err := gatherWorkableQueue(ctx, env, ap)
	if err != nil {
		return focusedQueue{}, err
	}
	// The scope is the walk's, not the kept rows': a filter that drops every
	// path row leaves the focus on (see focusScope).
	return focusedQueue{workableGather: queue.keepRows(criteria), scope: queue.scope}, nil
}

// gatherWorkableQueue is the workable pipeline with nothing narrowed: the whole
// queue, for a caller that applies the filter itself. `lit next` is the one —
// its filter decides which rows it may serve, not which rows its walks may read,
// so it needs the rows a filter would drop (see routeNext).
func gatherWorkableQueue(ctx context.Context, env environ.Env, ap *app.App) (focusedQueue, error) {
	requiredFields, err := readyRequiredFields(env, ap)
	if err != nil {
		return focusedQueue{}, err
	}
	// [LAW:locality-or-seam] The pipeline's real inputs are a store surface and
	// the ready required-fields policy; the *app.App only supplied those two.
	// Splitting the config read (a repo-rooted effect) from the classification
	// lets the cross-project rollup run the SAME pipeline over a read-only
	// foreign store — which has no repo root and so no per-repo config — by
	// passing the policy as a value. [LAW:single-enforcer]
	queue, err := classifyWorkable(ctx, ap.Store, requiredFields)
	if err != nil {
		return focusedQueue{}, err
	}
	// The focus walk runs here and nowhere below: `lit next` and `lit backlog`
	// are the views that answer over a focus scope, and the rollup that shares
	// classifyWorkable reports whole-project counts. [LAW:decomposition]
	return focusQueue(ctx, ap.Store, queue)
}

// readyRequiredFields reads this repo's ready required-fields policy. Every
// surface that annotates — the workable pipeline and `lit show`'s epic plan
// slice — reads it through here, so the two cannot disagree about which fields
// a ticket must fill before it counts as startable.
// [LAW:one-source-of-truth] One read of the policy, one place it is named.
func readyRequiredFields(env environ.Env, ap *app.App) ([]string, error) {
	cfg, err := config.Load(env, pathspec.New(ap.Workspace.RootDir))
	if err != nil {
		return nil, err
	}
	return cfg.Ready.RequiredFields, nil
}

// readyPolicy defers the required-fields read to the point of use, for callers
// that do not yet know whether they need it. `lit ls` is the one: which columns
// were projected is not known until the flag parse inside runListWithStore, and
// only a readiness-sourced column consults the policy at all.
//
// Deferring is not a micro-optimization over one file read. config.Load
// validates the whole config — snapshot.retention_budget, sync.cadence,
// sync.history_bound, claims.freshness_window — and fails on any of them, so
// reading it eagerly would make `lit ls` fail on config defects in settings it
// does not render.
// That would take out the plain listing exactly when a repo's config is broken,
// which is when a reader most needs to see their tickets.
// [LAW:no-silent-failure] the policy still fails loudly, at the one projection
// that depends on it.
type readyPolicy func() ([]string, error)

// workspaceReadyPolicy reads the repo's policy when asked.
func workspaceReadyPolicy(env environ.Env, ap *app.App) readyPolicy {
	return func() ([]string, error) { return readyRequiredFields(env, ap) }
}

// noReadyPolicy is the policy of a store with no repo root to read one from.
// A foreign store opened through --at gets this rather than a nil check at the
// point of use, so the field-presence gate opts out by the value it was handed.
// [LAW:dataflow-not-control-flow]
func noReadyPolicy() ([]string, error) { return nil, nil }

// classifyWorkable is the store-facing core of the workable pipeline: list
// workable leaves, fetch details, annotate against the given required-fields
// policy, sort into canonical order, enrich with parent epics. It takes the
// store and the policy directly — not an *app.App — so any opened store drives
// it, including a read-only foreign store the process is not cd'd into.
// [LAW:one-source-of-truth] `lit next`/`backlog` and the cross-project
// rollup share this one definition of "what is workable, annotated how".
//
// No focus walk runs here; focusQueue stamps the focus path on top for the two
// views that answer over it. The rollup counts a whole project's
// ready/in-flight/blocked rows, and narrowing those to one project's focus path
// would leave the counts reading as project totals while meaning something
// else — a query silently swapped for a similar-looking one with different
// semantics. [LAW:no-silent-failure]
func classifyWorkable(ctx context.Context, st storage.Store, requiredFields []string) (workableGather, error) {
	// The query asks for the workable population and nothing a caller wants
	// narrowed. A narrowed query is upstream of everything: it would put the
	// queue facts below out of reach of every consumer at once, and no wider
	// list handed to a renderer could reach back past it. The caller narrows —
	// and releases the rest — once those facts are derived.
	//
	// [LAW:one-source-of-truth] rank is the canonical ordering, and the store's
	// default sort is it: tree order, so every epic's leaves arrive together at
	// the epic's own place in the queue.
	listFilter := storage.ListIssuesFilter{
		Statuses:        []model.State{model.StateOpen, model.StateInProgress},
		IncludeArchived: false,
		IncludeDeleted:  false,
		Limit:           0,
	}
	issues, err := st.ListIssues(ctx, listFilter)
	if err != nil {
		return workableGather{}, err
	}
	issues = filterWorkableIssues(issues)
	queue, err := annotateIssues(ctx, st, requiredFields, issues)
	if err != nil {
		return workableGather{}, err
	}
	// The listing's rank order, then one stable sort by priority, are the whole
	// ordering story. Focus is a scope the views apply afterwards (focusQueue),
	// never an ordering, so ordering has one authority. [LAW:one-source-of-truth]
	sortByPriority(queue.rows)
	enrichWithParentEpic(queue.rows, queue.details)
	// Derived over the queue, so the facts are true of the queue whatever the
	// caller narrows it to: keepRows lets the rest go, and the facts outlive the
	// set they came from as a map of ids and an int.
	return workableGather{annotatedRows: queue, facts: deriveQueueFacts(queue.rows)}, nil
}

// annotateIssues runs every registered annotator over the given issues and
// returns the annotated rows alongside the relations fetched to compute them.
// It asks nothing about where the issues came from, so the list pipeline above
// and the epic plan slice (buildEpicContext) put the SAME facts in front of
// ClassifyReadiness — the whole registry, not the subset a caller happened to
// derive for itself.
// [LAW:single-enforcer] The annotator set lives here alone. A surface that
// assembled its own shorter set would be a second opinion on what blocks.
// [LAW:decomposition] The joint is between WHICH issues a surface is about and
// WHAT the registry says about them; cutting here lets a caller with an already
// resolved set — an epic's children — skip the workable list query entirely.
//
// The focus path is not among these facts. It scopes a view rather than gating
// a pull (annotation.RoleNone), so it is stamped where the views that read it
// are fed (focusQueue), and a surface that reads only readiness — the epic plan,
// `lit ls --columns blocked`, the cross-project rollup — never pays for the
// focus walk. [LAW:decomposition]
//
// The epics above the issues come back too: the blocker annotator already
// loaded them to pass an epic's blockers down, and routing climbs the same
// chain to find the work under an epic that blocks. One load, so "which epics
// is this issue under" has one answer on both sides.
//
// seeds donate relations the caller already holds — the epic plan has its epic
// — so the ancestry climb reuses them rather than fetching them again. The
// subjects themselves are always read fresh, below.
func annotateIssues(ctx context.Context, st storage.Store, requiredFields []string, issues []model.Issue, seeds ...map[string]storage.IssueRelations) (annotatedRows, error) {
	fieldAnnotator, err := newFieldAnnotator(requiredFields)
	if err != nil {
		return annotatedRows{}, err
	}
	details, err := fetchIssueRelations(ctx, st, issues)
	if err != nil {
		return annotatedRows{}, err
	}
	// Annotate the subjects as this fetch returned them, not as the caller passed
	// them in. The caller's copies came from an earlier read — a list query, or an
	// epic's child set — and every annotator already reads its relations out of
	// details, so annotating the caller's copies would classify one snapshot of a
	// child using another snapshot's edges, and hand back rows whose Issue is
	// older than the details returned beside them. One fetch, one snapshot, for
	// the annotations and for everything downstream that renders from either.
	// [LAW:one-source-of-truth] fetchIssueRelations fails loudly on a subject it
	// cannot resolve, so every input issue has an entry here.
	subjects := make([]model.Issue, len(issues))
	for i, issue := range issues {
		subjects[i] = details[issue.ID].Issue
	}
	// The lane gate and the blocker annotator read the ancestor epics' FULL
	// relations (unfiltered by the CLI assignee/type/label narrowing), so an
	// earlier sibling or an epic's blocker hidden by those filters still gates.
	// details goes in last so the fresh read of a subject wins over any seeded
	// copy of it.
	memo, loaded := memoizeRelations(st.GetRelationsByIDs, slices.Concat(seeds, []map[string]storage.IssueRelations{details})...)
	held, err := fetchHeldAncestry(ctx, memo, details)
	if err != nil {
		return annotatedRows{}, err
	}
	// Which open dependencies are rank inversions is a reading of every live
	// edge, not of these rows alone, so it is the store's one answer rather
	// than one derived here. [LAW:single-enforcer]
	dependencies, err := st.DependencyOrder(ctx)
	if err != nil {
		return annotatedRows{}, err
	}
	annotated, err := annotation.Annotate(ctx, subjects,
		fieldAnnotator,
		newBlockerAnnotator(details, held, dependencies),
		newSiblingGateAnnotator(details, pendingSiblingsByEpic(held.ancestry.relations)),
		newOrphanedAnnotator(orphanedThreshold),
		newBlockingLabelAnnotator(),
	)
	if err != nil {
		return annotatedRows{}, err
	}
	return annotatedRows{rows: annotated, details: details, epics: held.ancestry.relations, loaded: loaded}, nil
}

// annotatedRows is one annotateIssues pass. Named rather than positional
// because details and epics share a type: as two map results in a row, a
// caller that crossed them would compile and route on the wrong map.
// [LAW:types-are-the-program]
type annotatedRows struct {
	rows    []annotation.AnnotatedIssue
	details map[string]storage.IssueRelations
	// epics is the relations of every epic above a row, keyed by epic id — the
	// chain epicsAbove climbs.
	epics map[string]storage.IssueRelations
	// loaded is every relation the pass read — the subjects, the epics above
	// them, and the blockers' wait walks — the seed a later walk over the same
	// graph reuses rather than re-reading.
	loaded map[string]storage.IssueRelations
}

// runOrphaned lists in_progress issues whose last update is older than
// orphanedThreshold — the same definition `lit backlog` uses for the
// "(ORPHANED)" marker, surfaced as a focused command for reclamation
// workflows.
//
// [LAW:single-enforcer] Orphan classification lives in
// newOrphanedAnnotator; this command only filters and presents.
// [LAW:one-source-of-truth] Threshold comes from orphanedThreshold,
// not a re-declared local constant.
func orphanedLeaf() appLeaf {
	fs := newCobraFlagSet("orphaned")
	assignee := fs.StringArray("assignee", assigneeFlagUsage)
	return appLeaf{fs: fs, positionals: 0, usage: "usage: lit orphaned [--assignee <user>]", work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		assignees, err := parseAssigneeSet(*assignee)
		if err != nil {
			return err
		}
		listFilter := storage.ListIssuesFilter{
			Statuses:        []model.State{model.StateInProgress},
			Assignees:       assignees,
			IncludeArchived: false,
			IncludeDeleted:  false,
		}
		issues, err := ap.Store.ListIssues(ctx, listFilter)
		if err != nil {
			return err
		}
		// Containers (epics) derive state from children; their own UpdatedAt
		// has no relationship to whether any agent is working on them, so
		// orphaning them based on it is meaningless. Drop them — orphan is
		// a leaf-only concept here, same as in `lit backlog`/`lit next`.
		issues = filterWorkableIssues(issues)
		annotated, err := annotation.Annotate(ctx, issues, newOrphanedAnnotator(orphanedThreshold))
		if err != nil {
			return err
		}
		orphaned := make([]annotation.AnnotatedIssue, 0, len(annotated))
		for _, entry := range annotated {
			if ClassifyReadiness(entry.Annotations).IsOrphaned() {
				orphaned = append(orphaned, entry)
			}
		}
		// Sort oldest-first so the most stale work surfaces at the top —
		// the row most likely to need reclamation. Same UpdatedAt the
		// annotator keyed off, so order matches staleness directly.
		sort.SliceStable(orphaned, func(i, j int) bool {
			return orphaned[i].UpdatedAt.Before(orphaned[j].UpdatedAt)
		})
		return printOrphanedText(inv.Stdout, orphaned)
	}}
}

func printOrphanedText(w io.Writer, rows []annotation.AnnotatedIssue) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No orphaned issues.")
		return err
	}
	columns := mustColumns("id", "state", "topic", "assignee", "title")
	for _, entry := range rows {
		line := formatIssueColumns(entry.Issue, columns, " | ", nil)
		age := time.Since(entry.UpdatedAt).Truncate(time.Minute)
		line += " | Last Update: " + age.String()
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func showLeaf() appLeaf {
	fs := newCobraFlagSet("show")
	fieldsExpr := fs.String("field", "", "Comma-separated field names (e.g. description) to print with no surrounding context; omit for the full detail view")
	const usage = "usage: lit show <id> [--field <name>[,<name>...]]"
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		// links-sync-pgct.2: `lit show` is named explicitly as one of the ordinary
		// read commands unpushed/unfetched drift must surface on — but only ahead
		// of the full-detail view. --field is the compact, machine-parseable
		// contract (bare value(s), no surrounding context) automation round-trips
		// into `lit update`; prepending the banner there would corrupt it.
		// [LAW:dataflow-not-control-flow]
		if strings.TrimSpace(*fieldsExpr) == "" {
			if err := printStalenessWarning(ctx, inv.Env, inv.Stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
				return err
			}
		}
		detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
		if err != nil {
			return err
		}
		// The agent viewed the ticket's details either way, --field or full view.
		if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, showTicketOccasion(detail.Issue)); err != nil {
			return err
		}
		// --field is the compact, edit-oriented view: exactly the requested
		// fields, none of the parent/epic/siblings context below. [LAW:dataflow-not-control-flow]
		if fields := splitCSV(*fieldsExpr); len(fields) > 0 {
			return printIssueFields(inv.Stdout, detail, fields)
		}
		// Resolved before the body is printed, so the body and the plan block are
		// all-or-nothing: a slice that cannot be built fails without a body, rather
		// than after one that reads as a complete show of an epic-less ticket.
		// [LAW:parse-dont-validate]
		plan, err := resolveEpicContext(ctx, inv.Env, ap, detail)
		if err != nil {
			return err
		}
		if err := printIssueDetail(inv.Stdout, detail); err != nil {
			return err
		}
		return writeEpicContext(inv.Stdout, plan)
	}}
}

// runHistory renders a ticket's state-transition trail as a dedicated
// read-only surface. It reads the same GetIssueDetail as `lit show`
// and renders only the event trail, so the two views share one data source
// and one formatter (printHistoryEvents). [LAW:one-source-of-truth]
// --version reads the same trail for the one superseded text the trail
// itself only numbers.
func historyLeaf() appLeaf {
	fs := newCobraFlagSet("history")
	version := fs.String("version", "", "Print the text one version of a long text field held, as <field>@<n>; each text the field has held is numbered once, from 1, in the order its history first shows it, and the trail shows each change as vN → vM. A sync that merges in an older edit renumbers the versions after it")
	const usage = "usage: lit history <id> [--version <field>@<n>]"
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		// The flag is parsed before the store is read, so a malformed reference
		// is a usage error whatever the id names. [LAW:parse-dont-validate]
		render := printIssueHistory
		if fs.Changed("version") {
			ref, err := parseVersionRef(*version)
			if err != nil {
				return err
			}
			render = func(w io.Writer, detail model.IssueDetail) error { return printFieldVersion(w, detail, ref) }
		}
		detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
		if err != nil {
			return err
		}
		return render(inv.Stdout, detail)
	}}
}

// statusViaVerbsGuidance is returned when `lit update --status` is used. Status
// is moved only by the transition verbs, which are [LAW:single-enforcer] the one
// surface carrying the transition guardrails — `close` requires a resolution,
// `start` does the claim transfer and identity resolution, each records its own
// guidance. `update` sets fields; letting it also set status would make it a
// second, divergent face of the same mutation. The break is deliberate and
// documented: the capability is fully reachable, just under the verbs.
// [LAW:no-silent-failure]
const statusViaVerbsGuidance = "lit update no longer changes status — the transition verbs are the single enforcer of the transition guardrails. Use: `lit start <id>` (claim → in_progress), `lit done <id>` (finish → closed), `lit close <id> --resolution <duplicate|superseded|obsolete|wontfix>` (close with an outcome), `lit open <id>` (reopen)"

func updateLeaf() appLeaf {
	fs := newCobraFlagSet("update").Detail(helpText("update"))
	title := fs.String("title", "", "Issue title")
	description := registerLongText(fs, "description", "Issue description")
	prompt := registerLongText(fs, "prompt", "Reusable agent prompt for the work this issue captures")
	plan := registerLongText(fs, "plan", planUsage)
	issueType := fs.String("type", "", "Issue type: "+issueTypeChoices())
	priority := fs.String("priority", model.PriorityNormal.String(), "Priority: "+priorityChoices()) // [LAW:one-source-of-truth] default derives from model constant; matches runNew/runFollowup
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	lane := fs.String("lane", "", "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes")
	// --status stays registered (its value discarded) only so a stale invocation
	// is intercepted with the documented pointer to the transition verbs below,
	// not pflag's bare "unknown flag". Presence is detected via fs.Visit.
	fs.String("status", "", "(removed) change status with the transition verbs: lit start|done|close|open")
	reason := fs.String("reason", "", "Reason recorded on the field-change event")
	resolveActor := registerActor(fs)
	// update takes twelve value flags, past the point where the derived fallback
	// stops naming them and points at --help. Its own sentence names them all, so
	// it supplies it rather than taking the fallback. [LAW:one-source-of-truth]
	usage := "usage: lit update <id> [--title <text>] [--description <text> | --description-file <path|->] [--prompt <text> | --prompt-file <path|->] [--plan <text> | --plan-file <path|->] [--type <task|feature|bug|chore|epic>] [--priority <" + priorityChoices() + ">] [--assignee <user>] [--labels <csv>] [--lane <key>] [--reason <text>] " + bulkEditGuidance
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		visited := map[string]bool{}
		fs.Visit(func(flag *pflag.Flag) { visited[flag.Name] = true })
		// Status is moved only by the transition verbs; `update` is field mutation.
		// Rejecting --status here (rather than routing it through an action) keeps
		// the verbs the single enforcer of the transition guardrails and stops
		// `update` from being a second, divergent face of the same mutation — most
		// concretely, no `--status closed` back door around `close`'s required
		// resolution. The capability is fully reachable under the verbs; the break
		// is deliberate and named, not silent. [LAW:single-enforcer] [LAW:no-silent-failure]
		if visited["status"] {
			return UsageError{Message: statusViaVerbsGuidance}
		}

		// [LAW:dataflow-not-control-flow] Always build one Change; variability lives
		// in which field pointers are set, not in which branch runs. The actor
		// resolves through the same identity rule as the assignee — the agent's
		// session wins, else --by — and is recorded on the field-change event.
		// Fields.Reason annotates that event; with no action, there is no transition
		// event for update to reason about. [LAW:single-enforcer]
		in := storage.Change{
			Actor: resolveActor(inv.Env),
			Fields: storage.UpdateIssueInput{
				Reason: strings.TrimSpace(*reason),
			},
		}
		if visited["title"] {
			value := *title
			in.Fields.Title = &value
		}
		in.Fields.Description = description.text
		in.Fields.Prompt = prompt.text
		in.Fields.Plan = plan.text
		if visited["type"] {
			value, err := model.ParseIssueType(*issueType)
			if err != nil {
				return err
			}
			in.Fields.IssueType = &value
		}
		if visited["priority"] {
			value, err := model.ParsePriorityName(*priority)
			if err != nil {
				return err
			}
			in.Fields.Priority = &value
		}
		if visited["assignee"] {
			// Update is a field write, not a claim: the explicit value is honored
			// verbatim and empty means clear. Session-identity resolution
			// (resolveIdentity) is a claim-time convenience that belongs to
			// `start` only — applying it here would silently turn an explicit
			// clear (or an explicit third-party assignee) into "assign to me".
			// A Start action built above already carries this same value, so the
			// transition and the field write cannot disagree. [LAW:no-silent-failure]
			value := strings.TrimSpace(*assignee)
			in.Fields.Assignee = &value
		}
		if visited["labels"] {
			value := splitCSV(*labels)
			in.Fields.Labels = &value
		}
		if visited["lane"] {
			value := strings.TrimSpace(*lane)
			in.Fields.Lane = &value
		}
		if in.IsEmpty() {
			return UsageError{Message: "lit update requires at least one field flag; " + usage}
		}
		applied, err := ap.Store.Apply(ctx, positional[0], in)
		if err != nil {
			return err
		}
		if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, ticketUpdatedOccasion(applied.Issue)); err != nil {
			return err
		}
		if err := printIssueSummary(inv.Stdout, applied.Issue); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

// frameLabel names a rank frame for a reader: an epic by its id, and the top
// level by what a reader actually sees there.
func frameLabel(f storage.Frame) string {
	if f == storage.TopLevel {
		return "the backlog"
	}
	return string(f)
}

// rankSetSubcommand is the one token that selects `lit rank`'s second surface.
// rankDispatch matches it and the registry advertises it from this one name, so
// completion cannot offer a subcommand dispatch does not honor — nor miss one it
// does. [LAW:one-source-of-truth]
const rankSetSubcommand = "set"

// rankDispatch picks between the two surfaces registered under `rank`:
// 'lit rank set <id1> <id2> ...' is a separate verb that establishes absolute
// order across N issues atomically. Issue IDs always carry a
// workspace-configured prefix (e.g. <prefix>-<n>), so the literal 'set' is
// unambiguous. The choice reads argv only, so `lit rank set --help` renders the
// rank-set surface with nothing open.
func rankDispatch(args []string) (appLeaf, []string) {
	if len(args) > 0 && args[0] == rankSetSubcommand {
		return rankSetLeaf(), args[1:]
	}
	return rankLeaf(), args
}

// rankUsage is rank's own sentence, read by both its body and the arity refusal
// at the seam. The choice structure (--top|--bottom|--above|--below) is local
// knowledge no flag list conveys: the flags are mutually exclusive and one is
// required, which "values are passed as flags" cannot say. [LAW:one-source-of-truth]
const rankUsage = "usage: lit rank <id> --top|--bottom|--above <id>|--below <id>"

func rankLeaf() appLeaf {
	fs := newCobraFlagSet("rank")
	_ = fs.Bool("top", false, "Move to highest rank")
	_ = fs.Bool("bottom", false, "Move to lowest rank")
	above := fs.String("above", "", "Rank above this issue ID")
	below := fs.String("below", "", "Rank below this issue ID")
	return appLeaf{fs: fs, positionals: 1, usage: rankUsage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: rankUsage}
		}
		visited := map[string]bool{}
		fs.Visit(func(flag *pflag.Flag) { visited[flag.Name] = true })
		modeCount := 0
		if visited["top"] {
			modeCount++
		}
		if visited["bottom"] {
			modeCount++
		}
		if visited["above"] {
			modeCount++
		}
		if visited["below"] {
			modeCount++
		}
		if modeCount != 1 {
			return model.ValidationError{Message: "exactly one of --top, --bottom, --above, --below is required"}
		}
		issueID := positional[0]
		// Relative ops resolve cross-frame requests to the comparable pair (a
		// child's stand-in is its epic); the move record carries what actually
		// happened so the substitution is reported, never silent.
		// [LAW:no-silent-failure]
		move := storage.RankMove{MovedID: issueID, AnchorID: issueID}
		// edgeWord is the user's own word for the end they asked for, and it is
		// empty for the relative verbs — which is what tells the reporting below
		// that `end` describes a real edge move rather than an unset zero value.
		edgeWord := ""
		var end storage.RankEnd
		var err error
		switch {
		case visited["top"]:
			edgeWord = "top"
			end, err = ap.Store.RankToTop(ctx, issueID)
		case visited["bottom"]:
			edgeWord = "bottom"
			end, err = ap.Store.RankToBottom(ctx, issueID)
		case visited["above"]:
			move, err = ap.Store.RankAbove(ctx, issueID, *above)
		case visited["below"]:
			move, err = ap.Store.RankBelow(ctx, issueID, *below)
		}
		if err != nil {
			return err
		}
		// Both edge outcomes read `end`, so both sit under the one discriminator
		// that says an edge verb ran and left it meaningful. The relative verbs
		// never assign it, and TopLevel being the zero Frame is not allowed to be
		// what keeps their reporting correct — that coincidence is one constant's
		// edit away from printing an empty frame at a reader.
		// [LAW:types-are-the-program]
		if edgeWord != "" {
			switch {
			// An issue already holding its frame's edge did not move. Printing the
			// summary as though it had is the one outcome a reader cannot tell from
			// success — the command exits 0 and shows the ticket either way.
			// [LAW:no-silent-failure]
			case !end.Moved:
				if _, err := fmt.Fprintf(inv.Stdout, "%s is already at the %s of %s; nothing to rank\n", issueID, edgeWord, frameLabel(end.Frame)); err != nil {
					return err
				}
				return emitBreadcrumb(inv.Stdout, "update")
			// --top/--bottom move within the issue's own frame, so a ticket inside
			// an epic goes to the head of that epic's children and nowhere in the
			// queue at large. Say which frame: the reader has just watched the
			// relative verbs explain their substitution, and an unqualified "moved
			// to the top" reads as the top of the backlog. [LAW:no-silent-failure]
			case end.Frame != storage.TopLevel:
				if _, err := fmt.Fprintf(inv.Stdout, "%s is inside %s; ranked it to the %s of %s's children, leaving the rest of the queue unchanged\n", issueID, end.Frame, edgeWord, end.Frame); err != nil {
					return err
				}
			}
		}
		namedAnchor := *above + *below // exactly one mode is set; empty for --top/--bottom
		// A relative op against an issue inside an epic ranks the epic as its
		// stand-in; report the substitution so it is never silent. [LAW:no-silent-failure]
		if move.MovedID != issueID {
			if _, err := fmt.Fprintf(inv.Stdout, "%s is inside %s; ranked the epic %s instead, leaving its internal order unchanged\n", issueID, move.MovedID, move.MovedID); err != nil {
				return err
			}
		}
		if namedAnchor != "" && move.AnchorID != namedAnchor {
			if _, err := fmt.Fprintf(inv.Stdout, "%s is inside %s; ranked relative to the epic %s instead\n", namedAnchor, move.AnchorID, move.AnchorID); err != nil {
				return err
			}
		}
		issue, err := ap.Store.GetIssue(ctx, move.MovedID)
		if err != nil {
			return err
		}
		if err := printIssueSummary(inv.Stdout, issue); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

// rankSetLeaf establishes absolute order across N issues by stacking them at
// the top in the given sequence: id1 becomes the topmost, id2 ranks just
// below, etc. Atomic: the store either applies all or none. IDs inside an
// epic resolve to the epic itself; the substitution is reported, never
// silent. [LAW:no-silent-failure]
func rankSetLeaf() appLeaf {
	fs := newCobraFlagSet("rank set")
	// Without this the refusal renders the unbounded sentinel — "takes any
	// number of positional arguments" while refusing one, which is both
	// self-contradicting and no act a caller can perform. The sentence is one
	// line below; the declaration and the body read it from here.
	const usage = "usage: lit rank set <id1> <id2> [<id3> ...]"
	return appLeaf{fs: fs, positionals: allPositionals, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) < 2 {
			return UsageError{Message: usage}
		}
		result, err := ap.Store.RankSet(ctx, positional)
		if err != nil {
			return err
		}
		if err := printRankSet(inv.Stdout, result); err != nil {
			return err
		}
		return emitBreadcrumb(inv.Stdout, "update")
	}}
}

// printRankSet reports an order a RankSet wrote: each name that resolved to
// its epic, then the order itself. `lit rank set` and a bulk file's
// `rank: top` documents write the same RankSet, so they report it in the same
// words. [LAW:one-source-of-truth]
func printRankSet(stdout io.Writer, result storage.RankSetResult) error {
	ranked := make([]string, len(result.Resolutions))
	for i, r := range result.Resolutions {
		ranked[i] = r.RankedID
	}
	for _, r := range result.Resolutions {
		if r.RankedID != r.NamedID {
			if _, err := fmt.Fprintf(stdout, "%s is inside %s; ranked the epic %s instead, leaving its internal order unchanged\n", r.NamedID, r.RankedID, r.RankedID); err != nil {
				return err
			}
		}
	}
	// Name the frame the stack landed in, for the reason the edge verbs do: this
	// verb anchors at the top of the representatives' own frame, so an
	// unqualified "at top" reads as the head of the backlog when the set was
	// three children leading one epic. [LAW:no-silent-failure]
	noun := "issues"
	if len(ranked) == 1 {
		noun = "issue"
	}
	_, err := fmt.Fprintf(stdout, "ranked %d %s at the top of %s in order: %s\n", len(ranked), noun, frameLabel(result.Frame), strings.Join(ranked, ", "))
	return err
}

// filterWorkableIssues keeps the leaves that are still work anyone might do.
// The nil-status test is the container test (an epic derives its state from its
// children and owns no status of its own); InPlay is the unfinished test.
// [LAW:one-source-of-truth] Reading status.Value != StateClosed here would be a
// second spelling of "unfinished" that ignores retention — defended today only
// by the callers' IncludeDeleted:false, which is a filter elsewhere, not an
// invariant here.
func filterWorkableIssues(issues []model.Issue) []model.Issue {
	filtered := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Capabilities().Status != nil && issue.InPlay() {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

// resolveIdentity returns the identity this lit invocation acts as — used for
// both the assignee (who owns the work) and the event actor (who performed the
// transition), which are the same agent. When CLAUDE_CODE_SESSION_ID is set the
// value is always claude_<sessionId>, regardless of what (if anything) the
// caller passed. When the env var is empty the caller's explicit --by value,
// trimmed, passes through.
// [LAW:one-source-of-truth] sole producer of the acting identity; assignee and
// actor cannot diverge because both derive from this one rule.
// [LAW:types-are-the-program] env presence is the discriminator; no callsite
// re-decides precedence or re-parses the env var.
func resolveIdentity(env environ.Env, explicit string) string {
	if sessionID := strings.TrimSpace(env.Get("CLAUDE_CODE_SESSION_ID")); sessionID != "" {
		return "claude_" + sessionID
	}
	return strings.TrimSpace(explicit)
}

// actorResolver yields the identity this lit invocation acts as. It is the only
// way to read the --by flag: registerActor captures the raw flag pointer and
// never exposes it, so every value a callsite can obtain has already passed
// through resolveIdentity. [LAW:single-enforcer] one boundary resolves the
// actor for every mutating command; [LAW:types-are-the-program] an unresolved
// --by cannot reach a CreatedBy/actor field, so a new mutating command
// cannot split provenance by forgetting to resolve.
type actorResolver func(env environ.Env) string

// byUsage is the one help line for --by, shown on every command that registers
// it, so the flag reads the same wherever an operator meets it.
const byUsage = "Who you are when CLAUDE_CODE_SESSION_ID is unset (when it is set, the session is who you are and --by is ignored): the actor a change records, the assignee lit start writes, and the reader lit next tells whether a ticket in flight is theirs"

// registerActor declares the --by flag on fs and returns the resolver that
// reads it. Call the returned resolver after parseFlagSet has run.
// [LAW:one-source-of-truth] --by is the one spelling of the session-less
// identity: the actor every mutating command records, the assignee `start`
// writes, and the reader `next` compares against, so an operator naming
// themselves once is the same person to the write and to the read.
// [LAW:one-source-of-truth] the fallback default lives here alone, not at each
// callsite. Absent both CLAUDE_CODE_SESSION_ID and --by it is "": the shared
// database syncs to remotes and admits no username-shaped values
// (design-docs/work-claims.md, "The privacy invariant"), and "" resolves
// through the store's empty-actor normalization (store.go Apply) to the opaque
// "unknown".
func registerActor(fs *cobraFlagSet) actorResolver {
	by := fs.String("by", "", byUsage)
	// --assignee was start's spelling of this identity before --by replaced it.
	// A command that registers its own --assignee (a filter, a field write)
	// parses it normally; everywhere else it is pointed here.
	fs.Retire("assignee", "--assignee does not name who is acting here; name yourself with --by <name> (CLAUDE_CODE_SESSION_ID wins when set), or change a ticket's assignee with `lit update <id> --assignee <name>`")
	return func(env environ.Env) string { return resolveIdentity(env, *by) }
}

// transitionBreadcrumbTopics maps transition actions to the quickstart topic
// whose guidance follows naturally from that success: claiming work points at
// the finding-work guidance, finishing (done or close) at the wrap-up
// guidance. Absence from the table is the encoding of "no natural follow-on
// topic" (archive, delete, reopen, ...), not a skipped branch.
// [LAW:dataflow-not-control-flow]
var transitionBreadcrumbTopics = map[model.ActionName]string{
	model.ActionStart: "work",
	model.ActionDone:  "done",
	model.ActionClose: "done",
}

// transitionSpec describes one lifecycle transition command: its name and how
// its parsed flags become the typed action value. Each command registers only
// the flags its action consumes, so flag misuse on any other transition is the
// parser's unknown-flag error. [LAW:types-are-the-program]
// [LAW:one-type-per-behavior] The nine transition commands are instances of
// one spec, not nine handlers; variability lives in the spec value.
type transitionSpec struct {
	name string
	// synopsis is the command's own arguments between the id and --reason,
	// each preceded by a space; empty for a transition that takes none. The
	// command's one usage line is built from it.
	synopsis string
	// registerFlags declares the action's own flags on fs and returns the
	// builder invoked after parsing, which turns the flag values and the
	// invocation's resolved identity into the sealed action variant. A flag
	// value it refuses is a UsageError quoting usage, the command's one usage
	// line. [LAW:one-source-of-truth]
	registerFlags func(fs *cobraFlagSet, usage string) func(actor string) (model.Action, error)
	// authorize, when set, runs after the action is built and before Apply,
	// and may abort the transition by returning an error. `start` supplies
	// the fresh-claim takeover gate (design-docs/work-claims.md,
	// links-claims-1ihf.7) and `phase` the facts of its move; every other
	// transition supplies noAuthorize. [LAW:single-enforcer] one hook point
	// for per-transition authorization, rather than a type switch on the
	// built action scattered through runTransition.
	//
	// Its result is what the transition owes AFTER Apply, decided on the
	// pre-Apply state the hook just read. For `start` that is whether the
	// handover Apply reports is announced, and the count of the lane, and both
	// are decided here rather than beside Apply because only this read knows
	// the lane: a start on a lane nobody holds hands nothing over, whatever the
	// row's history says about who once started it, since an expired claim is
	// not a claim, and reading the lane again would be a second gather of
	// every event in the store. [LAW:one-source-of-truth]
	authorize func(fs *cobraFlagSet) authorizeFunc
}

// authorizeFunc is a transition's gate ahead of Apply, run with the invocation
// so a gate that asks can read the answer from its stdin.
type authorizeFunc = func(ctx context.Context, inv Invocation, ap *app.App, issueID string, prior ticketState, action model.Action) (afterApply, error)

// afterApply is what a transition owes once Apply has written the issue: the
// line it prints and the facts it tells the statusline file. The authorize hook
// decides both on the state it read; what the write did completes them — where
// it left the ticket, its row and its phase, and the claim it handed over, as
// Apply reported it. An empty notice and no facts are the identity values for
// "nothing owed", so runTransition prints and records the result
// unconditionally. [LAW:dataflow-not-control-flow]
type afterApply func(written ticketState, handover *claims.Handover) (notice string, facts statusline.Facts)

// noAuthorize is the authorize hook of every transition that gates nothing
// beyond the ordinary state-machine check store.Apply already performs, and
// owes nothing after it.
func noAuthorize(*cobraFlagSet) authorizeFunc {
	return func(context.Context, Invocation, *app.App, string, ticketState, model.Action) (afterApply, error) {
		return func(ticketState, *claims.Handover) (string, statusline.Facts) { return "", nil }, nil
	}
}

// fixedAction is the registerFlags of every transition whose action carries no
// payload and therefore consumes no flags.
func fixedAction(action model.Action) func(*cobraFlagSet, string) func(string) (model.Action, error) {
	return func(*cobraFlagSet, string) func(string) (model.Action, error) {
		return func(string) (model.Action, error) { return action, nil }
	}
}

var (
	startSpec = transitionSpec{
		name:     "start",
		synopsis: " [--take]",
		// Start is the claim transition and the only action that carries an
		// assignee: whoever performs the start is who it assigns.
		// [LAW:one-source-of-truth] the assignee is the actor transitionLeaf
		// resolves once from the one --by flag, never a flag of its own.
		registerFlags: func(*cobraFlagSet, string) func(actor string) (model.Action, error) {
			return func(actor string) (model.Action, error) {
				return model.Start{Assignee: actor}, nil
			}
		},
		authorize: func(fs *cobraFlagSet) authorizeFunc {
			take := fs.Bool("take", false, "Confirm taking over a lane another checkout claims right now (required for non-interactive callers; without it an interactive terminal is prompted instead)")
			return func(ctx context.Context, inv Invocation, ap *app.App, issueID string, prior ticketState, action model.Action) (afterApply, error) {
				// registerFlags above builds every action this hook sees, so a
				// different variant is a wiring error, not a caller's mistake.
				// [LAW:no-silent-failure]
				start, ok := action.(model.Start)
				if !ok {
					return nil, fmt.Errorf("start: authorize hook received a %T action", action)
				}
				return authorizeStart(ctx, inv, ap, issueID, prior.issue, start, *take)
			}
		},
	}
	doneSpec = transitionSpec{
		name:          "done",
		registerFlags: fixedAction(model.Done{}),
		authorize: func(*cobraFlagSet) authorizeFunc {
			return authorizeDone
		},
	}
	closeSpec = transitionSpec{
		name:     "close",
		synopsis: " --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>]",
		registerFlags: func(fs *cobraFlagSet, usage string) func(string) (model.Action, error) {
			resolution, target := registerCloseOutcomeFlags(fs)
			return func(string) (model.Action, error) {
				outcome, err := closeOutcomeFromFlags(*resolution, *target, usage)
				if err != nil {
					return nil, err
				}
				return model.Close{Outcome: outcome}, nil
			}
		},
		authorize: noAuthorize,
	}
	openSpec  = transitionSpec{name: "open", registerFlags: fixedAction(model.Reopen{}), authorize: noAuthorize}
	phaseSpec = transitionSpec{
		name:     "phase",
		synopsis: " --to <" + model.JoinPhases("|") + ">",
		registerFlags: func(fs *cobraFlagSet, usage string) func(string) (model.Action, error) {
			to := fs.String("to", "", "Phase to move the ticket to (required): "+model.JoinPhases(", "))
			return func(string) (model.Action, error) {
				phase, err := model.ParsePhase(*to)
				if err != nil {
					return nil, UsageError{Message: fmt.Sprintf("%s\n%v", usage, err)}
				}
				return model.SetPhase{Phase: phase}, nil
			}
		},
		authorize: func(*cobraFlagSet) authorizeFunc {
			return authorizePhase
		},
	}
	archiveSpec   = transitionSpec{name: "archive", registerFlags: fixedAction(model.Archive{}), authorize: noAuthorize}
	unarchiveSpec = transitionSpec{name: "unarchive", registerFlags: fixedAction(model.Unarchive{}), authorize: noAuthorize}
	deleteSpec    = transitionSpec{name: "delete", registerFlags: fixedAction(model.Delete{}), authorize: noAuthorize}
	restoreSpec   = transitionSpec{name: "restore", registerFlags: fixedAction(model.Restore{}), authorize: noAuthorize}
)

// authorizePhase gates nothing — Issue.PlanPhase, which both engines run, is
// the phase rule — and owes the move it made: the line saying where the
// ticket's phase now stands, and on the command's event the phase left, the
// phase entered, and whether anything changed. The statusline file hears of
// the move from the store's change observer, as it hears of every change but a
// start. A refused move fails Apply, so its event carries the refusal instead.
// The phase left is the one transitionLeaf read before Apply on the same write
// Store the move is planned on, whose engine holds Dolt's LOCK for the whole
// command, so both reads answer from one history. [LAW:nothing-unseen]
func authorizePhase(ctx context.Context, _ Invocation, _ *app.App, _ string, prior ticketState, action model.Action) (afterApply, error) {
	set, ok := action.(model.SetPhase)
	if !ok {
		return nil, fmt.Errorf("phase: authorize hook received a %T action", action)
	}
	move := model.PhaseChange{From: prior.phase, To: set.Phase}
	return func(ticketState, *claims.Handover) (string, statusline.Facts) {
		telemetry.Set(ctx, "phase.from", string(move.From))
		telemetry.Set(ctx, "phase.to", string(move.To))
		telemetry.Set(ctx, "phase.changed", move.Changed())
		return phaseNotice(move), nil
	}, nil
}

// authorizeDone reads where the ticket's plan stands in its law review, the
// rule storage.PlanApproval holds, and finishes the ticket whatever it finds:
// review shapes the work, it never keeps a ticket open. It gates nothing — the
// lifecycle refusals, on an epic or on frozen work, are the store's Apply's
// alone. A plan short of approval prints a warning naming what is missing.
// done.plan_standing on the event says where the plan stood, and an approval
// adds the plan and the review it rested on; a done that found the ticket
// already closed finished nothing, so its event says that instead.
// [LAW:single-enforcer] [LAW:nothing-unseen]
func authorizeDone(ctx context.Context, _ Invocation, ap *app.App, issueID string, prior ticketState, action model.Action) (afterApply, error) {
	reviews, err := ap.Store.ListPlanReviews(ctx, issueID)
	if err != nil {
		return nil, err
	}
	// Resolved before the write, so a standing this switch does not know fails
	// the command with the ticket untouched. [LAW:no-silent-failure]
	standing := storage.PlanApproval(prior.issue, prior.events, reviews)
	// What the done owes once it lands, by standing: the facts it records and
	// the warning it prints.
	record, warning := func() {}, ""
	switch standing := standing.(type) {
	case storage.PlanApproved:
		record = func() {
			telemetry.Set(ctx, "done.plan_digest", string(standing.Review.PlanDigest))
			telemetry.Set(ctx, "done.plan_review", standing.Review.ID)
		}
	case storage.PlanGap:
		warning = "warning: finished without an approved plan: " + standing.Describe(issueID) + "\n"
	default:
		return nil, fmt.Errorf("done: unhandled plan standing %T", standing)
	}
	return func(written ticketState, _ *claims.Handover) (string, statusline.Facts) {
		if written.issue.StatusValue() == prior.issue.StatusValue() {
			telemetry.Set(ctx, "done.plan_standing", "already_closed")
			return "", nil
		}
		telemetry.Set(ctx, "done.plan_standing", standing.Kind())
		record()
		return warning, nil
	}, nil
}

// phaseNotice is the line `lit phase` prints before the ticket's summary: the
// move as history records it, or the phase the ticket was already in.
func phaseNotice(move model.PhaseChange) string {
	if !move.Changed() {
		return fmt.Sprintf("phase: %s (unchanged)\n", move.To)
	}
	return fmt.Sprintf("phase: %s → %s\n", move.From, move.To)
}

// registerCloseOutcomeFlags declares the close-outcome flags shared by `lit
// close` and `lit bulk close`, so the two boundaries expose the same surface.
// [LAW:single-enforcer]
func registerCloseOutcomeFlags(fs *cobraFlagSet) (resolution, target *string) {
	resolution = fs.String("resolution", "", "Close resolution (required): duplicate|superseded|obsolete|wontfix")
	target = fs.String("of", "", "Canonical ticket a duplicate/superseded close redirects to (required for those, rejected otherwise)")
	return resolution, target
}

// closeOutcomeFromFlags is the one flag boundary that turns untrusted
// (--resolution, --of) strings into the sealed Outcome sum: the resolution is
// parsed through model.ParseResolution, and the redirect target is required
// exactly for the redirecting resolutions and rejected for the terminal ones —
// after this gate, which outcomes carry a target is structural.
// [LAW:single-enforcer] `lit close` and `lit bulk close` both construct their
// outcome here, so the two commands cannot disagree about what a close
// requires.
func closeOutcomeFromFlags(resolution, target, usage string) (model.Outcome, error) {
	parsed, err := model.ParseResolution(resolution)
	if err != nil {
		return nil, UsageError{Message: fmt.Sprintf("%s\n%v", usage, err)}
	}
	trimmedTarget := strings.TrimSpace(target)
	if parsed.RedirectsToCanonical() {
		if trimmedTarget == "" {
			return nil, UsageError{Message: fmt.Sprintf("closing as %s redirects to a canonical ticket — name it with --of", parsed)}
		}
	} else if trimmedTarget != "" {
		return nil, UsageError{Message: fmt.Sprintf("--of applies only to duplicate/superseded closes, not %s", parsed)}
	}
	switch parsed {
	case model.ResolutionDuplicate:
		return model.Duplicate{Of: trimmedTarget}, nil
	case model.ResolutionSuperseded:
		return model.Superseded{By: trimmedTarget}, nil
	case model.ResolutionObsolete:
		return model.Obsolete{}, nil
	case model.ResolutionWontfix:
		return model.Wontfix{}, nil
	}
	// Unreachable: ParseResolution seals the set. Loud beats silent if the set
	// ever grows without this switch. [LAW:no-silent-failure]
	return nil, fmt.Errorf("resolution %q has no close outcome", parsed)
}

// readTicketState reads where issue stands: the phase its history puts it in,
// beside the row already read. [LAW:one-source-of-truth] the phase is never
// stored, so every reader derives it through model.PhaseOf from the events.
func readTicketState(ctx context.Context, st storage.Store, issue model.Issue) (ticketState, error) {
	events, err := st.ListEvents(ctx, issue.ID)
	if err != nil {
		return ticketState{}, err
	}
	return ticketStateOf(issue, events), nil
}

// ticketStateOf is where issue stands given its history: the row, and the
// phase events put it in, empty when it is not in progress.
func ticketStateOf(issue model.Issue, events []model.IssueEvent) ticketState {
	phase, _ := model.PhaseOf(issue, events)
	return ticketState{issue: issue, events: events, phase: phase}
}

// transitionDone is what one transition did to one ticket: where it found
// the ticket, where it left it, and the line the transition owes for it.
type transitionDone struct {
	prior, written ticketState
	notice         string
}

// applyTransition runs one transition on one ticket: the pre-transition read,
// the spec's authorize hook, Apply, and what the transition owes, with its
// statusline facts recorded. `lit <verb>` and `lit bulk <verb>` both run it,
// so a transition one refuses the other refuses with the same error.
// [LAW:single-enforcer]
func applyTransition(ctx context.Context, inv Invocation, ap *app.App, issueID string, action model.Action, actor, reason string, authorize authorizeFunc) (transitionDone, error) {
	// The pre-transition read is the state `authorize` gates on and the
	// before-half of the caller's workflow occasion: the row, and the phase
	// the ticket's history puts it in.
	priorIssue, err := ap.Store.GetIssue(ctx, issueID)
	if err != nil {
		return transitionDone{}, err
	}
	prior, err := readTicketState(ctx, ap.Store, priorIssue)
	if err != nil {
		return transitionDone{}, err
	}

	// [LAW:single-enforcer] The one gate every transition's authorization
	// passes through, ahead of Apply — a no-op for every transition but
	// `start`. See transitionSpec.authorize and authorizeStart
	// (claims_takeover.go). What it owes is decided on pre-Apply state and
	// completed after Apply, so a failed Apply announces and records
	// nothing.
	owed, err := authorize(ctx, inv, ap, issueID, prior, action)
	if err != nil {
		return transitionDone{}, err
	}

	applied, err := ap.Store.Apply(ctx, issueID, storage.Change{Action: action, Actor: actor, Reason: reason})
	if err != nil {
		return transitionDone{}, err
	}
	written, err := readTicketState(ctx, ap.Store, applied.Issue)
	if err != nil {
		return transitionDone{}, err
	}
	// Recorded after Apply, so these facts follow the ones the store's
	// change observer recorded for this same write.
	notice, facts := owed(written, applied.Handover)
	ap.Statusline.Record(facts...)
	return transitionDone{prior: prior, written: written, notice: notice}, nil
}

func transitionLeaf(spec transitionSpec) appLeaf {
	fs := newCobraFlagSet(spec.name)
	reason := fs.String("reason", "", "Transition reason")
	resolveActor := registerActor(fs)
	// The issue id, declared: the declared channel carries it and parseLeaf
	// refuses a second id rather than this leaf discovering it.
	// [LAW:one-source-of-truth]
	// One sentence, read by the seam, the body and the action's own flag
	// parsing. Hoisted out of the work because the arity refusal in parseLeaf
	// runs BEFORE any work does, so a usage line computed inside the closure is
	// a line the enforcer can never see. [LAW:one-source-of-truth]
	usage := fmt.Sprintf("usage: lit %s <id>%s [--reason <text>]", spec.name, spec.synopsis)
	buildAction := spec.registerFlags(fs, usage)
	authorize := spec.authorize(fs)
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			// UsageError, not errors.New: a missing id is the caller mis-writing
			// the command line, which exits 2 and says to re-read --help. A bare
			// error would exit 1 carrying "Retry the command. If it still
			// fails, run `lit doctor`" — advice that cannot work, since
			// retrying the same line fails the same way.
			// [LAW:types-are-the-program]
			return UsageError{Message: usage}
		}

		issueID := positional[0]

		// [LAW:single-enforcer] One resolution serves the action and the event:
		// the agent's session wins, else --by. History must record who actually
		// performed the transition (claude_<session>), not the shell user, and
		// the assignee `start` writes is that same value.
		// The action is built before the store is read: a mis-written command
		// line is a usage error whatever the id names. [LAW:parse-dont-validate]
		actor := resolveActor(inv.Env)
		action, err := buildAction(actor)
		if err != nil {
			return err
		}

		did, err := applyTransition(ctx, inv, ap, issueID, action, actor, *reason, authorize)
		if err != nil {
			return err
		}
		prior, written, issue := did.prior, did.written, did.written.issue

		// What the transition did is said first, ahead of any guidance about
		// what to do next: a hand-off and a phase move say it out loud, every
		// other transition says it as the empty string, so this write always
		// runs and only its value varies. The authorize hook holds the rule.
		// [LAW:dataflow-not-control-flow]
		if _, err := io.WriteString(inv.Stdout, did.notice); err != nil {
			return err
		}

		// [LAW:single-enforcer] transitionLeaf is the one lifecycle-transition
		// path (start/done/close/open and phase all arrive here), so it is the
		// one place that dispatches their events — no per-command
		// re-derivation. transitionOccasion decides which actions fire;
		// retention actions (archive/unarchive/delete/restore) fire nothing.
		// Dispatch is also where any matching workflow definition's guidance is
		// injected — `done`'s embedded default is what prints the post-close
		// capture reminder below.
		if occasion, fires := transitionOccasion(action, prior, written); fires {
			if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, occasion); err != nil {
				return err
			}
		}

		if err := printIssueSummary(inv.Stdout, issue); err != nil {
			return err
		}
		// At the capture moment a closing agent's freshest need is "which adjacent
		// tickets just became actionable or stale" — a relationship question the
		// one-line summary above answers with nothing. Render the live neighborhood
		// from the canonical graph, in the command already running.
		// [LAW:one-source-of-truth] "Closes the issue" is the variant's Target fact
		// (done and close both target Closed), not a re-enumerated action list here,
		// so a future closing action inherits this block for free.
		if statusAction, ok := action.(model.StatusAction); ok && statusAction.Target() == model.StateClosed {
			detail, err := ap.Store.GetIssueDetail(ctx, issueID)
			if err != nil {
				return err
			}
			if err := printCloseAdjacency(inv.Stdout, detail); err != nil {
				return err
			}
		}
		if topic, ok := transitionBreadcrumbTopics[action.Name()]; ok {
			return emitBreadcrumb(inv.Stdout, topic)
		}
		return nil
	}}
}

// assign is retired: reassigning is a single-field write, so it folds into
// `lit update <id> --assignee <name>` rather than earning its own verb — see the
// retired spec in register.go. `update` already resolves the actor through the
// shared identity rule and records the field-change event, so the fold loses
// nothing (including the optional --reason). [LAW:one-type-per-behavior]

var commentFamily = commandFamily[appSubcommand]{
	usage: "usage: lit comment <add|rm> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, declare: commentAddLeaf}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, declare: commentRmLeaf}},
	},
}

func commentAddLeaf() appLeaf {
	fs := newCobraFlagSet("comment add")
	body := registerLongText(fs, "body", "Comment body")
	resolveActor := registerActor(fs)
	const usage = "usage: lit comment add <id> (--body <text> | --body-file <path|->)"
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		commentBody, err := model.ParseCommentBody(body.value())
		if err != nil {
			return err
		}
		comment, issue, err := addComment(ctx, ap, positional[0], commentBody, resolveActor(inv.Env))
		if err != nil {
			return err
		}
		if err := dispatchOccasion(ctx, inv.Env, inv.Stdout, ap.Workspace, commentAddedOccasion(issue)); err != nil {
			return err
		}
		return printComment(inv.Stdout, comment)
	}}
}

// addComment is the comment write `lit comment add` and `lit bulk comment`
// both run, so a comment lands, and records its comment.id fact, the same way
// from either. The comment_added workflow occasion is the single verb's own, as
// every bulk verb leaves its single command's guidance unprinted.
// The author resolves through the same identity rule as every other actor.
// [LAW:single-enforcer] [LAW:one-type-per-behavior]
func addComment(ctx context.Context, ap *app.App, issueID string, body model.CommentBody, actor string) (model.Comment, model.Issue, error) {
	comment, issue, err := ap.Store.AddComment(ctx, storage.AddCommentInput{IssueID: issueID, Body: body, CreatedBy: actor})
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	telemetry.Set(ctx, "comment.id", comment.ID)
	return comment, issue, nil
}

func commentRmLeaf() appLeaf {
	fs := newCobraFlagSet("comment rm")
	const usage = "usage: lit comment rm <comment-id>"
	return appLeaf{fs: fs, positionals: 1, usage: usage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if len(positional) != 1 {
			return UsageError{Message: usage}
		}
		comment, err := ap.Store.DeleteComment(ctx, positional[0])
		if err != nil {
			return err
		}
		return printComment(inv.Stdout, comment)
	}}
}

func printComment(w io.Writer, c model.Comment) error {
	_, err := io.WriteString(w, commentLine(c))
	return err
}

// commentLine is the line a comment command prints for a comment: the issue
// it is on, then its own id.
func commentLine(c model.Comment) string {
	return c.IssueID + " " + c.ID + "\n"
}

func exportLeaf() appLeaf {
	fs := newCobraFlagSet("export")
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		export, err := ap.Store.Export(ctx)
		if err != nil {
			return err
		}
		// Export is JSON-only — there is no text representation of a full database export.
		return writeJSON(inv.Stdout, export)
	}}
}

// importUsage is shared by every usage error runImportTree can raise, so a
// malformed invocation always points at the same two accepted shapes.
const importUsage = "usage: lit import --path <tree-spec.json | bulk-file.yaml> (run `lit import --help` for both formats)"

// runImportTree reads --path and dispatches on its extension to one of two
// bulk-ingest formats sharing this one command surface (never two competing
// stories for "create many issues from a file"): a JSON tree spec
// (runImportTreeJSON) or a YAML bulk create/update file
// (runImportBulk). [LAW:dataflow-not-control-flow] the format is a value —
// the file's own extension — not a second flag or mode.
func importTreeLeaf() appLeaf {
	fs := newCobraFlagSet("import").Detail(helpText("import"))
	path := registerPath(fs, "path", "Path to a JSON tree-spec file or a YAML bulk create/update file")
	resolveActor := registerActor(fs)
	return appLeaf{fs: fs, positionals: 0, usage: importUsage, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		if path.path() == "" {
			return UsageError{Message: importUsage}
		}
		data, err := os.ReadFile(path.path())
		if err != nil {
			return fmt.Errorf("read import spec: %w", err)
		}
		switch strings.ToLower(filepath.Ext(path.path())) {
		case ".yaml", ".yml":
			return runImportBulk(ctx, inv.Stdout, ap, data, resolveActor(inv.Env))
		default:
			return runImportTreeJSON(ctx, inv.Stdout, ap, data, resolveActor(inv.Env))
		}
	}}
}

// runImportTreeJSON consumes a JSON tree spec and creates issues in
// dependency order as one batch, so a tree refused at any write writes
// nothing (see Store.ImportTree). The spec is an array of records; each
// carries a local_id used inside the spec to wire parent/blocked_by refs.
// Real issue IDs are generated at create time and printed as an ordered
// mapping.
//
// JSON shape (see storage.ImportTreeSpec):
//
//	[
//	  {"local_id": "epic-x", "title": "Build X", "type": "epic", "topic": "exporter", "priority": 0},
//	  {"local_id": "task-1", "parent": "epic-x", "title": "Design", "type": "task", "topic": "exporter", "priority": 0},
//	  {"local_id": "task-2", "parent": "epic-x", "blocked_by": ["task-1"], "title": "Build", "type": "task", "topic": "exporter", "priority": 0}
//	]
func runImportTreeJSON(ctx context.Context, stdout io.Writer, ap *app.App, data []byte, actor string) error {
	specs, err := storage.ParseImportTreeSpecs(data)
	if err != nil {
		return err
	}
	prefix, err := ap.Workspace.IssuePrefix.Mintable()
	if err != nil {
		return err
	}
	// Counted before the refusal is returned, as a bulk file is: the event
	// counts what landed either way. [LAW:nothing-unseen]
	result, err := ap.Store.ImportTree(ctx, prefix.Value(), actor, specs)
	telemetry.Set(ctx, "import.created", len(result.Created))
	telemetry.Set(ctx, "import.edges_added", result.EdgesAdded)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "imported %d issues\n", len(result.Created)); err != nil {
		return err
	}
	for _, m := range result.Created {
		if _, err := fmt.Fprintf(stdout, "  %s -> %s\n", m.Ref, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// runImportBulk consumes a multi-document YAML bulk file and creates or
// updates each document's issue (see Store.BulkApply and
// storage.BulkIssueSpec for the id-as-selector rule and the full field list).
//
// YAML shape — one document per issue, documents separated by `---`:
//
//	local_id: epic-x        # optional; lets a later document's parent/blocked_by name this one
//	title: Build X
//	type: epic
//	topic: x
//	---
//	title: Design
//	type: task
//	topic: x
//	parent: epic-x           # a local_id above, or an existing real issue ID
//	---
//	id: existing-issue-7     # present => update that issue instead of creating
//	title: Renamed
//	labels: [reviewed]
//	parent: epic-x           # optional; reparents the issue, and "" clears its parent
//	blocked_by: [epic-y]     # optional; the issue's whole blocked-by set, and [] empties it
//	rank: top                # optional; the file's rank: top documents are stacked, in file order, at the top of their frame
func runImportBulk(ctx context.Context, stdout io.Writer, ap *app.App, data []byte, actor string) error {
	specs, err := storage.ParseBulkSpecs(data)
	if err != nil {
		return err
	}
	prefix, err := ap.Workspace.IssuePrefix.Mintable()
	if err != nil {
		return err
	}
	// A refused file returns what it left written — nothing, unless its reset
	// failed too — so the event counts what landed either way.
	// [LAW:nothing-unseen]
	result, err := ap.Store.BulkApply(ctx, prefix.Value(), actor, specs)
	telemetry.Set(ctx, "import.created", len(result.Created))
	telemetry.Set(ctx, "import.updated", len(result.Updated))
	telemetry.Set(ctx, "import.fields_held", len(result.FieldsHeld))
	telemetry.Set(ctx, "import.reparented", len(result.Reparented))
	telemetry.Set(ctx, "import.parent_held", len(result.ParentHeld))
	telemetry.Set(ctx, "import.blocked_by_set", len(result.BlockedBySet))
	telemetry.Set(ctx, "import.blocked_by_held", len(result.BlockedByHeld))
	telemetry.Set(ctx, "import.edges_added", result.EdgesAdded)
	telemetry.Set(ctx, "import.edges_removed", result.EdgesRemoved)
	telemetry.Set(ctx, "import.ranked", len(result.Ranked.Resolutions))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "created %d issues\n", len(result.Created)); err != nil {
		return err
	}
	for _, m := range result.Created {
		if _, err := fmt.Fprintf(stdout, "  %s -> %s\n", m.Ref, m.ID); err != nil {
			return err
		}
	}
	if err := printIDs(stdout, fmt.Sprintf("updated %d issues", len(result.Updated)), result.Updated); err != nil {
		return err
	}
	if len(result.FieldsHeld) > 0 {
		if err := printIDs(stdout, fmt.Sprintf("%d issues already had the fields the file states", len(result.FieldsHeld)), result.FieldsHeld); err != nil {
			return err
		}
	}
	// A file that states no parent or blocked_by on an update changed no such
	// set, so there is nothing to report, as with an order it did not state.
	if len(result.Reparented)+len(result.ParentHeld) > 0 {
		if err := printIDs(stdout, fmt.Sprintf("reparented %d issues", len(result.Reparented)), result.Reparented); err != nil {
			return err
		}
		if err := printIDs(stdout, fmt.Sprintf("%d issues already had the parent the file states", len(result.ParentHeld)), result.ParentHeld); err != nil {
			return err
		}
	}
	if len(result.BlockedBySet)+len(result.BlockedByHeld) > 0 {
		if err := printIDs(stdout, fmt.Sprintf("set blocked_by on %d issues", len(result.BlockedBySet)), result.BlockedBySet); err != nil {
			return err
		}
		if err := printIDs(stdout, fmt.Sprintf("%d issues already had the blocked_by the file states", len(result.BlockedByHeld)), result.BlockedByHeld); err != nil {
			return err
		}
	}
	// A file that states no rank wrote no order, so there is none to report.
	if len(result.Ranked.Resolutions) == 0 {
		return nil
	}
	return printRankSet(stdout, result.Ranked)
}

// printIDs writes a heading line and one indented id per line beneath it.
func printIDs(stdout io.Writer, heading string, ids []string) error {
	if _, err := fmt.Fprintln(stdout, heading); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := fmt.Fprintf(stdout, "  %s\n", id); err != nil {
			return err
		}
	}
	return nil
}

func workspaceLeaf() wsLeaf {
	fs := newCobraFlagSet("workspace")
	return wsLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ws workspace.Info, positional []string) error {
		// [LAW:one-source-of-truth] The ordered slice is the single source of the
		// output's field order; each line is `key: value`, parseable by an agent
		// that needs one field (e.g. `lit workspace | sed -n 's/^traces_dir: //p'`).
		fields := []struct{ key, value string }{
			{"workspace_id", ws.WorkspaceID},
			{"issue_prefix", string(ws.IssuePrefix.Stored())},
			{"git_common_dir", ws.GitCommonDir},
			{"storage_dir", ws.StorageDir},
			{"database_path", ws.DatabasePath},
			{"dolt_repo_path", ws.DoltRepoPath},
			{"traces_dir", automationTraceDir(ws)},
		}
		for _, f := range fields {
			if _, err := fmt.Fprintf(inv.Stdout, "%s: %s\n", f.key, f.value); err != nil {
				return err
			}
		}
		return nil
	}}
}

// completionFamily is the single source of the supported shells: its rows both
// validate the `lit completion <shell>` argument and feed the completion
// command's own completion surface. The payload is the shell name; the renderer
// is selected by completionRenderer rather than stored here, because a render
// closure projects the command registry (commandSpecs), and the registry refers
// back to this family — holding the closure here would form a package
// initialization cycle. [LAW:dataflow-not-control-flow]
var completionFamily = commandFamily[string]{
	usage: "usage: lit completion <bash|zsh|fish>",
	subcommands: []subcommandRow[string]{
		{name: "bash", payload: "bash"},
		{name: "zsh", payload: "zsh"},
		{name: "fish", payload: "fish"},
	},
}

func runCompletion(ctx context.Context, stdout io.Writer, args []string) error {
	shell, err := completionFamily.resolve(ctx, args)
	if err != nil {
		return err
	}
	// The shell name is the whole surface — a completion script takes no flags —
	// but the surface still has to be PARSED, because that parse is what answers
	// `lit completion bash --help`. [LAW:single-enforcer] every advertised leaf
	// answers help through the one parse, this one included.
	fs := newCobraFlagSet("completion " + shell)
	if err := parseFlagSet(fs, args[1:], stdout); err != nil {
		return err
	}
	// The family's own line, not a derived sentence: a caller who typed a stray
	// token after `completion bash` needs the shells named, and the family is
	// where that list already lives. [LAW:one-source-of-truth]
	if err := refuseSurplusPositionals(fs, 0, completionFamily.usage); err != nil {
		return err
	}
	_, err = io.WriteString(stdout, completionRenderer(shell)())
	return err
}

func quickstartLeaf() wsLeaf {
	fs := newCobraFlagSet("quickstart")
	refresh := fs.Bool("refresh", false, "Refresh managed repo assets and report quickstart override status (never overwrites overrides)")
	eject := fs.StringOptional("eject", "all", "", "Eject embedded default(s) to the global override path (comma-separated short names; empty = all)")
	force := fs.Bool("force", false, "With --eject, overwrite existing override files")
	// The optional topic, declared. [LAW:single-enforcer]
	return wsLeaf{fs: fs, positionals: 1, usage: quickstartUsage, work: func(_ context.Context, inv Invocation, ws workspace.Info, positional []string) error {
		ejectChanged := fs.Changed("eject")
		ejectValue := *eject
		if ejectChanged && ejectValue == "" {
			ejectValue = "all"
		}
		if ejectChanged && *refresh {
			return UsageError{Message: "usage: --refresh and --eject are mutually exclusive"}
		}
		if *force && !ejectChanged {
			return UsageError{Message: "usage: --force is only valid with --eject"}
		}

		if len(positional) == 1 {
			// [LAW:dataflow-not-control-flow] Topic dispatch is a value lookup; every topic shares one render path.
			if *refresh || ejectChanged || *force {
				// This is where `lit quickstart --eject all` lands. `--eject`
				// carries an optional value, so pflag takes one only as
				// `--eject=all`; written `--eject all`, the `all` is not the
				// flag's value at all, it is the topic, and a topic renders on
				// its own. The --eject half is stated only when --eject is
				// actually in play: --refresh reaches this same branch.
				// When it IS in play both halves are needed, because
				// `--eject=all topic` and `--eject topic` are indistinguishable
				// by the time we see them.
				// [LAW:no-silent-failure] name the act that works, not the shape
				// of the mistake.
				advice := "a topic renders on its own"
				if ejectChanged {
					advice += ", and --eject takes its value as --eject=LIST"
				}
				return UsageError{Message: quickstartUsage + "; " + advice}
			}
			templateName, ok := quickstartTopicTemplate(positional[0])
			if !ok {
				return UsageError{Message: fmt.Sprintf("usage: unknown quickstart topic %q (must be one of: %s)", positional[0], strings.Join(quickstartTopicTokens(), ", "))}
			}
			guidance, err := renderQuickstartTopic(inv.Env, ws.RootDir, templateName)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(inv.Stdout, guidance)
			return err
		}

		if ejectChanged {
			results, err := ejectTemplates(inv.Env, ejectValue, *force)
			if err != nil {
				return err
			}
			return writeEjectReport(inv.Stdout, results, *force)
		}

		// [LAW:one-source-of-truth] Quickstart guidance is loaded from the managed quickstart template instead of being re-encoded in CLI data structures.
		guidance, err := renderQuickstartGuidance(inv.Env, ws.RootDir)
		if err != nil {
			return err
		}

		lines := []string{}
		if *refresh {
			// [LAW:single-enforcer] Quickstart refresh rewrites through the managed
			// asset writers, in the workspace the leaf already resolved.
			refreshReport, err := refreshQuickstartManagedAssets(inv.Env, ws)
			if err != nil {
				return err
			}
			lines = append(lines, formatQuickstartRefreshSummary(refreshReport), "")
		}
		lines = append(lines, guidance)
		_, err = fmt.Fprintln(inv.Stdout, strings.Join(lines, "\n"))
		return err
	}}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// priorityChoices renders the sealed priority vocabulary for flag help, derived
// from the canonical list so help text cannot drift from the parse gate.
// Mirrors issueTypeChoices. [LAW:one-source-of-truth]
func priorityChoices() string {
	priorities := model.Priorities()
	names := make([]string, len(priorities))
	for i, p := range priorities {
		names[i] = p.String()
	}
	return strings.Join(names, "|")
}

// issueTypeChoices renders the sealed vocabulary for flag help, derived from
// the canonical list so help text cannot drift from the parse gate.
// [LAW:one-source-of-truth]
func issueTypeChoices() string {
	types := model.IssueTypes()
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return strings.Join(names, "|")
}

func toSlice[S ~string](s S) []S {
	if s == "" {
		return nil
	}
	return []S{s}
}

func splitCSV(input string) []string {
	if strings.TrimSpace(input) == "" {
		return nil
	}
	parts := strings.Split(input, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
