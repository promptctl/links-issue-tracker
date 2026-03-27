package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/backup"
	"github.com/bmf/links-issue-tracker/internal/config"
	"github.com/bmf/links-issue-tracker/internal/merge"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/query"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/syncfile"
	"github.com/bmf/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var missingRemoteBranchPattern = regexp.MustCompile(`branch "([^"]+)" not found on remote`)

const debugSyncBranchEnvVar = "LINKS_DEBUG_DOLT_SYNC_BRANCH"

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
		Use:   "lit",
		Short: "Worktree-native issue tracker",
		Long: strings.Join([]string{
			"Worktree-native issue tracker with Dolt-backed sync.",
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
	addGroupedPassthrough(root, "operations", "ready", "List open work", func(args []string) error {
		return runWithApp(ctx, appAccessRead, append([]string{"ready"}, args...), func(commandCtx context.Context, ap *app.App) error {
			return runReady(commandCtx, stdout, ap, args)
		})
	})
	addGroupedPassthrough(root, "operations", "ls", "List issues", func(args []string) error {
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
	addGroupedPassthrough(root, "structure", "children", "List child issues", func(args []string) error {
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

func validateSyncCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit sync <status|remote|fetch|pull|push> ...", "status", "remote", "fetch", "pull", "push")
}

func validateCommentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit comment add <id> --body <text>", "add")
}

func validateLabelCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit label <add|rm> ...", "add", "rm")
}

func validateParentCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit parent <set|clear> ...", "set", "clear")
}

func validateDepCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit dep <add|rm|ls> ...", "add", "rm", "ls")
}

func validateBackupCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit backup <create|list|restore> ...", "create", "list", "restore")
}

func validateBulkCommandPath(args []string) error {
	return validateNestedCommandPath(args, "usage: lit bulk <label|close|archive|import> ...", "label", "close", "archive", "import")
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

func resolveDoctorAccessMode(args []string) appAccessMode {
	cmd := &cobra.Command{Use: "doctor"}
	fix := cmd.Flags().String("fix", "", "")
	cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	if err := cmd.ParseFlags(args); err != nil {
		return appAccessWrite
	}
	if *fix != "" {
		return appAccessWrite
	}
	return appAccessRead
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

func (fs *cobraFlagSet) NArg() int {
	return fs.cmd.Flags().NArg()
}

func (fs *cobraFlagSet) Visit(fn func(*pflag.Flag)) {
	fs.cmd.Flags().Visit(fn)
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
	sortExpr := fs.String("sort", "", "Sort fields, e.g. priority:asc,updated_at:desc")
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
		Statuses:        []string{"open", "in_progress"},
		Assignees:       toSlice(strings.TrimSpace(*assignee)),
		IncludeArchived: false,
		IncludeDeleted:  false,
		Limit:           0,
	})
	if err != nil {
		return err
	}
	fieldAnnotator, err := newFieldAnnotator(cfg.Ready.RequiredFields)
	if err != nil {
		return err
	}
	annotated, err := annotation.Annotate(ctx, issues,
		fieldAnnotator,
		newBlockerAnnotator(ap.Store),
		newOrphanedAnnotator(24*time.Hour),
	)
	if err != nil {
		return err
	}
	sortByReadiness(annotated)
	annotated = applyLimit(annotated, *limit)
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
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
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

func runDep(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit dep <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("dep add")
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit dep add <issue-id> <depends-on-id> [--type blocks|parent-child|related-to]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep add <issue-id> <depends-on-id> [--type blocks|parent-child|related-to]")
		}
		rel, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: positional[0], DstID: positional[1], Type: *relType, CreatedBy: *by})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			r := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --depends-on--> %s\n", r.SrcID, r.DstID)
			return err
		})
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("dep rm")
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit dep rm <issue-id> <depends-on-id> [--type ...]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep rm <issue-id> <depends-on-id> [--type ...]")
		}
		if err := ap.Store.RemoveRelation(ctx, positional[0], positional[1], *relType); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	case "ls":
		positional, flagArgs := splitArgs(args[1:], 1)
		fs := newCobraFlagSet("dep ls")
		relType := fs.String("type", "", "Filter relation type")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		relations, err := ap.Store.ListRelationsForIssue(ctx, positional[0], *relType)
		if err != nil {
			return err
		}
		return printValue(stdout, relations, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]model.Relation)
			for _, rel := range list {
				if _, err := fmt.Fprintf(w, "%s --depends-on--> %s\n", rel.SrcID, rel.DstID); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return errors.New("usage: lit dep <add|rm|ls> ...")
	}
}

func runLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit label <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("label add")
		by := fs.String("by", os.Getenv("USER"), "Label author")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by})
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("label rm")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1])
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	default:
		return errors.New("usage: lit label <add|rm> ...")
	}
}

func runParent(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit parent <set|clear> ...")
	}
	switch args[0] {
	case "set":
		positional, flagArgs := splitArgs(args[1:], 2)
		fs := newCobraFlagSet("parent set")
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 2 {
			return errors.New("usage: lit parent set <child-id> <parent-id> [--by <user>] [--json]")
		}
		rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
			ChildID:   positional[0],
			ParentID:  positional[1],
			CreatedBy: *by,
		})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			relation := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --parent-child--> %s\n", relation.SrcID, relation.DstID)
			return err
		})
	case "clear":
		positional, flagArgs := splitArgs(args[1:], 1)
		fs := newCobraFlagSet("parent clear")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("usage: lit parent clear <child-id> [--json]")
		}
		if err := ap.Store.ClearParent(ctx, positional[0]); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	default:
		return errors.New("usage: lit parent <set|clear> ...")
	}
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("children")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: lit children <parent-id> [--json]")
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	return printValue(stdout, children, *jsonOut, func(w io.Writer, v any) error {
		return printIssueLines(w, v.([]model.Issue), []string{"id", "state", "title"})
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

func runSync(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
	syncStore, err := store.OpenSync(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}
	defer syncStore.Close()

	switch args[0] {
	case "remote":
		if len(args) < 2 {
			return errors.New("usage: lit sync remote ls [--json]")
		}
		switch args[1] {
		case "ls":
			fs := newCobraFlagSet("sync remote ls")
			jsonOut := fs.Bool("json", false, "Output JSON")
			if err := parseFlagSet(fs, args[2:], stdout); err != nil {
				return err
			}
			syncState, err := readSyncRemoteState(ctx, syncStore, ws)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"git_remotes":  syncState.gitRemotes,
				"dolt_remotes": syncState.doltRemotes,
				"changes":      syncState.changes,
			}
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				p := v.(map[string]any)
				_, err := fmt.Fprintf(
					w,
					"git=%d dolt=%d added=%d updated=%d removed=%d\n",
					len(p["git_remotes"].([]workspace.GitRemote)),
					len(p["dolt_remotes"].([]store.SyncRemote)),
					len(syncState.changes.Added),
					len(syncState.changes.Updated),
					len(syncState.changes.Removed),
				)
				return err
			})
		default:
			return errors.New("usage: lit sync remote ls [--json]")
		}
	case "fetch":
		fs := newCobraFlagSet("sync fetch")
		remote := fs.String("remote", "origin", "Remote name")
		prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if _, err := syncDoltRemotesFromGit(ctx, syncStore, ws); err != nil {
			return err
		}
		remoteName := strings.TrimSpace(*remote)
		if err := syncStore.SyncFetch(ctx, remoteName, *prune); err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": remoteName,
			"prune":  *prune,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if !*verbose {
				_, err := fmt.Fprintln(w, "fetched")
				return err
			}
			_, err := fmt.Fprintf(w, "fetched %s\n", p["remote"])
			return err
		})
	case "pull":
		fs := newCobraFlagSet("sync pull")
		remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
		if err != nil {
			return err
		}
		remoteName, remoteErr := resolveSyncRemote(
			strings.TrimSpace(*remote),
			workspace.UpstreamRemote(ws.RootDir),
			syncState.gitRemotes,
		)
		if remoteErr != nil {
			return remoteErr
		}
		if remoteName == "" {
			payload := map[string]any{
				"status": "skipped",
				"reason": "no_sync_remote",
				"raw":    "no upstream remote and no single configured remote; skipping sync pull",
			}
			// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPullPayload(w, v, *verbose)
			})
		}
		resolvedBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
		if err != nil {
			return err
		}
		result, err := syncStore.SyncPull(ctx, remoteName, resolvedBranch)
		payload, handledErr := buildSyncPullPayload(remoteName, resolvedBranch, result.Message, err)
		if handledErr != nil {
			return handledErr
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			return printSyncPullPayload(w, v, *verbose)
		})
	case "push":
		fs := newCobraFlagSet("sync push")
		remote := fs.String("remote", "", "Remote name (defaults to upstream remote, then single configured remote)")
		setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
		force := fs.Bool("force", false, "Pass --force to dolt push")
		verbose := fs.Bool("verbose", false, "Include detailed remote output")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		syncState, err := syncDoltRemotesFromGit(ctx, syncStore, ws)
		if err != nil {
			return err
		}
		remoteName, remoteErr := resolveSyncRemote(
			strings.TrimSpace(*remote),
			workspace.UpstreamRemote(ws.RootDir),
			syncState.gitRemotes,
		)
		if remoteErr != nil {
			return remoteErr
		}
		if remoteName == "" {
			payload := map[string]any{
				"status": "skipped",
				"reason": "no_sync_remote",
				"raw":    "no upstream remote and no single configured remote; skipping sync push",
			}
			// [LAW:dataflow-not-control-flow] exception: explicit no-remote policy requires suppressing sync side effects when remote resolution yields empty input.
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				return printSyncPushPayload(w, v, *verbose)
			})
		}
		syncBranch, err := resolveSyncBranch(ws.RootDir, remoteName)
		if err != nil {
			return err
		}
		// [LAW:dataflow-not-control-flow] Sync push runs one deterministic embedded mutation path from resolved remote+branch state.
		result, err := syncStore.SyncPush(ctx, remoteName, syncBranch, *setUpstream, *force)
		traceMetadata := map[string]string{
			"remote":      remoteName,
			"sync_branch": syncBranch,
		}
		if strings.TrimSpace(result.Message) != "" {
			traceMetadata["message"] = strings.TrimSpace(result.Message)
		}
		traceStatus := "ok"
		traceReason := "managed automation requested sync push"
		if err != nil {
			traceStatus = "error"
			traceReason = err.Error()
			traceMetadata["error"] = err.Error()
		}
		syncCommandArgs := []string{"sync", "push", "--remote", remoteName}
		if *setUpstream {
			syncCommandArgs = append(syncCommandArgs, "--set-upstream")
		}
		if *force {
			syncCommandArgs = append(syncCommandArgs, "--force")
		}
		// [LAW:one-source-of-truth] Hook-triggered sync traces reuse the shared automation trace writer instead of shell-local trace formats.
		traceRef, traceRecordErr := maybeRecordAutomatedCommandTrace(
			ws,
			formatCommand(syncCommandArgs),
			"mirror Dolt data to the configured git remote",
			traceStatus,
			traceReason,
			traceMetadata,
		)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status":      "ok",
			"remote":      remoteName,
			"branch":      syncBranch,
			"raw":         result.Message,
			"push_status": result.Status,
		}
		if traceRef != nil {
			payload["trace_ref"] = traceRef.Path
		}
		if traceRecordErr != nil {
			payload["trace_error"] = traceRecordErr.Error()
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			return printSyncPushPayload(w, v, *verbose)
		})
	case "status":
		fs := newCobraFlagSet("sync status")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		syncState, err := readSyncRemoteState(ctx, syncStore, ws)
		if err != nil {
			return err
		}
		report, err := syncStore.SyncStatus(ctx)
		if err != nil {
			return err
		}
		head := strings.TrimSpace(report.HeadCommit)
		if strings.TrimSpace(report.HeadMessage) != "" {
			head = strings.TrimSpace(report.HeadCommit + " " + report.HeadMessage)
		}
		payload := map[string]any{
			"dolt_version": report.DoltVersion,
			"branch":       report.Branch,
			"head":         head,
			"head_commit":  report.HeadCommit,
			"head_message": report.HeadMessage,
			"status":       report.Status,
			"git_remotes":  syncState.gitRemotes,
			"dolt_remotes": syncState.doltRemotes,
			"changes":      syncState.changes,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			_, err := fmt.Fprintf(
				w,
				"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
				p["dolt_version"],
				p["branch"],
				p["head"],
				len(p["git_remotes"].([]workspace.GitRemote)),
				len(p["dolt_remotes"].([]store.SyncRemote)),
				len(syncState.changes.Added),
				len(syncState.changes.Updated),
				len(syncState.changes.Removed),
			)
			return err
		})
	default:
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
}

func firstNonEmptySyncBranch(candidates ...string) string {
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func resolveSyncRemote(requestedRemote string, upstreamRemote string, gitRemotes []workspace.GitRemote) (string, error) {
	validatedRequestedRemote := strings.TrimSpace(requestedRemote)
	if validatedRequestedRemote != "" {
		// [LAW:no-silent-fallbacks] Explicit remote that doesn't exist is a configuration error, not a skip condition.
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
	return firstNonEmptySyncRemote(validatedUpstreamRemote, singleRemote), nil
}

func firstNonEmptySyncRemote(candidates ...string) string {
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
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

func resolveSyncBranch(rootDir string, remote string) (string, error) {
	debugOverride := strings.TrimSpace(os.Getenv(debugSyncBranchEnvVar))
	defaultBranch := strings.TrimSpace(workspace.DefaultRemoteBranch(rootDir, remote))
	// [LAW:single-enforcer] Sync branch selection is centralized so pull/push/hooks consume one canonical branch decision.
	resolvedBranch := firstNonEmptySyncBranch(debugOverride, defaultBranch)
	if resolvedBranch == "" {
		return "", fmt.Errorf(
			"resolve sync branch for remote %q: default branch unavailable; configure %s to override",
			strings.TrimSpace(remote),
			debugSyncBranchEnvVar,
		)
	}
	return resolvedBranch, nil
}

func buildSyncPullPayload(remote string, requestedBranch string, output string, runErr error) (map[string]any, error) {
	if runErr == nil {
		return map[string]any{
			"status": "ok",
			"remote": remote,
			"branch": requestedBranch,
			"raw":    output,
		}, nil
	}
	message := strings.TrimSpace(runErr.Error())
	missingBranch, matchesMissingBranch := detectMissingRemoteBranch(message, requestedBranch)
	if !matchesMissingBranch {
		return nil, runErr
	}
	nextCommand := fmt.Sprintf("lit sync push --remote %s --set-upstream", remote)
	retryCommand := fmt.Sprintf("lit sync pull --remote %s", remote)
	// [LAW:dataflow-not-control-flow] Sync pull always returns structured payload; outcome variance lives in status/reason fields.
	return map[string]any{
		"status":        "skipped",
		"reason":        "remote_branch_missing",
		"remote":        remote,
		"branch":        missingBranch,
		"next_command":  nextCommand,
		"retry_command": retryCommand,
		"raw":           message,
	}, nil
}

func detectMissingRemoteBranch(message string, requestedBranch string) (string, bool) {
	// [LAW:single-enforcer] Remote-branch-missing classification is centralized here to avoid drift across callsites.
	normalized := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(normalized, "not found on remote") {
		return "", false
	}
	matches := missingRemoteBranchPattern.FindStringSubmatch(message)
	branch := strings.TrimSpace(requestedBranch)
	if len(matches) == 2 && strings.TrimSpace(matches[1]) != "" {
		branch = strings.TrimSpace(matches[1])
	}
	if branch == "" {
		return "", false
	}
	return branch, true
}

func printSyncPullPayload(w io.Writer, v any, verbose bool) error {
	payload := v.(map[string]any)
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	switch status {
	case "skipped":
		reason := strings.TrimSpace(fmt.Sprintf("%v", payload["reason"]))
		if reason == "no_sync_remote" {
			if !verbose {
				return nil
			}
			_, err := fmt.Fprintln(w, "skipped sync pull: no eligible git remote")
			return err
		}
		nextCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["next_command"]))
		retryCommand := strings.TrimSpace(fmt.Sprintf("%v", payload["retry_command"]))
		if !verbose {
			_, err := fmt.Fprintf(
				w,
				"sync pull skipped; run `%s`, then retry `%s`\n",
				nextCommand,
				retryCommand,
			)
			return err
		}
		_, err := fmt.Fprintf(
			w,
			"skipped pull %s/%s: remote branch missing; run `%s`, then retry `%s`\n",
			remote,
			branch,
			nextCommand,
			retryCommand,
		)
		return err
	default:
		raw, hasRaw := payload["raw"].(string)
		if !verbose {
			_, err := fmt.Fprintln(w, "pulled")
			return err
		}
		if hasRaw && strings.TrimSpace(raw) != "" {
			_, err := fmt.Fprintln(w, raw)
			return err
		}
		if branch != "" {
			_, err := fmt.Fprintf(w, "pulled %s/%s\n", remote, branch)
			return err
		}
		_, err := fmt.Fprintf(w, "pulled %s\n", remote)
		return err
	}
}

func printSyncPushPayload(w io.Writer, v any, verbose bool) error {
	payload := v.(map[string]any)
	status := strings.TrimSpace(fmt.Sprintf("%v", payload["status"]))
	raw, hasRaw := payload["raw"].(string)
	if !verbose && status == "skipped" {
		return nil
	}
	if !verbose {
		_, err := fmt.Fprintln(w, "pushed")
		return err
	}
	if hasRaw && strings.TrimSpace(raw) != "" {
		_, err := fmt.Fprintln(w, strings.TrimSpace(raw))
		return err
	}
	if status == "skipped" {
		_, err := fmt.Fprintln(w, "skipped sync push: no eligible git remote")
		return err
	}
	remote := strings.TrimSpace(fmt.Sprintf("%v", payload["remote"]))
	branch := strings.TrimSpace(fmt.Sprintf("%v", payload["branch"]))
	if branch != "" {
		_, err := fmt.Fprintf(w, "pushed %s/%s\n", remote, branch)
		return err
	}
	_, err := fmt.Fprintf(w, "pushed %s\n", remote)
	return err
}

type errorDetailer interface {
	ErrorDetails() map[string]any
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []store.SyncRemote
	changes     remoteSyncChanges
}

func readSyncRemoteState(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: doltRemotes,
		changes:     buildRemoteSyncChanges(gitRemotes, doltRemotes),
	}, nil
}

func syncDoltRemotesFromGit(ctx context.Context, syncStore *store.Store, ws workspace.Info) (remoteSyncState, error) {
	state, err := readSyncRemoteState(ctx, syncStore, ws)
	if err != nil {
		return remoteSyncState{}, err
	}
	gitRemotes := state.gitRemotes
	doltRemotes := state.doltRemotes
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := buildRemoteSyncChanges(gitRemotes, doltRemotes)

	for _, remote := range gitRemotes {
		desiredURL := syncRemoteURL(remote.URL)
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
			continue
		}
		if strings.TrimSpace(currentURL) != desiredURL {
			if err := syncStore.SyncRemoveRemote(ctx, remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if err := syncStore.SyncAddRemote(ctx, remote.Name, desiredURL); err != nil {
				return remoteSyncState{}, err
			}
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if err := syncStore.SyncRemoveRemote(ctx, name); err != nil {
			return remoteSyncState{}, err
		}
	}
	finalRemotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: finalRemotes,
		changes:     changes,
	}, nil
}

func buildRemoteSyncChanges(gitRemotes []workspace.GitRemote, doltRemotes []store.SyncRemote) remoteSyncChanges {
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)
	changes := remoteSyncChanges{
		Added:   []string{},
		Updated: []string{},
		Removed: []string{},
	}
	for _, remote := range gitRemotes {
		desiredURL := syncRemoteURL(remote.URL)
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

func mapRemotesByName(remotes []store.SyncRemote) map[string]string {
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

func sameRemoteURL(left, right string) bool {
	return normalizeRemoteURL(left) == normalizeRemoteURL(right)
}

func syncRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "git+") {
		return trimmed
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), ".git") {
		return trimmed
	}
	if strings.Contains(trimmed, "://") {
		// [LAW:one-source-of-truth] Git-backed Dolt remotes use one explicit `git+...` transport form instead of relying on procedure-side URL inference.
		return "git+" + trimmed
	}
	normalized := normalizeSCPLikeRemoteURL(trimmed)
	if normalized != trimmed {
		return "git+" + normalized
	}
	return trimmed
}

func normalizeRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "git+")
	if trimmed == "" {
		return ""
	}
	// [LAW:one-source-of-truth] Remote URL comparison uses one canonical normalizer so sync reconciliation decisions do not drift across URL spellings.
	trimmed = normalizeSCPLikeRemoteURL(trimmed)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	parsed.Host = strings.ToLower(strings.TrimSpace(parsed.Host))
	parsed.Path = normalizeRemotePath(parsed.Path)
	return parsed.String()
}

func normalizeSCPLikeRemoteURL(input string) string {
	if strings.Contains(input, "://") {
		return input
	}
	separator := scpHostPathSeparator(input)
	if separator <= 0 {
		return input
	}
	hostPart := strings.TrimSpace(input[:separator])
	pathPart := strings.TrimSpace(input[separator+1:])
	if hostPart == "" || pathPart == "" || strings.Contains(hostPart, "/") {
		return input
	}
	if strings.HasPrefix(pathPart, "/") {
		return "ssh://" + hostPart + pathPart
	}
	return "ssh://" + hostPart + "/" + pathPart
}

func scpHostPathSeparator(input string) int {
	separator := -1
	inBrackets := false
	for index, character := range input {
		switch character {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				separator = index
				return separator
			}
		}
	}
	return separator
}

func normalizeRemotePath(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	cleaned := path.Clean(strings.TrimSpace(input))
	if strings.HasPrefix(input, "/") && !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func allDoctorFixNames() []string {
	names := make([]string, 0, len(doctorFixes))
	for k := range doctorFixes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// doctorFixes is the registry of available doctor fixes.
// [LAW:one-source-of-truth] This map is the single authority for valid fix names.
var doctorFixes = map[string]func(context.Context, io.Writer, *app.App) error{
	"integrity": func(ctx context.Context, w io.Writer, ap *app.App) error {
		report, err := ap.Store.Fsck(ctx, true)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "Integrity repair: foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d\n",
			report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows)
		return err
	},
	"rank": func(ctx context.Context, w io.Writer, ap *app.App) error {
		fixed, err := ap.Store.FixRankInversions(ctx)
		if err != nil {
			return err
		}
		if fixed > 0 {
			_, err = fmt.Fprintf(w, "Fixed %d rank inversion(s).\n", fixed)
		}
		return err
	},
}

func runDoctor(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("doctor")
	fix := fs.String("fix", "", "Apply fixes: --fix (all) or --fix rank,thingA")
	fs.cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if *fix != "" {
		fixNames := allDoctorFixNames()
		if *fix != "all" {
			fixNames = splitCSV(*fix)
		}
		for _, name := range fixNames {
			fn, ok := doctorFixes[name]
			if !ok {
				return fmt.Errorf("unknown fix %q; available: %s", name, strings.Join(allDoctorFixNames(), ", "))
			}
			if err := fn(ctx, stdout, ap); err != nil {
				return err
			}
		}
	}
	report, err := ap.Store.Doctor(ctx)
	if err != nil {
		return err
	}
	if err := printValue(stdout, report, *jsonOut, func(w io.Writer, v any) error {
		r := v.(store.HealthReport)
		_, err := fmt.Fprintf(w, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d rank_inversions=%d\n", r.IntegrityCheck, r.ForeignKeyIssues, r.InvalidRelatedRows, r.OrphanHistoryRows, r.RankInversions)
		return err
	}); err != nil {
		return err
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}

func runBackup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
	switch args[0] {
	case "create":
		fs := newCobraFlagSet("backup create")
		keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		export, err := ap.Store.Export(ctx)
		if err != nil {
			return err
		}
		snapshot, err := backup.Create(ap.Workspace.StorageDir, export)
		if err != nil {
			return err
		}
		if err := backup.Prune(ap.Workspace.StorageDir, *keep); err != nil {
			return err
		}
		return printValue(stdout, snapshot, *jsonOut, func(w io.Writer, v any) error {
			s := v.(backup.Snapshot)
			_, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Path)
			return err
		})
	case "list":
		fs := newCobraFlagSet("backup list")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		return printValue(stdout, snapshots, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]backup.Snapshot)
			for _, snapshot := range list {
				if _, err := fmt.Fprintf(w, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
					return err
				}
			}
			return nil
		})
	case "restore":
		fs := newCobraFlagSet("backup restore")
		path := fs.String("path", "", "Backup snapshot path")
		latest := fs.Bool("latest", false, "Restore latest backup snapshot")
		force := fs.Bool("force", false, "Force restore over unsynced state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		restorePath := strings.TrimSpace(*path)
		if *latest {
			latestSnapshot, err := backup.Latest(ap.Workspace.StorageDir)
			if err != nil {
				return err
			}
			if latestSnapshot == nil {
				return errors.New("no backups available")
			}
			restorePath = latestSnapshot.Path
		}
		if restorePath == "" {
			return errors.New("usage: lit backup restore --path <snapshot.json> [--force] [--json] or --latest")
		}
		if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "restored", "path": restorePath}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
}

func runRecover(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("recover")
	fromSync := fs.String("from-sync", "", "Restore from sync file")
	fromBackup := fs.String("from-backup", "", "Restore from backup snapshot")
	latestBackup := fs.Bool("latest-backup", false, "Restore from latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	var restorePath string
	switch {
	case strings.TrimSpace(*fromSync) != "":
		restorePath = strings.TrimSpace(*fromSync)
	case strings.TrimSpace(*fromBackup) != "":
		restorePath = strings.TrimSpace(*fromBackup)
	case *latestBackup:
		latest, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		if latest == nil {
			return errors.New("no backups available")
		}
		restorePath = latest.Path
	default:
		return errors.New("usage: lit recover --from-sync <path> | --from-backup <path> | --latest-backup [--force] [--json]")
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	payload := map[string]string{"status": "recovered", "path": restorePath}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
		return err
	})
}

func runBulk(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
	switch args[0] {
	case "label":
		if len(args) < 2 {
			return errors.New("usage: lit bulk label <add|rm> ...")
		}
		action := args[1]
		fs := newCobraFlagSet("bulk label")
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		label := fs.String("label", "", "Label name")
		by := fs.String("by", os.Getenv("USER"), "Label actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[2:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*label) == "" {
			return errors.New("--label is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			switch action {
			case "add":
				_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
					IssueID:   issueID,
					Name:      *label,
					CreatedBy: *by,
				})
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			case "rm":
				_, err := ap.Store.RemoveLabel(ctx, issueID, *label)
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			default:
				return errors.New("usage: lit bulk label <add|rm> ...")
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "close", "archive":
		fs := newCobraFlagSet("bulk transition")
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("--reason is required")
		}
		results := map[string]string{}
		for _, issueID := range issueIDs {
			_, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:   issueID,
				Action:    args[0],
				Reason:    *reason,
				CreatedBy: *by,
			})
			if err != nil {
				results[issueID] = err.Error()
				continue
			}
			results[issueID] = "ok"
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "import":
		fs := newCobraFlagSet("bulk import")
		path := fs.String("path", "", "Path to JSON export")
		force := fs.Bool("force", false, "Force import over unsynced local state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := parseFlagSet(fs, args[1:], stdout); err != nil {
			return err
		}
		if strings.TrimSpace(*path) == "" {
			return errors.New("--path is required")
		}
		if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "imported", "path": filepath.Clean(*path)}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
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
	fs := newCobraFlagSet("quickstart")
	jsonOut := fs.Bool("json", false, "Output JSON")
	refresh := fs.Bool("refresh", false, "Refresh managed repo assets")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit quickstart [--json] [--refresh]")
	}

	topics := []string{}
	readStore, err := store.OpenForRead(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err == nil {
		topics, err = readStore.ListTopics(ctx)
		_ = readStore.Close()
		if err != nil {
			topics = []string{}
		}
	}

	payload := map[string]any{
		"summary":      "Agent quickstart for links issue tracking",
		"issue_prefix": ws.IssuePrefix,
		"topics":       topics,
		"workflow": []string{
			"Initialize and auto-migrate with `lit init`.",
			"Refresh managed repo assets with `lit quickstart --refresh`.",
			"Discover workspace identity with `lit workspace`.",
			"Migrate legacy Beads data/wiring explicitly with `lit migrate --apply` when needed.",
			"Install git hook automation once with `lit hooks install`.",
			"List ready work with `lit ready` (or `lit ls --query \"status:open\"`).",
			"Create a concise immutable one-word topic, reuse an existing topic when possible, and pass it with `lit new --topic <topic> ...`.",
			"Use `lit new --parent <issue-id> ...` when creating a child issue so the ID becomes `parentID.<n>`.",
			"Create issues with `lit new ...`; use `--type epic` for epics.",
			"Connect issues using `lit parent set` and `lit dep add <issue> <depends-on> --type blocks|related-to`.",
			"Configure remotes with `git remote`; `lit sync` mirrors those remotes into Dolt automatically.",
			"Run health checks with `lit doctor` and repair issues with `lit doctor --fix`.",
			"Snapshot and rollback using `lit backup create`, `lit backup restore`, or `lit recover`.",
		},
		"examples": []string{
			"lit init",
			"lit quickstart --refresh",
			"lit migrate --apply",
			"lit hooks install",
			"lit workspace",
			"lit ready",
			"lit update <issue-id> --status in_progress",
			"lit start <issue-id> --reason \"claim\"",
			"lit done <issue-id> --reason \"completed\"",
			"lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc",
			"lit new --title \"Fix renderer race\" --topic renderer --type bug --priority 1 --labels renderer,urgent",
			"lit new --title \"Tighten race reproducer\" --topic renderer --type task --parent <issue-id>",
			"lit parent set <issue-id> <parent-issue-id>",
			"lit dep add <issue-id> <depends-on-id> --type related-to",
			"git remote add origin https://github.com/org/repo.git",
			"lit sync remote ls",
			"lit sync pull",
			"lit sync push",
		},
		"exit_codes": map[string]int{
			"ok":         ExitOK,
			"usage":      ExitUsage,
			"validation": ExitValidation,
			"not_found":  ExitNotFound,
			"conflict":   ExitConflict,
			"corruption": ExitCorruption,
		},
	}
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
		payload["refresh"] = refreshReport
	}

	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		instructions := v.(map[string]any)
		lines := []string{
			"links agent quickstart",
			"",
			"1) Discover context",
			"   `lit init`",
			"   `lit quickstart --refresh`",
			"   `lit migrate --apply`  # for legacy Beads repos",
			"   `lit workspace`",
			"",
			"2) Find work",
			"   `lit ready`",
			"   `lit update <issue-id> --status in_progress`",
			"   `lit start <issue-id> --reason \"claim\"`",
			"   `lit ls --format lines`",
			"   `lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc`",
			"",
			"3) Create and relate issues/epics",
			fmt.Sprintf("   project prefix: `%s`", ws.IssuePrefix),
			"   choose or reuse a concise one-word topic and pass `--topic <topic>`",
			"   `lit new --title \"...\" --topic <topic> --type task|bug|feature|chore|epic`",
			"   `lit new --title \"...\" --topic <topic> --parent <parent-id> --type task`",
			"   `lit parent set <child-id> <parent-id>`",
			"   `lit dep add <issue-id> <depends-on-id> --type blocks|related-to|parent-child`",
			"",
			"4) Mutations",
			"   Use command outputs directly for follow-up writes.",
			"",
			"5) Dolt remote sync",
			"   Configure remotes with git, then run sync commands.",
			"   `git remote -v`",
			"   `lit sync remote ls`",
			"   `lit sync fetch`",
			"   `lit sync pull`",
			"   `lit sync push`",
			"   If sync warning appears, agent should auto-retry silently before escalating.",
			"",
			"6) Integrity and recovery",
			"   `lit doctor`",
			"   `lit doctor --fix`",
			"   `lit backup create`",
			"   `lit backup restore --latest`",
			"   `lit recover --latest-backup`",
			"",
			fmt.Sprintf("Exit codes: ok=%d usage=%d validation=%d not_found=%d conflict=%d corruption=%d", ExitOK, ExitUsage, ExitValidation, ExitNotFound, ExitConflict, ExitCorruption),
		}
		if summary, ok := instructions["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			lines[0] = summary
		}
		if quickstartTopics, ok := instructions["topics"].([]string); ok && len(quickstartTopics) > 0 {
			for index, line := range lines {
				if strings.HasPrefix(line, "   project prefix: ") {
					lines = append(lines[:index+1], append([]string{
						fmt.Sprintf("   existing topics: %s", strings.Join(quickstartTopics, ", ")),
					}, lines[index+1:]...)...)
					break
				}
			}
		}
		if refreshReport, ok := instructions["refresh"].(quickstartRefreshReport); ok {
			lines = append(lines[:1], append([]string{
				fmt.Sprintf("refresh %s", formatQuickstartRefreshSummary(refreshReport)),
				"",
			}, lines[1:]...)...)
		}
		_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
		return err
	})
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

