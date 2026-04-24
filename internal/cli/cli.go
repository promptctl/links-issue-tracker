package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/config"
	"github.com/bmf/links-issue-tracker/internal/merge"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/query"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type outputMode string

const (
	outputModeText outputMode = "text"
	outputModeJSON outputMode = "json"
)

type outputModeWriter struct {
	io.Writer
	mode outputMode
}

type appAccessMode string

const (
	appAccessRead  appAccessMode = "read"
	appAccessWrite appAccessMode = "write"
)

var errHelpHandled = errors.New("help handled")

func (w outputModeWriter) linksOutputMode() outputMode {
	return w.mode
}

type outputModeProvider interface {
	linksOutputMode() outputMode
}

const (
	humanBootstrapHelp = "Human bootstrap command. Run once per repository/worktree setup before autonomous agent operations."
	agentCommandHelp   = "Agent-facing operational command."
)

func Run(ctx context.Context, stdout io.Writer, stderr io.Writer, args []string) error {
	normalizedArgs, resolvedOutputMode, err := parseGlobalOutputMode(args, stdout)
	if err != nil {
		return err
	}
	stdout = outputModeWriter{Writer: stdout, mode: resolvedOutputMode}
	root := newRootCommand(ctx, stdout, stderr)
	root.SetArgs(normalizedArgs)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SilenceErrors = true
	root.SilenceUsage = true
	err = root.ExecuteContext(ctx)
	if errors.Is(err, pflag.ErrHelp) || errors.Is(err, errHelpHandled) {
		return nil
	}
	return err
}

