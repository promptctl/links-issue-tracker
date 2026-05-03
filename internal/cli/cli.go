package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
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
	applyRegistry(root, commandGroups, commandSpecs(ctx, stdout, stderr))
	return root
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
	prompt := fs.String("prompt", "", "Reusable agent prompt for the work this issue captures")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	topic := fs.String("topic", "", "Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')")
	parentID := fs.String("parent", "", "Optional parent issue ID; child IDs become parentID.<n>")
	priority := fs.Int("priority", 2, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title: *title, Description: *description, Prompt: *prompt, IssueType: *issueType, Topic: *topic, ParentID: *parentID, Priority: *priority, Assignee: *assignee, Labels: splitCSV(*labels),
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

// runFollowup creates a child issue parented to --on, intended for the
// capture-at-close moment when a closing agent has fresh context about
// follow-up work the close surfaced. Topic inherits from the parent when
// omitted; description defaults to a reference back to the parent.
//
// See design-docs/preparing-the-next-loop.md for the principle this
// implements (capture-at-close affordances).
func runFollowup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("followup")
	on := fs.String("on", "", "Required parent issue ID (typically the just-closed ticket)")
	title := fs.String("title", "", "Required follow-up title")
	description := fs.String("description", "", "Optional description; defaults to a reference back to --on")
	prompt := fs.String("prompt", "", "Optional reusable agent prompt for the follow-up")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	topic := fs.String("topic", "", "Topic slug; inherits from --on when omitted")
	priority := fs.Int("priority", 2, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	parentID := strings.TrimSpace(*on)
	titleValue := strings.TrimSpace(*title)
	if parentID == "" || titleValue == "" {
		return errors.New("usage: lit followup --on <id> --title <text> [--description <text>] [--topic <slug>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--json]")
	}
	parent, err := ap.Store.GetIssue(ctx, parentID)
	if err != nil {
		return err
	}
	resolvedTopic := strings.TrimSpace(*topic)
	if resolvedTopic == "" {
		resolvedTopic = parent.Topic
	}
	resolvedDescription := strings.TrimSpace(*description)
	if resolvedDescription == "" {
		resolvedDescription = fmt.Sprintf("Follow-up surfaced at the close of %s: %s", parent.ID, parent.Title)
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title:       titleValue,
		Description: resolvedDescription,
		Prompt:      strings.TrimSpace(*prompt),
		IssueType:   *issueType,
		Topic:       resolvedTopic,
		ParentID:    parent.ID,
		Priority:    *priority,
		Assignee:    *assignee,
		Labels:      splitCSV(*labels),
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

func runReady(ctx context.Context, stdout io.Writer, stderr io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("ready")
	assignee := fs.String("assignee", "", "Filter by assignee")
	issueType := fs.String("type", "", "Filter by issue type")
	status := fs.String("status", "", "Filter by status: open|in_progress (closed excludes everything)")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	priorityMin := fs.Int("priority-min", -1, "Minimum priority 0..4")
	priorityMax := fs.Int("priority-max", -1, "Maximum priority 0..4")
	limit := fs.Int("limit", 0, "Limit results")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit ready [--type ...] [--status ...] [--labels ...] [--assignee <user>] [--priority-min N] [--priority-max N] [--limit N] [--columns ...] [--json]")
	}
	visited := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) { visited[f.Name] = true })
	rf := readyFilter{
		Assignee:  strings.TrimSpace(*assignee),
		IssueType: strings.TrimSpace(*issueType),
		Status:    strings.TrimSpace(*status),
		Labels:    splitCSV(*labels),
	}
	if visited["priority-min"] {
		v := *priorityMin
		rf.PriorityMin = &v
	}
	if visited["priority-max"] {
		v := *priorityMax
		rf.PriorityMax = &v
	}
	annotated, _, err := gatherReadyAnnotated(ctx, ap, rf)
	if err != nil {
		return err
	}
	annotated = applyLimit(annotated, *limit)
	columns := parseColumns(*columnsExpr)
	// Coaching preamble goes to stderr so stdout stays parseable: a script piping
	// `lit ready | parser` reads only the data, while a TTY user sees both streams
	// merged. JSON output skips the preamble entirely — structured consumers don't
	// need prose.
	if !*jsonOut {
		if err := writeReadyPreamble(stderr); err != nil {
			return err
		}
	}
	return printValue(stdout, annotated, *jsonOut, func(w io.Writer, v any) error {
		return printReadyOutput(w, columns, v.([]annotation.AnnotatedIssue))
	})
}