func printIssueSummary(w io.Writer, v any) error {
	issue := v.(model.Issue)
	_, err := fmt.Fprintf(w, "%s [%s/%s/%s/P%d] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Topic, issue.Priority, issue.Title, formatLabels(issue.Labels))
	return err
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(resolved, "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, resolved, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, resolved, " | ")); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nstatus: %s\ntype: %s\ntopic: %s\npriority: %d\nassignee: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.Status, issue.IssueType, issue.Topic, issue.Priority, emptyDash(issue.Assignee), emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(issue.ArchivedAt), formatOptionalTime(issue.DeletedAt)); err != nil {
		return err
	}
	if issue.Description != "" {
		if _, err := fmt.Fprintf(w, "\ndescription:\n%s\n", issue.Description); err != nil {
			return err
		}
	}
	if detail.Parent != nil {
		if _, err := fmt.Fprintf(w, "\nparent:\n- %s %s\n", detail.Parent.ID, detail.Parent.Title); err != nil {
			return err
		}
	}
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
		return err
	}
	if err := printIssueGroup(w, "depends_on", detail.DependsOn); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocks", detail.Blocks); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if len(detail.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "\ncomments:"); err != nil {
			return err
		}
		for _, c := range detail.Comments {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", c.CreatedBy, strings.ReplaceAll(c.Body, "\n", "\\n")); err != nil {
				return err
			}
		}
	}
	if len(detail.History) > 0 {
		if _, err := fmt.Fprintln(w, "\nhistory:"); err != nil {
			return err
		}
		for _, event := range detail.History {
			if _, err := fmt.Fprintf(w, "- [%s] %s %s (%s -> %s)\n", event.CreatedBy, event.Action, strings.ReplaceAll(event.Reason, "\n", "\\n"), emptyDash(event.FromStatus), emptyDash(event.ToStatus)); err != nil {
				return err
			}
		}
	}
	return nil
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintf(w, "- %s %s\n", issue.ID, issue.Title); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueColumns(issue model.Issue, columns []string, delimiter string) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		switch column {
		case "id":
			values = append(values, issue.ID)
		case "state":
			values = append(values, formatIssueState(issue))
		case "type":
			values = append(values, issue.IssueType)
		case "topic":
			values = append(values, issue.Topic)
		case "priority":
			values = append(values, strconv.Itoa(issue.Priority))
		case "title":
			values = append(values, issue.Title)
		case "assignee":
			values = append(values, emptyDash(issue.Assignee))
		case "labels":
			values = append(values, emptyDash(strings.Join(issue.Labels, ",")))
		case "updated_at":
			values = append(values, issue.UpdatedAt.Format(time.RFC3339))
		case "created_at":
			values = append(values, issue.CreatedAt.Format(time.RFC3339))
		}
	}
	return strings.Join(values, delimiter)
}