func newRootCommand(ctx context.Context, stdout io.Writer, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use: "lit",
		Long: strings.Join([]string{
			"Agent-native issue tracker",
			"",
			"Output mode:",
			"  default text",
			"  --json for machine-readable JSON output",
		}, "\n"),
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q", args[0])
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddGroup(
		&cobra.Group{ID: "bootstrap", Title: "Human Bootstrap"},
		&cobra.Group{ID: "operations", Title: "Agent Operations"},
		&cobra.Group{ID: "structure", Title: "Dependencies & Structure"},
		&cobra.Group{ID: "data", Title: "Sync & Data"},
		&cobra.Group{ID: "maintenance", Title: "Setup & Maintenance"},
		&cobra.Group{ID: "guidance", Title: "Guidance & Tooling"},
	)

	addGroupedPassthrough(root, "bootstrap", "init", "Initialize links", func(args []string) error {
		return runWithWorkspace(append([]string{"init"}, args...), func(ws workspace.Info) error {
			return runInit(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "guidance", "quickstart", "Agent quickstart workflow", func(args []string) error {
		return runWithWorkspace(append([]string{"quickstart"}, args...), func(ws workspace.Info) error {
			return runQuickstart(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "guidance", "completion", "Generate shell completion script", func(args []string) error {
		return runCompletion(stdout, args)
	})
	addGroupedPassthrough(root, "maintenance", "hooks", "Install git hook automation", func(args []string) error {
		if err := validateHooksCommandPath(args); err != nil {
			return err
		}
		return runWithWorkspace(append([]string{"hooks"}, args...), func(ws workspace.Info) error {
			return runHooks(stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "migrate", "Migrate from Beads to links", func(args []string) error {
		return runWithWorkspace(append([]string{"migrate"}, args...), func(ws workspace.Info) error {
			return runMigrate(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "data", "sync", "Mirror Dolt data through git remotes", func(args []string) error {
		if err := validateSyncCommandPath(args); err != nil {
			return err
		}
		return runWithWorkspace(append([]string{"sync"}, args...), func(ws workspace.Info) error {
			return runSync(ctx, stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "operations", "new", "Create an issue", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"new"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runNew(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "ready", "List open work by readiness and rank", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"ready"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runReady(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "ls", "List issues (rank by default)", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"ls"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runList(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "show", "Show issue details", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"show"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runShow(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "update", "Update issue fields", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"update"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runUpdate(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "rank", "Reorder an issue's rank", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"rank"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runRank(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "start", "Claim issue work", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"start"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "start")
		})
	})
	addGroupedPassthrough(root, "operations", "done", "Mark claimed work complete", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"done"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "done")
		})
	})
	addGroupedPassthrough(root, "operations", "close", "Close issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"close"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "close")
		})
	})
	addGroupedPassthrough(root, "operations", "open", "Reopen issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"open"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "reopen")
		})
	})
	addGroupedPassthrough(root, "operations", "archive", "Archive issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"archive"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "archive")
		})
	})
	addGroupedPassthrough(root, "operations", "delete", "Delete issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"delete"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "delete")
		})
	})
	addGroupedPassthrough(root, "operations", "unarchive", "Unarchive issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"unarchive"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "unarchive")
		})
	})
	addGroupedPassthrough(root, "operations", "restore", "Restore deleted issue(s)", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"restore"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runTransition(commandCtx, stdout, ap, args, "restore")
		})
	})
	addGroupedPassthrough(root, "operations", "comment", "Add issue comments", func(args []string) error {
		if err := validateCommentCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, appAccessWrite, append([]string{"comment"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runComment(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "label", "Manage labels", func(args []string) error {
		if err := validateLabelCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, appAccessWrite, append([]string{"label"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runLabel(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "parent", "Manage parent relationships", func(args []string) error {
		if err := validateParentCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, appAccessWrite, append([]string{"parent"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runParent(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "children", "List child issues by rank", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"children"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runChildren(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "structure", "dep", "Manage dependency edges", func(args []string) error {
		if err := validateDepCommandPath(args); err != nil {
			return err
		}
		accessMode := appAccessWrite
		if len(args) > 0 && args[0] == "ls" {
			accessMode = appAccessRead
		}
		return runWithApp(ctx, accessMode, append([]string{"dep"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runDep(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "export", "Export workspace snapshot", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"export"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runExport(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "workspace", "Show workspace metadata", func(args []string) error {
		return runWithWorkspace(append([]string{"workspace"}, args...), func(ws workspace.Info) error {
			return runWorkspace(stdout, ws, args)
		})
	})
	addGroupedPassthrough(root, "maintenance", "doctor", "Health check", func(args []string) error {
		return runWithApp(ctx, resolveDoctorAccessMode(args), append([]string{"doctor"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runDoctor(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "backup", "Backup snapshot operations", func(args []string) error {
		if err := validateBackupCommandPath(args); err != nil {
			return err
		}
		accessMode := appAccessWrite
		if len(args) > 0 && (args[0] == "create" || args[0] == "list") {
			accessMode = appAccessRead
		}
		return runWithApp(ctx, accessMode, append([]string{"backup"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runBackup(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "data", "recover", "Recover from backup or sync", func(args []string) error {
		return runWithApp(ctx, appAccessWrite, append([]string{"recover"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runRecover(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "bulk", "Bulk issue operations", func(args []string) error {
		if err := validateBulkCommandPath(args); err != nil {
			return err
		}
		return runWithApp(ctx, appAccessWrite, append([]string{"bulk"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runBulk(commandCtx, stdout, ap, args)
		})
	})
	return root
}

func addGroupedPassthrough(root *cobra.Command, groupID string, name string, summary string, run func(args []string) error) {
	cmd := newPassthroughCommand(name, summary, run)
	cmd.GroupID = groupID
	root.AddCommand(cmd)
}

func newPassthroughCommand(name string, summary string, run func(args []string) error) *cobra.Command {
	description := agentCommandHelp
	if name == "init" {
		description = humanBootstrapHelp
	}
	return &cobra.Command{
		Use:                name,
		Short:              summary,
		Long:               description,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(args)
		},
	}
}

func runWithPreflight(commandArgs []string, run func() error) error {
	_, _, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	return run()
}

func validateNestedCommandPath(args []string, usage string, commands ...string) error {
	// [LAW:single-enforcer] Nested command path validation is centralized here so invalid/help paths fail before startup side effects.
	if len(args) == 0 {
		return errors.New(usage)
	}
	subcommand := strings.TrimSpace(args[0])
	if subcommand == "" || subcommand == "--help" || subcommand == "-h" || strings.HasPrefix(subcommand, "-") {
		return errors.New(usage)
	}
	for _, command := range commands {
		if subcommand == command {
			return nil
		}
	}
	return errors.New(usage)
}

func validateHooksCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit hooks install [--json]", "install")
}

func validateCommentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit comment add <id> --body <text>", "add")
}

func runWithWorkspace(commandArgs []string, run func(workspace.Info) error) error {
	preflightWorkspace, hasPreflightWorkspace, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	// [LAW:one-source-of-truth] Reuse the preflight workspace resolution when available.
	ws := preflightWorkspace
	if !hasPreflightWorkspace {
		ws, err = resolveWorkspaceFromWD()
		if err != nil {
			return err
		}
	}
	return run(ws)
}

func runWithApp(ctx context.Context, accessMode appAccessMode, commandArgs []string, run func(context.Context, *app.App) error) error {
	_, _, err := enforceBeadsPreflight(commandArgs)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	// [LAW:single-enforcer] Cobra command registration owns app access selection so startup mode is declared once per entrypoint.
	var ap *app.App
	switch accessMode {
	case appAccessRead:
		ap, err = app.OpenForRead(ctx, cwd)
	default:
		ap, err = app.Open(ctx, cwd)
	}
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return err
	}
	defer ap.Close()
	return run(ctx, ap)
}

func enforceBeadsPreflight(commandArgs []string) (workspace.Info, bool, error) {
	if shouldBypassBeadsPreflight(commandArgs) {
		return workspace.Info{}, false, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Info{}, false, fmt.Errorf("get cwd: %w", err)
	}
	ws, resolveErr := workspace.Resolve(cwd)
	if resolveErr == nil {
		if err := requireBeadsMigrationPreflight(ws, commandArgs); err != nil {
			return workspace.Info{}, false, err
		}
		return ws, true, nil
	}
	if !errors.Is(resolveErr, workspace.ErrNotGitRepo) {
		return workspace.Info{}, false, resolveErr
	}
	return workspace.Info{}, false, nil
}

func resolveWorkspaceFromWD() (workspace.Info, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Info{}, fmt.Errorf("get cwd: %w", err)
	}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return workspace.Info{}, fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return workspace.Info{}, err
	}
	return ws, nil
}

func parseGlobalOutputMode(args []string, _ io.Writer) ([]string, outputMode, error) {
	// [LAW:single-enforcer] Exact global --json handling and legacy output flag rejection live in one parser path.
	// [LAW:one-source-of-truth] Text is the canonical default; only an explicit global --json flips the shared writer mode.
	mode := outputModeText
	index := 0
	for index < len(args) {
		switch {
		case args[index] == "--":
			index++
			goto done
		case args[index] == "--json":
			mode = outputModeJSON
			index++
		case args[index] == "--output":
			return nil, "", unsupportedOutputFlagError()
		default:
			if strings.HasPrefix(args[index], "--output=") {
				return nil, "", unsupportedOutputFlagError()
			}
			if args[index] == "--help" || args[index] == "-h" {
				goto done
			}
			goto done
		}
	}

done:
	return args[index:], mode, nil
}

type cobraFlagSet struct {
	cmd *cobra.Command
}

func newCobraFlagSet(use string) *cobraFlagSet {
	cmd := &cobra.Command{
		Use:           use,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.InitDefaultHelpFlag()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.Flags().SetOutput(io.Discard)
	return &cobraFlagSet{cmd: cmd}
}

func (fs *cobraFlagSet) SetOutput(w io.Writer) {
	fs.cmd.SetOut(w)
	fs.cmd.SetErr(w)
	fs.cmd.Flags().SetOutput(w)
}

func (fs *cobraFlagSet) Parse(args []string) error {
	return fs.cmd.ParseFlags(args)
}

func (fs *cobraFlagSet) String(name string, value string, usage string) *string {
	return fs.cmd.Flags().String(name, value, usage)
}

func (fs *cobraFlagSet) Bool(name string, value bool, usage string) *bool {
	return fs.cmd.Flags().Bool(name, value, usage)
}

func (fs *cobraFlagSet) Int(name string, value int, usage string) *int {
	return fs.cmd.Flags().Int(name, value, usage)
}

// StringOptional declares a string flag whose value is `defaultIfAbsent` when
// the flag is not passed, `defaultIfPresent` when the flag is passed with no
// value (e.g. `--eject`), or the caller-supplied value otherwise.
func (fs *cobraFlagSet) StringOptional(name, defaultIfPresent, defaultIfAbsent, usage string) *string {
	p := fs.cmd.Flags().String(name, defaultIfAbsent, usage)
	fs.cmd.Flags().Lookup(name).NoOptDefVal = defaultIfPresent
	return p
}

func (fs *cobraFlagSet) NArg() int {
	return fs.cmd.Flags().NArg()
}

func (fs *cobraFlagSet) Visit(fn func(*pflag.Flag)) {
	fs.cmd.Flags().Visit(fn)
}

func (fs *cobraFlagSet) Changed(name string) bool {
	return fs.cmd.Flags().Changed(name)
}

func (fs *cobraFlagSet) printHelp(helpOutput io.Writer) error {
	fs.SetOutput(helpOutput)
	if _, writeErr := fmt.Fprintf(helpOutput, "Usage of %s:\n", fs.cmd.Use); writeErr != nil {
		return writeErr
	}
	fs.cmd.Flags().PrintDefaults()
	return nil
}

func parseFlagSet(fs *cobraFlagSet, args []string, helpOutput io.Writer) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			// [LAW:single-enforcer] Flag help rendering is normalized in one Cobra parser path.
			if helpErr := fs.printHelp(helpOutput); helpErr != nil {
				return helpErr
			}
			return errHelpHandled
		}
		return err
	}
	if helpFlag := fs.cmd.Flags().Lookup("help"); helpFlag != nil && helpFlag.Changed {
		// [LAW:single-enforcer] Parsed help flags follow the same Cobra help rendering path as explicit help errors.
		if helpErr := fs.printHelp(helpOutput); helpErr != nil {
			return helpErr
		}
		return errHelpHandled
	}
	return nil
}

func unsupportedOutputFlagError() error {
	return errors.New("--output is no longer supported; use --json for JSON or omit it for text")
}

func runNew(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("new")
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	topic := fs.String("topic", "", "Required immutable issue topic slug")
	parentID := fs.String("parent", "", "Optional parent issue ID; child IDs become parentID.<n>")
	priority := fs.Int("priority", 2, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title: *title, Description: *description, IssueType: *issueType, Topic: *topic, ParentID: *parentID, Priority: *priority, Assignee: *assignee, Labels: splitCSV(*labels),
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runList(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("ls")
	status := fs.String("status", "", "Filter by status: open|in_progress|closed")
	issueType := fs.String("type", "", "Filter by issue type")
	assignee := fs.String("assignee", "", "Filter by assignee")
	priorityMin := fs.Int("priority-min", -1, "Minimum priority 0..4")
	priorityMax := fs.Int("priority-max", -1, "Maximum priority 0..4")
	search := fs.String("search", "", "Search title and description text")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	hasComments := fs.Bool("has-comments", false, "Only include issues with comments")
	includeArchived := fs.Bool("include-archived", false, "Include archived issues")
	includeDeleted := fs.Bool("include-deleted", false, "Include deleted issues")
	updatedAfter := fs.String("updated-after", "", "Only include issues updated at or after RFC3339 timestamp")
	updatedBefore := fs.String("updated-before", "", "Only include issues updated at or before RFC3339 timestamp")
	queryExpr := fs.String("query", "", "Query language: status:in_progress type:task priority<=2 has:comments text")
	sortExpr := fs.String("sort", "", "Sort fields, e.g. rank:asc,updated_at:desc")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	format := fs.String("format", "lines", "Output format: lines|table")
	limit := fs.Int("limit", 0, "Limit results")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) { visited[f.Name] = true })
	filter := store.ListIssuesFilter{
		Statuses:        toSlice(strings.TrimSpace(*status)),
		IssueTypes:      toSlice(strings.TrimSpace(*issueType)),
		Assignees:       toSlice(strings.TrimSpace(*assignee)),
		IncludeArchived: *includeArchived,
		IncludeDeleted:  *includeDeleted,
		Limit:           *limit,
	}
	if strings.TrimSpace(*sortExpr) != "" {
		sortSpecs, err := parseSortSpecs(*sortExpr)
		if err != nil {
			return err
		}
		filter.SortBy = sortSpecs
	}
	if visited["priority-min"] {
		value := *priorityMin
		filter.PriorityMin = &value
	}
	if visited["priority-max"] {
		value := *priorityMax
		filter.PriorityMax = &value
	}
	if visited["search"] {
		filter.SearchTerms = append(filter.SearchTerms, strings.TrimSpace(*search))
	}
	if visited["ids"] {
		filter.IDs = splitCSV(*ids)
	}
	if visited["labels"] {
		filter.LabelsAll = splitCSV(*labels)
	}
	if visited["has-comments"] {
		value := *hasComments
		filter.HasComments = &value
	}
	if visited["updated-after"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedAfter))
		if err != nil {
			return fmt.Errorf("parse --updated-after: %w", err)
		}
		filter.UpdatedAfter = &parsed
	}
	if visited["updated-before"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedBefore))
		if err != nil {
			return fmt.Errorf("parse --updated-before: %w", err)
		}
		filter.UpdatedBefore = &parsed
	}
	if strings.TrimSpace(*queryExpr) != "" {
		parsed, err := query.Parse(*queryExpr)
		if err != nil {
			return err
		}
		filter, err = query.Merge(filter, parsed.Filter)
		if err != nil {
			return err
		}
	}
	// [LAW:dataflow-not-control-flow] Default status filter is data, not a branch
	// around ListIssues. When the user hasn't narrowed by status (via --status or
	// --query status:...), exclude closed issues so `lit ls` shows active work.
	if len(filter.Statuses) == 0 {
		filter.Statuses = []string{"open", "in_progress"}
	}
	issues, err := ap.Store.ListIssues(ctx, filter)
	if err != nil {
		return err
	}
	columns := parseColumns(*columnsExpr)
	formatMode := strings.ToLower(strings.TrimSpace(*format))
	return printValue(stdout, issues, *jsonOut, func(w io.Writer, v any) error {
		list := v.([]model.Issue)
		switch formatMode {
		case "", "lines":
			return printIssueLines(w, list, columns)
		case "table":
			return printIssueTable(w, list, columns)
		default:
			return fmt.Errorf("unsupported --format %q", formatMode)
		}
	})
}

func runReady(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("ready")
	assignee := fs.String("assignee", "", "Filter by assignee")
	limit := fs.Int("limit", 0, "Limit results")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit ready [--assignee <user>] [--limit N] [--columns ...] [--json]")
	}
	cfg, err := config.Load(ap.Workspace.RootDir)
	if err != nil {
		return err
	}
	// [LAW:one-source-of-truth] rank is the canonical ordering; no explicit SortBy
	// needed — the store default is item_rank ASC.
	issues, err := ap.Store.ListIssues(ctx, store.ListIssuesFilter{
		Statuses: []string{"open", "in_progress"},
		// [LAW:dataflow-not-control-flow] Epics never enter the ready pipeline;
		// agents can't `lit start` a container, so surfacing one would force
		// prose-driven drill rules to translate epic->child. Filter at the data
		// boundary instead. (links-agent-epic-model-uew.1)
		ExcludeIssueTypes: []string{"epic"},
		Assignees:         toSlice(strings.TrimSpace(*assignee)),
		IncludeArchived:   false,
		IncludeDeleted:    false,
		Limit:             0,
	})
	if err != nil {
		return err
	}
	fieldAnnotator, err := newFieldAnnotator(cfg.Ready.RequiredFields)
	if err != nil {
		return err
	}
	// [LAW:single-enforcer] One pre-pass fetches per-row detail for the whole
	// ready pipeline; both annotation and enrichment read from this map,
	// eliminating the duplicate GetIssueDetail traffic that landed when
	// enrichWithParentEpic was introduced alongside newBlockerAnnotator.
	details, err := fetchIssueDetails(ctx, ap.Store, issues)
	if err != nil {
		return err
	}
	annotated, err := annotation.Annotate(ctx, issues,
		fieldAnnotator,
		newBlockerAnnotator(details),
		newOrphanedAnnotator(24*time.Hour),
	)
	if err != nil {
		return err
	}
	sortByReadiness(annotated)
	annotated = applyLimit(annotated, *limit)
	enrichWithParentEpic(annotated, details)
	columns := parseColumns(*columnsExpr)
	return printValue(stdout, annotated, *jsonOut, func(w io.Writer, v any) error {
		return printReadyOutput(w, columns, v.([]annotation.AnnotatedIssue))
	})
}

func runShow(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("show")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit show <id>")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit show <id>")
	}
	detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
	if err != nil {
		return err
	}
	return printValue(stdout, detail, *jsonOut, func(w io.Writer, v any) error {
		return printIssueDetail(w, v.(model.IssueDetail))
	})
}

type statusTransitionKey struct {
	From string
	To   string
}

var updateStatusTransitionActions = map[statusTransitionKey]string{
	{From: "open", To: "in_progress"}:   "start",
	{From: "in_progress", To: "closed"}: "done",
	{From: "open", To: "closed"}:        "close",
	{From: "closed", To: "open"}:        "reopen",
	{From: "closed", To: "in_progress"}: "reopen+start",
	{From: "in_progress", To: "open"}:   "done+reopen",
}

func runUpdate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("update")
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	issueType := fs.String("type", "", "Issue type: task|feature|bug|chore|epic")
	priority := fs.Int("priority", 0, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	status := fs.String("status", "", "Status: open|in_progress|closed")
	reason := fs.String("reason", "", "Status transition reason")
	by := fs.String("by", os.Getenv("USER"), "Status transition actor")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit update <id> [--title <text>] [--description <text>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--status <open|in_progress|closed>] [--reason <text>] [--by <user>] [--json]")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit update <id> [--title <text>] [--description <text>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--status <open|in_progress|closed>] [--reason <text>] [--by <user>] [--json]")
	}
	visited := map[string]bool{}
	fs.Visit(func(flag *pflag.Flag) { visited[flag.Name] = true })
	if visited["reason"] && !visited["status"] {
		return errors.New("--reason requires --status")
	}
	if visited["by"] && !visited["status"] {
		return errors.New("--by requires --status")
	}
	mutatesFields := visited["title"] || visited["description"] || visited["type"] || visited["priority"] || visited["assignee"] || visited["labels"]
	mutatesStatus := visited["status"]
	if !mutatesFields && !mutatesStatus {
		return errors.New("lit update requires at least one field flag")
	}

	issueID := positional[0]
	var issue model.Issue

	if mutatesStatus {
		targetStatus, err := store.NormalizeStatusToken(*status)
		if err != nil {
			return err
		}
		if targetStatus == "" {
			return errors.New("--status requires a non-empty value")
		}
		current, err := ap.Store.GetIssue(ctx, issueID)
		if err != nil {
			return err
		}
		// [LAW:one-source-of-truth] Status changes are normalized to canonical lifecycle transitions instead of writing status directly.
		actions, err := statusTransitionActionsForUpdate(current.Status, targetStatus)
		if err != nil {
			return err
		}
		transitionReason := strings.TrimSpace(*reason)
		if transitionReason == "" {
			transitionReason = fmt.Sprintf("status update via lit update: %s -> %s", current.Status, targetStatus)
		}
		issue = current
		// [LAW:dataflow-not-control-flow] Transition execution order is fixed; data determines whether action slice is empty.
		for _, action := range actions {
			transitioned, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:   issueID,
				Action:    action,
				Reason:    transitionReason,
				CreatedBy: *by,
			})
			if err != nil {
				return err
			}
			issue = transitioned
		}
	}

	if mutatesFields {
		update := store.UpdateIssueInput{}
		if visited["title"] {
			value := *title
			update.Title = &value
		}
		if visited["description"] {
			value := *description
			update.Description = &value
		}
		if visited["type"] {
			value := *issueType
			update.IssueType = &value
		}
		if visited["priority"] {
			value := *priority
			update.Priority = &value
		}
		if visited["assignee"] {
			value := *assignee
			update.Assignee = &value
		}
		if visited["labels"] {
			value := splitCSV(*labels)
			update.Labels = &value
		}
		updated, err := ap.Store.UpdateIssue(ctx, issueID, update)
		if err != nil {
			return err
		}
		issue = updated
	}

	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runRank(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("rank")
	_ = fs.Bool("top", false, "Move to highest rank")
	_ = fs.Bool("bottom", false, "Move to lowest rank")
	above := fs.String("above", "", "Rank above this issue ID")
	below := fs.String("below", "", "Rank below this issue ID")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit rank <id> --top|--bottom|--above <id>|--below <id>")
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
		return errors.New("exactly one of --top, --bottom, --above, --below is required")
	}
	issueID := positional[0]
	var err error
	switch {
	case visited["top"]:
		err = ap.Store.RankToTop(ctx, issueID)
	case visited["bottom"]:
		err = ap.Store.RankToBottom(ctx, issueID)
	case visited["above"]:
		err = ap.Store.RankAbove(ctx, issueID, *above)
	case visited["below"]:
		err = ap.Store.RankBelow(ctx, issueID, *below)
	}
	if err != nil {
		return err
	}
	issue, err := ap.Store.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func statusTransitionActionsForUpdate(fromStatus string, toStatus string) ([]string, error) {
	if fromStatus == toStatus {
		return nil, nil
	}
	action, exists := updateStatusTransitionActions[statusTransitionKey{From: fromStatus, To: toStatus}]
	if !exists {
		return nil, fmt.Errorf("unsupported status transition %q -> %q for lit update", fromStatus, toStatus)
	}
	return strings.Split(action, "+"), nil
}

func runTransition(ctx context.Context, stdout io.Writer, ap *app.App, args []string, action string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet(action)
	reason := fs.String("reason", "", "Transition reason")
	by := fs.String("by", os.Getenv("USER"), "Transition actor")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: lit %s <id> [--reason <text>]", transitionCommandName(action))
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit %s <id> [--reason <text>]", transitionCommandName(action))
	}
	issue, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID:   positional[0],
		Action:    action,
		Reason:    *reason,
		CreatedBy: *by,
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runComment(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	positional, flagArgs := splitArgs(args[1:], 1)
	fs := newCobraFlagSet("comment add")
	body := fs.String("body", "", "Comment body")
	by := fs.String("by", os.Getenv("USER"), "Comment author")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	comment, err := ap.Store.AddComment(ctx, store.AddCommentInput{IssueID: positional[0], Body: *body, CreatedBy: *by})
	if err != nil {
		return err
	}
	return printValue(stdout, comment, *jsonOut, func(w io.Writer, v any) error {
		c := v.(model.Comment)
		_, err := fmt.Fprintf(w, "%s %s\n", c.IssueID, c.ID)
		return err
	})
}

func runExport(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("export")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	export, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	// Export is JSON-only — there is no text representation of a full database export.
	return writeJSON(stdout, export)
}

func runWorkspace(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("workspace")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	payload := map[string]string{
		"workspace_id":   ws.WorkspaceID,
		"issue_prefix":   ws.IssuePrefix,
		"git_common_dir": ws.GitCommonDir,
		"storage_dir":    ws.StorageDir,
		"database_path":  ws.DatabasePath,
		"dolt_repo_path": ws.DoltRepoPath,
		"traces_dir":     automationTraceDir(ws),
	}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		for _, key := range []string{"workspace_id", "issue_prefix", "git_common_dir", "storage_dir", "database_path", "dolt_repo_path", "traces_dir"} {
			if _, err := fmt.Fprintf(w, "%s: %s\n", key, p[key]); err != nil {
				return err
			}
		}
		return nil
	})
}

type errorDetailer interface {
	ErrorDetails() map[string]any
}

func runCompletion(stdout io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
	switch args[0] {
	case "bash":
		_, err := io.WriteString(stdout, bashCompletionScript)
		return err
	case "zsh":
		_, err := io.WriteString(stdout, zshCompletionScript)
		return err
	case "fish":
		_, err := io.WriteString(stdout, fishCompletionScript)
		return err
	default:
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
}

func runQuickstart(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	_ = ctx
	fs := newCobraFlagSet("quickstart")
	refresh := fs.Bool("refresh", false, "Refresh managed repo assets")
	eject := fs.StringOptional("eject", "all", "", "Eject embedded default(s) to the global override path (comma-separated: quickstart,agents,hook; empty = all)")
	force := fs.Bool("force", false, "With --eject, overwrite existing override files")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit quickstart [--refresh] [--eject[=LIST]] [--force]")
	}
	if outputModeFromWriter(stdout) == outputModeJSON {
		return errors.New("usage: lit quickstart [--refresh] [--eject[=LIST]] [--force]")
	}
	ejectChanged := fs.Changed("eject")
	ejectValue := *eject
	if ejectChanged && ejectValue == "" {
		ejectValue = "all"
	}
	if ejectChanged && *refresh {
		return errors.New("usage: --refresh and --eject are mutually exclusive")
	}
	if *force && !ejectChanged {
		return errors.New("usage: --force is only valid with --eject")
	}

	if ejectChanged {
		results, err := ejectTemplates(ejectValue, *force)
		if err != nil {
			return err
		}
		return writeEjectReport(stdout, results, *force)
	}

	// [LAW:one-source-of-truth] Quickstart guidance is loaded from the managed quickstart template instead of being re-encoded in CLI data structures.
	guidance, err := renderQuickstartGuidance(ws.RootDir)
	if err != nil {
		return err
	}

	lines := []string{}
	if *refresh {
		// [LAW:single-enforcer] Quickstart refresh resolves the workspace once and delegates all file rewrites to the managed asset writers.
		ws, err := workspace.Resolve(".")
		if err != nil {
			return err
		}
		refreshReport, err := refreshQuickstartManagedAssets(ws)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("refresh %s", formatQuickstartRefreshSummary(refreshReport)), "")
	}
	lines = append(lines, guidance)
	_, err = fmt.Fprintln(stdout, strings.Join(lines, "\n"))
	return err
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func outputModeFromWriter(w io.Writer) outputMode {
	provider, ok := w.(outputModeProvider)
	if !ok {
		return outputModeText
	}
	return provider.linksOutputMode()
}

// printValue is the single enforcer for output format selection.
// [LAW:single-enforcer] Commands with both JSON and text modes go through this function
// to guarantee that both paths receive the same data. JSON-only commands (e.g., export)
// may call writeJSON directly since there is no text path to diverge from.
func printValue(w io.Writer, v any, jsonOut bool, textFn func(io.Writer, any) error) error {
	if jsonOut || outputModeFromWriter(w) == outputModeJSON {
		return writeJSON(w, v)
	}
	return textFn(w, v)
}

func toSlice(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func transitionCommandName(action string) string {
	switch action {
	case "reopen":
		return "open"
	default:
		return action
	}
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

func parseSortSpecs(input string) ([]store.SortSpec, error) {
	parts := splitCSV(input)
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]store.SortSpec, 0, len(parts))
	for _, part := range parts {
		spec := strings.TrimSpace(part)
		field := spec
		desc := false
		if strings.Contains(spec, ":") {
			chunks := strings.SplitN(spec, ":", 2)
			field = strings.TrimSpace(chunks[0])
			direction := strings.ToLower(strings.TrimSpace(chunks[1]))
			switch direction {
			case "asc":
				desc = false
			case "desc":
				desc = true
			default:
				return nil, fmt.Errorf("unsupported sort direction %q", direction)
			}
		}
		out = append(out, store.SortSpec{Field: field, Desc: desc})
	}
	return out, nil
}

type MergeConflictError struct {
	Message   string
	Conflicts []merge.IssueConflict
}

func (e MergeConflictError) Error() string {
	return e.Message
}

type CorruptionError struct {
	Message string
}

func (e CorruptionError) Error() string { return e.Message }

func printUsage(w io.Writer) {
	fmt.Fprint(w, `links / lit

Agent-native issue tracker

Output:
  default text
  --json                      Output machine-readable JSON.

Usage:
  lit [--json] [command]
  lit [--json] [command] [flags]

Sync Branch:
  default        remote default branch (resolved from git remote HEAD)
  debug override LINKS_DEBUG_DOLT_SYNC_BRANCH

Sync Remote (pull/push):
  default        upstream remote, else single configured remote
  no match       skip sync without Dolt side effects

Issue Workflow:
  init           Initialize links in the current repository (auto-migrates Beads residue)
  ready          List open work ordered by readiness, then rank
  new            Create an issue
  ls             List issues with filters/query/sort
  show           Show issue details
  update         Update issue fields
  start          Claim work (open -> in_progress)
  done           Mark work complete (in_progress -> closed)
  close          Close issue(s)
  open           Reopen issue(s)
  archive        Archive issue(s)
  delete         Soft-delete issue(s)
  unarchive      Unarchive issue(s)
  restore        Restore deleted issue(s)
  comment        Add issue comments
  label          Add/remove issue labels
  bulk           Bulk issue operations (label, close, archive, import)

Dependencies & Structure:
  parent         Manage parent/child links
  children       List child issues
  dep            Manage dependency edges

Sync & Data:
  export         Export workspace snapshot JSON
  sync           Mirror Dolt data through git remotes
  backup         Create/list/restore backup snapshots
  recover        Recover from sync file or backup

Setup & Maintenance:
  workspace      Show workspace metadata
  hooks          Install git hook automation
  migrate        Migrate from Beads to links
  doctor         Health check and repair

Guidance & Tooling:
  quickstart     Agent quickstart workflow
  completion     Generate shell completion script
  help           Show this help output

Command Syntax:
  lit init [--json] [--skip-hooks] [--skip-agents]
  lit ready [--assignee <user>] [--limit N] [--columns ...] [--json]
  lit update <id> [--title <text>] [--description <text>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--status <open|in_progress|closed>] [--reason <text>] [--by <user>] [--json]
  lit start <id> [--reason <text>] [--by <user>] [--json]
  lit done <id> [--reason <text>] [--by <user>] [--json]
  lit hooks install [--json]
  lit migrate [--apply] [--json] [--skip-hooks] [--skip-agents]
  lit quickstart [--refresh]
  lit completion <bash|zsh|fish>
  lit workspace [--json]
  lit sync remote ls [--json]
  lit sync fetch [--remote <name>] [--prune] [--verbose] [--json]
  lit sync pull [--remote <name>] [--verbose] [--json]
  lit sync push [--remote <name>] [--set-upstream] [--force] [--verbose] [--json]

Examples:
  lit init
  lit ready
  lit update <issue-id> --status in_progress
  lit start <issue-id>
  lit done <issue-id>
  lit new --title "Fix renderer race" --type bug --priority 1
  lit ls --query "status:open type:task"

Use "lit [command] --help" for more information about a command.
`)
}

func splitArgs(args []string, positionalCount int) ([]string, []string) {
	positionals := make([]string, 0, positionalCount)
	flags := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				flags = append(flags, args[index+1])
				index++
			}
			continue
		}
		if len(positionals) < positionalCount {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
	}
	return positionals, flags
}