// readyFilter carries the user-supplied narrowing options for `lit ready` and
// `lit next`. Empty fields mean "no narrowing"; the workable definition
// (open/in_progress, leaves only) is layered on top by gatherReadyAnnotated.
type readyFilter struct {
	Assignee    string
	IssueType   string
	Status      string
	Labels      []string
	PriorityMin *int
	PriorityMax *int
}

// gatherReadyAnnotated runs the shared ready pipeline: list workable leaves,
// fetch details, annotate, sort by composite rank then readiness, enrich with
// parent epic refs. Returns the prepared rows and the details map so callers
// that need extra row context (e.g. `lit next --continue`) avoid a second
// fetch round-trip.
// [LAW:single-enforcer] Both `lit ready` and `lit next` read from this single
// pipeline so their "what is workable, in what order" model cannot drift.
func gatherReadyAnnotated(ctx context.Context, ap *app.App, rf readyFilter) ([]annotation.AnnotatedIssue, map[string]model.IssueDetail, error) {
	cfg, err := config.Load(ap.Workspace.RootDir)
	if err != nil {
		return nil, nil, err
	}
	statuses := []string{"open", "in_progress"}
	if rf.Status != "" {
		// User-supplied status overrides the workable default. The intersection
		// with leaf-only filtering still applies via filterWorkableIssues below;
		// a user asking for closed items here gets none, which is the honest
		// answer rather than silently substituting a different status.
		statuses = []string{rf.Status}
	}
	// [LAW:one-source-of-truth] rank is the canonical ordering; no explicit SortBy
	// needed — the store default is item_rank ASC.
	listFilter := store.ListIssuesFilter{
		Statuses:        statuses,
		IssueTypes:      toSlice(rf.IssueType),
		Assignees:       toSlice(rf.Assignee),
		LabelsAll:       rf.Labels,
		PriorityMin:     rf.PriorityMin,
		PriorityMax:     rf.PriorityMax,
		IncludeArchived: false,
		IncludeDeleted:  false,
		Limit:           0,
	}
	issues, err := ap.Store.ListIssues(ctx, listFilter)
	if err != nil {
		return nil, nil, err
	}
	issues = filterWorkableIssues(issues)
	fieldAnnotator, err := newFieldAnnotator(cfg.Ready.RequiredFields)
	if err != nil {
		return nil, nil, err
	}
	details, err := fetchIssueDetails(ctx, ap.Store, issues)
	if err != nil {
		return nil, nil, err
	}
	annotated, err := annotation.Annotate(ctx, issues,
		fieldAnnotator,
		newBlockerAnnotator(details),
		newOrphanedAnnotator(orphanedThreshold),
		newNeedsDesignAnnotator(),
	)
	if err != nil {
		return nil, nil, err
	}
	sortByCompositeRank(annotated, details)
	sortByBlockingAnnotations(annotated)
	enrichWithParentEpic(annotated, details)
	return annotated, details, nil
}

// runNext returns exactly one workable leaf — the next thing the agent should
// `lit start`. Identical pipeline to `lit ready`; the only differences are the
// optional --continue bias and that the output is a single row instead of the
// sectioned backlog.
// (links-agent-epic-model-uew.6)
func runNext(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("next")
	assignee := fs.String("assignee", "", "Filter by assignee")
	continueFlag := fs.Bool("continue", false, "Bias toward leaves under in-progress epics")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit next [--continue] [--assignee <user>] [--json]")
	}
	annotated, details, err := gatherReadyAnnotated(ctx, ap, readyFilter{Assignee: strings.TrimSpace(*assignee)})
	if err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] --continue is one extra stable sort over the
	// same data; it does not change which rows are workable, only the order in
	// which we look for one to claim.
	if *continueFlag {
		sortByContinueBias(annotated, details)
	}
	next, ok := pickFirstReady(annotated)
	if !ok {
		return errors.New("no ready work")
	}
	return printValue(stdout, next, *jsonOut, printNextSummary)
}