func resolveColumns(columns []string) []string {
	if len(columns) == 0 {
		// [LAW:dataflow-not-control-flow] Default listing still flows through the same projection path.
		return []string{"id", "state", "topic", "title"}
	}
	valid := map[string]struct{}{
		"id": {}, "state": {}, "type": {}, "topic": {}, "priority": {}, "title": {}, "assignee": {}, "labels": {}, "updated_at": {}, "created_at": {},
	}
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		normalized := strings.ToLower(strings.TrimSpace(column))
		if normalized == "" {
			continue
		}
		if _, ok := valid[normalized]; ok {
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{"id", "state", "topic", "title"}
	}
	return out
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func printLabels(w io.Writer, v any) error {
	labels := v.([]string)
	_, err := fmt.Fprintln(w, strings.Join(labels, ","))
	return err
}

func formatLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return " [" + strings.Join(labels, ",") + "]"
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func transitionCommandName(action string) string {
	switch action {
	case "reopen":
		return "open"
	default:
		return action
	}
}

func formatIssueState(issue model.Issue) string {
	parts := []string{issue.Status}
	if issue.ArchivedAt != nil {
		parts = append(parts, "archived")
	}
	if issue.DeletedAt != nil {
		parts = append(parts, "deleted")
	}
	return strings.Join(parts, "+")
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

func parseColumns(input string) []string {
	return splitCSV(strings.ToLower(input))
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

func syncBasePath(ap *app.App) string {
	return filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")
}

func restoreFromExportPath(ctx context.Context, ap *app.App, path string, force bool) error {
	restorePath := filepath.Clean(path)
	targetExport, _, err := syncfile.Read(restorePath)
	if err != nil {
		return err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	state, err := ap.Store.GetSyncState(ctx)
	if err != nil {
		return err
	}
	if state.ContentHash != "" && !force {
		baseHash, hashErr := syncfile.HashFile(syncBasePath(ap))
		if hashErr != nil {
			return hashErr
		}
		if baseHash != "" {
			localHash, localHashErr := hashExport(localExport)
			if localHashErr != nil {
				return localHashErr
			}
			if localHash != baseHash {
				return MergeConflictError{Message: "restore conflict: local workspace has unsynced changes since last sync base"}
			}
		}
	}
	if _, err := backup.Create(ap.Workspace.StorageDir, localExport); err != nil {
		return err
	}
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
		return err
	}
	if err := ap.Store.ReplaceFromExport(ctx, targetExport); err != nil {
		return err
	}
	if _, err := syncfile.WriteAtomic(syncBasePath(ap), targetExport); err != nil {
		return err
	}
	hash, err := syncfile.HashFile(restorePath)
	if err != nil {
		return err
	}
	return ap.Store.RecordSyncState(ctx, store.SyncState{
		Path:        restorePath,
		ContentHash: hash,
	})
}

func hashExport(export model.Export) (string, error) {
	payload, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal export: %w", err)
	}
	payload = append(payload, '\n')
	sum := sha256.Sum256(payload)
	return strings.ToLower(hex.EncodeToString(sum[:])), nil
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

Worktree-native issue tracker with Dolt-backed sync.

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
  ready          List open work ordered by priority and recency
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
  lit start <id> --reason <text> [--by <user>] [--json]
  lit done <id> --reason <text> [--by <user>] [--json]
  lit hooks install [--json]
  lit migrate [--apply] [--json] [--skip-hooks] [--skip-agents]
  lit quickstart [--json] [--refresh]
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
  lit start <issue-id> --reason "claim"
  lit done <issue-id> --reason "completed"
  lit new --title "Fix renderer race" --type bug --priority 1
  lit ls --query "status:open type:task" --sort priority:asc,updated_at:desc

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