// runOrphaned lists in_progress issues whose last update is older than
// orphanedThreshold — the same definition `lit ready` uses for the
// "(ORPHANED)" marker, surfaced as a focused command for reclamation
// workflows.
//
// [LAW:single-enforcer] Orphan classification lives in
// newOrphanedAnnotator; this command only filters and presents.
// [LAW:one-source-of-truth] Threshold comes from orphanedThreshold,
// not a re-declared local constant.
func runOrphaned(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("orphaned")
	assignee := fs.String("assignee", "", "Filter by assignee")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit orphaned [--assignee <user>] [--json]")
	}
	listFilter := store.ListIssuesFilter{
		Statuses:        []string{"in_progress"},
		Assignees:       toSlice(strings.TrimSpace(*assignee)),
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
	// a leaf-only concept here, same as in `lit ready`/`lit next`.
	issues = filterWorkableIssues(issues)
	annotated, err := annotation.Annotate(ctx, issues, newOrphanedAnnotator(orphanedThreshold))
	if err != nil {
		return err
	}
	orphaned := make([]annotation.AnnotatedIssue, 0, len(annotated))
	for _, entry := range annotated {
		if annotation.HasAny(entry.Annotations, annotation.Orphaned) {
			orphaned = append(orphaned, entry)
		}
	}
	// Sort oldest-first so the most stale work surfaces at the top —
	// the row most likely to need reclamation. Same UpdatedAt the
	// annotator keyed off, so order matches staleness directly.
	sort.SliceStable(orphaned, func(i, j int) bool {
		return orphaned[i].UpdatedAt.Before(orphaned[j].UpdatedAt)
	})
	return printValue(stdout, orphaned, *jsonOut, printOrphanedText)
}

func printOrphanedText(w io.Writer, v any) error {
	rows := v.([]annotation.AnnotatedIssue)
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No orphaned issues.")
		return err
	}
	columns := []string{"id", "state", "topic", "assignee", "title"}
	for _, entry := range rows {
		line := formatIssueColumns(entry.Issue, columns, " | ")
		age := time.Since(entry.UpdatedAt).Truncate(time.Minute)
		line += " | Last Update: " + age.String()
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
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

func runUpdate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("update")
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	prompt := fs.String("prompt", "", "Reusable agent prompt for the work this issue captures")
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
		return errors.New("usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--status <open|in_progress|closed>] [--reason <text>] [--by <user>] [--json]")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0..4>] [--assignee <user>] [--labels <csv>] [--status <open|in_progress|closed>] [--reason <text>] [--by <user>] [--json]")
	}
	visited := map[string]bool{}
	fs.Visit(func(flag *pflag.Flag) { visited[flag.Name] = true })
	if visited["reason"] && !visited["status"] {
		return errors.New("--reason requires --status")
	}
	if visited["by"] && !visited["status"] {
		return errors.New("--by requires --status")
	}
	if visited["status"] && strings.TrimSpace(*status) == "" {
		return errors.New("--status requires a non-empty value")
	}

	// [LAW:dataflow-not-control-flow] Always build one UpdateInput; variability lives in empty fields/status, not in which branch runs.
	in := store.ApplyUpdateInput{
		TransitionReason: strings.TrimSpace(*reason),
		TransitionBy:     *by,
	}
	if visited["status"] {
		in.TargetStatus = *status
	}
	if visited["title"] {
		value := *title
		in.Fields.Title = &value
	}
	if visited["description"] {
		value := *description
		in.Fields.Description = &value
	}
	if visited["prompt"] {
		value := *prompt
		in.Fields.Prompt = &value
	}
	if visited["type"] {
		value := *issueType
		in.Fields.IssueType = &value
	}
	if visited["priority"] {
		value := *priority
		in.Fields.Priority = &value
	}
	if visited["assignee"] {
		value := *assignee
		in.Fields.Assignee = &value
	}
	if visited["labels"] {
		value := splitCSV(*labels)
		in.Fields.Labels = &value
	}
	if in.IsEmpty() {
		return errors.New("lit update requires at least one field flag")
	}
	issue, err := ap.Store.ApplyUpdate(ctx, positional[0], in)
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runRank(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	// Subcommand dispatch: 'lit rank set <id1> <id2> ...' is a separate verb
	// that establishes absolute order across N issues atomically. Issue IDs
	// always carry a workspace-configured prefix (e.g. <prefix>-<n>), so the
	// literal 'set' is unambiguous.
	if len(args) > 0 && args[0] == "set" {
		return runRankSet(ctx, stdout, ap, args[1:])
	}
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

// runRankSet establishes absolute order across N issues by stacking them at
// the top in the given sequence: id1 becomes the topmost, id2 ranks just
// below, etc. Atomic: the store either applies all or none.
func runRankSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, len(args))
	fs := newCobraFlagSet("rank set")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) < 2 {
		return errors.New("usage: lit rank set <id1> <id2> [<id3> ...]")
	}
	if err := ap.Store.RankSet(ctx, positional); err != nil {
		return err
	}
	return printValue(stdout, map[string]any{"status": "ok", "ranked": positional}, *jsonOut, func(w io.Writer, _ any) error {
		_, err := fmt.Fprintf(w, "ranked %d issues at top in order: %s\n", len(positional), strings.Join(positional, ", "))
		return err
	})
}


func filterWorkableIssues(issues []model.Issue) []model.Issue {
	filtered := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		status := issue.Capabilities().Status
		if status != nil && status.Value != model.StateClosed {
			filtered = append(filtered, issue)
		}
	}
	return filtered
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

// runImportTree consumes a JSON tree spec and creates issues in dependency
// order with best-effort rollback on failure (see Store.ImportTree). The spec
// is an array of records; each carries a local_id used inside the spec to
// wire parent/depends_on refs. Real issue IDs are generated at create time
// and returned in the id_map result. Run `lit doctor` after a failed import
// to detect any orphans left if rollback itself failed.
//
// JSON shape (see store.ImportTreeSpec):
//
//	[
//	  {"local_id": "epic-x", "title": "Build X", "type": "epic", "topic": "x", "priority": 2},
//	  {"local_id": "task-1", "parent": "epic-x", "title": "Design", "type": "task", "topic": "x", "priority": 2},
//	  {"local_id": "task-2", "parent": "epic-x", "depends_on": ["task-1"], "title": "Build", "type": "task", "topic": "x", "priority": 2}
//	]
//
// YAML support is a follow-up — the indirect yaml.v3 dep would have to become
// direct, and the spec types already carry yaml struct tags.
func runImportTree(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("import")
	path := fs.String("path", "", "Path to JSON tree spec file")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if strings.TrimSpace(*path) == "" {
		return errors.New("usage: lit import --path <file.json>")
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit import --path <file.json>")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read import spec: %w", err)
	}
	var specs []store.ImportTreeSpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return fmt.Errorf("parse import spec: %w", err)
	}
	result, err := ap.Store.ImportTree(ctx, specs)
	if err != nil {
		return err
	}
	return printValue(stdout, result, *jsonOut, func(w io.Writer, v any) error {
		r := v.(store.ImportTreeResult)
		if _, err := fmt.Fprintf(w, "imported %d issues\n", len(r.IDMap)); err != nil {
			return err
		}
		for local, real := range r.IDMap {
			if _, err := fmt.Fprintf(w, "  %s -> %s\n", local, real); err != nil {
				return err
			}
		}
		return nil
	})
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
	refresh := fs.Bool("refresh", false, "Refresh managed repo assets and report quickstart override status (never overwrites overrides)")
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
  followup       File a follow-up issue parented to a just-closed ticket
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
  lit followup --on <closed-id> --title "Follow-up surfaced at close"
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
