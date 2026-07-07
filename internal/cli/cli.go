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

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/query"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var errHelpHandled = errors.New("help handled")

const (
	humanBootstrapHelp = "Human bootstrap command. Run once per repository/worktree setup before autonomous agent operations."
	agentCommandHelp   = "Agent-facing operational command."
)

func Run(ctx context.Context, stdout io.Writer, stderr io.Writer, args []string) error {
	normalizedArgs, err := parseGlobalArgs(args)
	if err != nil {
		return err
	}
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
		Use:  "lit",
		Long: "Agent-native issue tracker",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return UnknownCommandError{Command: args[0]}
			}
			// [LAW:one-source-of-truth] Default command reuses renderQuickstartGuidance
			// so the output is always identical to `lit quickstart`.
			ws, wsErr := resolveWorkspaceFromWD()
			// [LAW:dataflow-not-control-flow] Only the "outside git repo" case routes to help;
			// other failures (getcwd, template load) surface so they remain diagnosable.
			if errors.Is(wsErr, workspace.ErrNotGitRepo) {
				return cmd.Help()
			}
			if wsErr != nil {
				return wsErr
			}
			guidance, guidanceErr := renderQuickstartGuidance(ws.RootDir)
			if guidanceErr != nil {
				return guidanceErr
			}
			_, printErr := fmt.Fprintln(cmd.OutOrStdout(), guidance)
			return printErr
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	// [CLI] Exit codes are a contract: a global-position unknown flag (e.g. the
	// removed `--json`) is a usage error, the same ExitUsage the per-command
	// parser returns, not a generic failure. [LAW:single-enforcer]
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageError{Message: err.Error()}
	})
	root.SetOut(stdout)
	root.SetErr(stderr)
	applyRegistry(root, commandGroups, commandSpecs(ctx, stdout, stderr))
	return root
}

func runWithWorkspace(run func(workspace.Info) error) error {
	ws, err := resolveWorkspaceFromWD()
	if err != nil {
		return err
	}
	return run(ws)
}

func runWithApp(ctx context.Context, accessMode app.AccessMode, run func(context.Context, *app.App) error) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	// [LAW:single-enforcer] Cobra command registration owns app access selection so startup mode is declared once per entrypoint.
	ap, err := app.Open(ctx, cwd, accessMode)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}
		}
		return err
	}
	// Capture the workspace before running: auto-sync needs it after the engine
	// is closed, and the close happens in the inner function below (including on
	// panic, as a deferred close) so it always precedes the inline receive.
	ws := ap.Workspace
	runErr := func() error {
		defer ap.Close()
		return run(ctx, ap)
	}()
	if runErr != nil {
		return runErr
	}
	// [LAW:single-enforcer] One owner consults the auto-sync policy after a
	// successful command, AND after that command's engine is closed: the on-change
	// push mirror is a detached worker that opens its own engine only once this
	// process exits, and the receive runs inline now on its own engine — so at no
	// point are two read-write engines open on the path, which embedded Dolt
	// forbids. Command handlers stay unaware of any of this.
	// [LAW:no-ambient-temporal-coupling]
	maybeAutoSyncAfterCommand(ctx, accessMode, ws)
	return nil
}

func resolveWorkspaceFromWD() (workspace.Info, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return workspace.Info{}, fmt.Errorf("get cwd: %w", err)
	}
	ws, err := workspace.Resolve(cwd)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			// [LAW:types-are-the-program] Typed error carries classification; message preserved for surface display.
			return workspace.Info{}, OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}
		}
		return workspace.Info{}, err
	}
	return ws, nil
}

func parseGlobalArgs(args []string) ([]string, error) {
	// [LAW:single-enforcer] Legacy --output rejection lives in one global parser path.
	index := 0
	for index < len(args) {
		arg := args[index]
		switch arg {
		case "--":
			index++
			goto done
		case "--output":
			return nil, unsupportedOutputFlagError()
		default:
			if strings.HasPrefix(arg, "--output=") {
				return nil, unsupportedOutputFlagError()
			}
			goto done
		}
	}

done:
	return args[index:], nil
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

// StringArray declares a repeatable string flag: each occurrence appends one
// value, with no splitting on commas, so a value may itself contain any
// character (including the multi-line merged prose the reconcile resolve carries).
func (fs *cobraFlagSet) StringArray(name string, usage string) *[]string {
	return fs.cmd.Flags().StringArray(name, nil, usage)
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

func (fs *cobraFlagSet) Arg(i int) string {
	return fs.cmd.Flags().Arg(i)
}

func (fs *cobraFlagSet) Visit(fn func(*pflag.Flag)) {
	fs.cmd.Flags().Visit(fn)
}

func (fs *cobraFlagSet) Changed(name string) bool {
	return fs.cmd.Flags().Changed(name)
}

// Hide marks a flag as hidden so it does not appear in help output. The flag
// itself remains functional for any caller that still passes it explicitly.
func (fs *cobraFlagSet) Hide(name string) {
	_ = fs.cmd.Flags().MarkHidden(name)
}

func (fs *cobraFlagSet) printHelp(helpOutput io.Writer) error {
	fs.SetOutput(helpOutput)
	if _, writeErr := fmt.Fprintf(helpOutput, "Usage of %s:\n", fs.cmd.Use); writeErr != nil {
		return writeErr
	}
	fs.cmd.Flags().PrintDefaults()
	return nil
}

func parseFlagSet(fs *cobraFlagSet, args []string, stdout io.Writer) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			// [LAW:single-enforcer] Flag help rendering is normalized in one Cobra parser path.
			if helpErr := fs.printHelp(stdout); helpErr != nil {
				return helpErr
			}
			return errHelpHandled
		}
		// [LAW:types-are-the-program] Wrap pflag errors at the parse boundary so sinks dispatch on type, not message text.
		msg := err.Error()
		if strings.Contains(msg, "flag provided but not defined: -output") ||
			strings.Contains(msg, "flag provided but not defined: --output") {
			return UnsupportedError{Message: "--output is no longer supported; omit it for text output", Feature: "--output"}
		}
		if strings.HasPrefix(msg, "unknown flag:") || strings.HasPrefix(msg, "flag provided but not defined:") {
			return UsageError{Message: msg}
		}
		return err
	}
	if helpFlag := fs.cmd.Flags().Lookup("help"); helpFlag != nil && helpFlag.Changed {
		// [LAW:single-enforcer] Parsed help flags follow the same Cobra help rendering path as explicit help errors.
		if helpErr := fs.printHelp(stdout); helpErr != nil {
			return helpErr
		}
		return errHelpHandled
	}
	return nil
}

func unsupportedOutputFlagError() error {
	return UnsupportedError{Message: "--output is no longer supported; omit it for text output", Feature: "--output"}
}

// rankPlacement translates the CLI's --bottom boolean into the domain
// placement value at the boundary, so the default (no flag) surfaces fresh
// work at the top.
func rankPlacement(bottom bool) store.RankPlacement {
	if bottom {
		return store.RankBottom
	}
	return store.RankTop
}

func runNew(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("new")
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	prompt := fs.String("prompt", "", "Reusable agent prompt for the work this issue captures")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	topic := fs.String("topic", "", "Required immutable issue topic slug (1-2 words; stable area of focus; e.g., 'refactor' or 'field-history')")
	parentID := fs.String("parent", "", "Optional parent issue ID; child IDs become parentID.<n>")
	priority := fs.Int("priority", model.PriorityNormal, "Priority: 0=normal, 1=urgent")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	lane := fs.String("lane", "", "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes")
	bottom := fs.Bool("bottom", false, "Rank the new issue at the bottom of the order instead of the top (the default surfaces fresh work at the top)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title: *title, Description: *description, Prompt: *prompt, IssueType: *issueType, Topic: *topic, ParentID: *parentID, Priority: *priority, Assignee: strings.TrimSpace(*assignee), Labels: splitCSV(*labels), Lane: *lane,
		Placement: rankPlacement(*bottom),
		Prefix:    ap.Workspace.IssuePrefix.Value(),
	})
	if err != nil {
		return err
	}
	if err := printIssueSummary(stdout, issue); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "new")
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
	priority := fs.Int("priority", model.PriorityNormal, "Priority: 0=normal, 1=urgent")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	bottom := fs.Bool("bottom", false, "Rank the follow-up at the bottom of the order instead of the top (the default surfaces fresh work at the top)")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	parentID := strings.TrimSpace(*on)
	titleValue := strings.TrimSpace(*title)
	if parentID == "" || titleValue == "" {
		return UsageError{Message: "usage: lit followup --on <id> --title <text> [--description <text>] [--topic <slug>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--bottom]"}
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
		Assignee:    strings.TrimSpace(*assignee),
		Labels:      splitCSV(*labels),
		Placement:   rankPlacement(*bottom),
		Prefix:      ap.Workspace.IssuePrefix.Value(),
	})
	if err != nil {
		return err
	}
	if err := printIssueSummary(stdout, issue); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "new")
}

func runList(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("ls")
	status := fs.String("status", "", "Filter by status: open|in_progress|closed")
	issueType := fs.String("type", "", "Filter by issue type")
	assignee := fs.String("assignee", "", "Filter by assignee")
	search := fs.String("search", "", "Search title and description text")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	hasComments := fs.Bool("has-comments", false, "Only include issues with comments")
	includeArchived := fs.Bool("include-archived", false, "Include archived issues")
	includeDeleted := fs.Bool("include-deleted", false, "Include deleted issues")
	updatedAfter := fs.String("updated-after", "", "Only include issues updated at or after RFC3339 timestamp")
	updatedBefore := fs.String("updated-before", "", "Only include issues updated at or before RFC3339 timestamp")
	queryExpr := fs.String("query", "", "Query language: status:in_progress resolution:wontfix type:task has:comments text")
	sortExpr := fs.String("sort", "", "Sort fields, e.g. rank:asc,updated_at:desc")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	format := fs.String("format", "lines", "Output format: lines|table")
	limit := fs.Int("limit", 0, "Limit results")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) { visited[f.Name] = true })
	statuses, err := parseStateSlice(strings.TrimSpace(*status))
	if err != nil {
		return fmt.Errorf("parse --status: %w", err)
	}
	filter := store.ListIssuesFilter{
		Statuses:        statuses,
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
		filter.Statuses = []model.State{model.StateOpen, model.StateInProgress}
	}
	issues, err := ap.Store.ListIssues(ctx, filter)
	if err != nil {
		return err
	}
	columns := parseColumns(*columnsExpr)
	rels, err := listRelationColumns(ctx, ap.Store, columns, issues)
	if err != nil {
		return err
	}
	formatMode := strings.ToLower(strings.TrimSpace(*format))
	switch formatMode {
	case "", "lines":
		return printIssueLines(stdout, issues, columns, rels)
	case "table":
		return printIssueTable(stdout, issues, columns, rels)
	default:
		return UnsupportedError{Message: fmt.Sprintf("unsupported --format %q", formatMode), Feature: "--format"}
	}
}

// listRelationColumns builds the per-issue relationship facts the relationship
// columns project, but only when one is actually selected — the default and
// every non-relationship projection load no relation-graph data and render
// byte-for-byte as before. The projected column set is the data that selects the
// load; a nil result means "no relationship column asked for", and every
// formatter lookup then yields the zero relationColumns.
// [LAW:dataflow-not-control-flow] which data to load is a value (the column set),
// not a forked code path.
// [LAW:one-source-of-truth] reuses fetchIssueRelations + the canonical graph
// rather than reinterpreting parent/blocks edges for the list view.
func listRelationColumns(ctx context.Context, st *store.Store, columns []string, issues []model.Issue) (map[string]relationColumns, error) {
	if !projectsRelationColumn(resolveColumns(columns)) {
		return nil, nil
	}
	relations, err := fetchIssueRelations(ctx, st, issues)
	if err != nil {
		return nil, err
	}
	out := make(map[string]relationColumns, len(relations))
	for id, rel := range relations {
		out[id] = deriveRelationColumns(rel)
	}
	return out, nil
}

// deriveRelationColumns projects one issue's graph edges down to the flat facts
// the list columns show: its parent/epic id, and whether a still-live dependency
// blocks it. Blocked reuses liveIssues — the single liveness predicate — so the
// list's "blocked" cannot drift from the close view's "unblocks".
// [LAW:single-enforcer] liveness decided once, in isLiveIssue.
func deriveRelationColumns(rel store.IssueRelations) relationColumns {
	parentID := ""
	if rel.Parent != nil {
		parentID = rel.Parent.ID
	}
	return relationColumns{
		parentID: parentID,
		blocked:  len(liveIssues(rel.DependsOn)) > 0,
	}
}

// workableFilter carries the user-supplied narrowing options for the
// shared workable pipeline. Empty fields mean "no narrowing"; the
// workable definition (open/in_progress, leaves only) is layered on top
// by gatherWorkableAnnotated.
type workableFilter struct {
	Assignee  string
	IssueType string
	Status    string
	Labels    []string
}

// gatherWorkableAnnotated runs the shared workable pipeline: list workable
// leaves, fetch details, annotate, sort into canonical priority/rank order,
// enrich with parent epic refs. Returns the prepared rows and the details
// map so callers that need extra row context (e.g. `lit next --continue`)
// avoid a second fetch round-trip.
//
// The returned order is the canonical backlog order: focus path first, then
// priority desc, then composite rank asc. Ready-specific presentation (e.g.
// pushing blocked items to the bottom) is applied by the caller, not here, so
// consumers that want the unmodified ranking (`lit backlog`) see it as ordered.
//
// [LAW:single-enforcer] `lit ready`, `lit next`, and `lit backlog` all
// read from this single pipeline so their "what is workable, in what
// order" model cannot drift.
func gatherWorkableAnnotated(ctx context.Context, ap *app.App, rf workableFilter) ([]annotation.AnnotatedIssue, map[string]store.IssueRelations, error) {
	cfg, err := config.Load(pathspec.New(ap.Workspace.RootDir))
	if err != nil {
		return nil, nil, err
	}
	statuses := []model.State{model.StateOpen, model.StateInProgress}
	if rf.Status != "" {
		// User-supplied status overrides the workable default. Unrecognized
		// values default to Open via DefaultOpen — the shared pipeline mirrors
		// store/import lenient parsing rather than enforcing strict CLI flag
		// validation. [LAW:comments-explain-why-only]
		statuses = []model.State{model.DefaultOpen(rf.Status)}
	}
	// [LAW:one-source-of-truth] rank is the canonical ordering; no explicit SortBy
	// needed — the store default is item_rank ASC.
	listFilter := store.ListIssuesFilter{
		Statuses:        statuses,
		IssueTypes:      toSlice(rf.IssueType),
		Assignees:       toSlice(rf.Assignee),
		LabelsAll:       rf.Labels,
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
	details, err := fetchIssueRelations(ctx, ap.Store, issues)
	if err != nil {
		return nil, nil, err
	}
	// The lane gate reads the parent epics' FULL child sets (unfiltered by the
	// CLI assignee/type/label narrowing) so an earlier sibling hidden by those
	// filters still gates its later same-lane mates.
	siblingRelations, err := ap.Store.GetRelationsByIDs(ctx, parentEpicIDs(details))
	if err != nil {
		return nil, nil, err
	}
	// The focus path is derived from the full dependency DAG (unfiltered by the
	// CLI narrowing) on every gather — the focus fact lives on the one goal
	// ticket; chain membership is never stored, so it cannot drift.
	// [LAW:one-source-of-truth]
	//
	// The walk reuses the relations already fetched for the workable leaves
	// (details) and their parent epics (siblingRelations) rather than re-querying
	// the same subjects; both are GetRelationsByIDs results, so a seeded hit is
	// byte-identical to a refetch. (links-query-efficiency-988d.2)
	focusPaths, err := fetchFocusPathGoals(ctx, ap.Store, details, siblingRelations)
	if err != nil {
		return nil, nil, err
	}
	annotated, err := annotation.Annotate(ctx, issues,
		fieldAnnotator,
		newBlockerAnnotator(details),
		newSiblingGateAnnotator(details, pendingSiblingsByEpic(siblingRelations)),
		newOrphanedAnnotator(orphanedThreshold),
		newNeedsDesignAnnotator(),
		newFocusPathAnnotator(focusPaths),
	)
	if err != nil {
		return nil, nil, err
	}
	sortByCompositeRank(annotated, details)
	sortByPriority(annotated)
	sortByFocusPath(annotated)
	enrichWithParentEpic(annotated, details)
	return annotated, details, nil
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
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit orphaned [--assignee <user>]"}
	}
	listFilter := store.ListIssuesFilter{
		Statuses:        []model.State{model.StateInProgress},
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
	return printOrphanedText(stdout, orphaned)
}

func printOrphanedText(w io.Writer, rows []annotation.AnnotatedIssue) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No orphaned issues.")
		return err
	}
	columns := []string{"id", "state", "topic", "assignee", "title"}
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

func runShow(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("show")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit show <id>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit show <id>"}
	}
	detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
	if err != nil {
		return err
	}
	if err := printIssueDetail(stdout, detail); err != nil {
		return err
	}
	return writeEpicContext(ctx, ap.Store, stdout, detail)
}

func runUpdate(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("update")
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	prompt := fs.String("prompt", "", "Reusable agent prompt for the work this issue captures")
	issueType := fs.String("type", "", "Issue type: task|feature|bug|chore|epic")
	priority := fs.Int("priority", model.PriorityNormal, "Priority: 0=normal, 1=urgent") // [LAW:one-source-of-truth] default derives from model constant; matches runNew/runFollowup
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	lane := fs.String("lane", "", "Lane key partitioning an epic's children into parallel rank-ordered sub-sequences; shared lane serializes, distinct lane parallelizes")
	status := fs.String("status", "", "Status: open|in_progress|closed")
	reason := fs.String("reason", "", "Status transition reason")
	resolveActor := registerActor(fs)
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--lane <key>] [--status <open|in_progress|closed>] [--reason <text>]"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit update <id> [--title <text>] [--description <text>] [--prompt <text>] [--type <task|feature|bug|chore|epic>] [--priority <0|1>] [--assignee <user>] [--labels <csv>] [--lane <key>] [--status <open|in_progress|closed>] [--reason <text>]"}
	}
	visited := map[string]bool{}
	fs.Visit(func(flag *pflag.Flag) { visited[flag.Name] = true })
	if visited["status"] && strings.TrimSpace(*status) == "" {
		return errors.New("--status requires a non-empty value")
	}

	// [LAW:dataflow-not-control-flow] Always build one Change; variability lives in empty fields/action, not in which branch runs.
	// The reason applies to both the transition event (Reason) and the field
	// event (Fields.Reason). The actor resolves through the same identity rule
	// as the assignee — the agent's session wins, else --by/$USER — and is
	// recorded on every event the change produces. [LAW:single-enforcer]
	in := store.Change{
		Reason: strings.TrimSpace(*reason),
		Actor:  resolveActor(),
		Fields: store.UpdateIssueInput{
			Reason: strings.TrimSpace(*reason),
		},
	}
	if visited["status"] {
		// CLI flags are a strict boundary: an unrecognized status is an error,
		// never leniently defaulted — DefaultOpen here would silently turn a
		// typo into a reopen. [LAW:no-silent-failure]
		targetState, err := model.ParseState(*status)
		if err != nil {
			return err
		}
		// Target state → canonical action is boundary constructor policy, owned
		// where the "make the status X" intent is expressed. Closed constructs
		// Done — the neutral success close — because a resolution-less Close is
		// unconstructible; `lit close` is the boundary that records outcomes.
		switch targetState {
		case model.StateOpen:
			in.Action = model.Reopen{}
		case model.StateInProgress:
			// Mirror `lit start`: when the user expressed no assignee intent at
			// all, ask the resolver so a bare `--status in_progress` still picks
			// up CLAUDE_CODE_SESSION_ID. The discriminator is flag presence, not
			// value emptiness — an explicit empty is a clear, never an invitation
			// to self-assign. [LAW:no-silent-failure]
			transitionAssignee := resolveIdentity("")
			if visited["assignee"] {
				transitionAssignee = strings.TrimSpace(*assignee)
			}
			in.Action = model.Start{Assignee: transitionAssignee}
		case model.StateClosed:
			in.Action = model.Done{}
		}
		if in.Reason == "" {
			// The synthesized transition reason is `lit update` provenance, not
			// store policy, so it is composed here at the command boundary.
			prior, err := ap.Store.GetIssue(ctx, positional[0])
			if err != nil {
				return err
			}
			in.Reason = fmt.Sprintf("status update via lit update: %s -> %s", prior.StatusValue(), targetState)
		}
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
		return errors.New("lit update requires at least one field flag")
	}
	issue, err := ap.Store.Apply(ctx, positional[0], in)
	if err != nil {
		return err
	}
	if err := printIssueSummary(stdout, issue); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
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
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit rank <id> --top|--bottom|--above <id>|--below <id>"}
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
		return ValidationError{Message: "exactly one of --top, --bottom, --above, --below is required"}
	}
	issueID := positional[0]
	// Relative ops resolve cross-frame requests to the comparable pair (a
	// child's stand-in is its epic); the move record carries what actually
	// happened so the substitution is reported, never silent.
	// [LAW:no-silent-failure]
	move := store.RankMove{MovedID: issueID, AnchorID: issueID}
	var err error
	switch {
	case visited["top"]:
		err = ap.Store.RankToTop(ctx, issueID)
	case visited["bottom"]:
		err = ap.Store.RankToBottom(ctx, issueID)
	case visited["above"]:
		move, err = ap.Store.RankAbove(ctx, issueID, *above)
	case visited["below"]:
		move, err = ap.Store.RankBelow(ctx, issueID, *below)
	}
	if err != nil {
		return err
	}
	namedAnchor := *above + *below // exactly one mode is set; empty for --top/--bottom
	// A relative op against an issue inside an epic ranks the epic as its
	// stand-in; report the substitution so it is never silent. [LAW:no-silent-failure]
	if move.MovedID != issueID {
		if _, err := fmt.Fprintf(stdout, "%s is inside %s; ranked the epic %s instead, leaving its internal order unchanged\n", issueID, move.MovedID, move.MovedID); err != nil {
			return err
		}
	}
	if namedAnchor != "" && move.AnchorID != namedAnchor {
		if _, err := fmt.Fprintf(stdout, "%s is inside %s; ranked relative to the epic %s instead\n", namedAnchor, move.AnchorID, move.AnchorID); err != nil {
			return err
		}
	}
	issue, err := ap.Store.GetIssue(ctx, move.MovedID)
	if err != nil {
		return err
	}
	if err := printIssueSummary(stdout, issue); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
}

// runRankSet establishes absolute order across N issues by stacking them at
// the top in the given sequence: id1 becomes the topmost, id2 ranks just
// below, etc. Atomic: the store either applies all or none. IDs inside an
// epic resolve to the epic itself; the substitution is reported, never
// silent. [LAW:no-silent-failure]
func runRankSet(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, len(args))
	fs := newCobraFlagSet("rank set")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) < 2 {
		return UsageError{Message: "usage: lit rank set <id1> <id2> [<id3> ...]"}
	}
	resolutions, err := ap.Store.RankSet(ctx, positional)
	if err != nil {
		return err
	}
	ranked := make([]string, len(resolutions))
	for i, r := range resolutions {
		ranked[i] = r.RankedID
	}
	for _, r := range resolutions {
		if r.RankedID != r.NamedID {
			if _, err := fmt.Fprintf(stdout, "%s is inside %s; ranked the epic %s instead, leaving its internal order unchanged\n", r.NamedID, r.RankedID, r.RankedID); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(stdout, "ranked %d issues at top in order: %s\n", len(ranked), strings.Join(ranked, ", ")); err != nil {
		return err
	}
	return emitBreadcrumb(stdout, "update")
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

// resolveIdentity returns the identity this lit invocation acts as — used for
// both the assignee (who owns the work) and the event actor (who performed the
// transition), which are the same agent. When CLAUDE_CODE_SESSION_ID is set the
// value is always claude_<sessionId>, regardless of what (if anything) the
// caller passed. When the env var is empty the caller's explicit value (an
// --assignee or --by fallback, trimmed) passes through.
// [LAW:one-source-of-truth] sole producer of the acting identity; assignee and
// actor cannot diverge because both derive from this one rule.
// [LAW:types-are-the-program] env presence is the discriminator; no callsite
// re-decides precedence or re-parses the env var.
func resolveIdentity(explicit string) string {
	if sessionID := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); sessionID != "" {
		return "claude_" + sessionID
	}
	return strings.TrimSpace(explicit)
}

// actorResolver yields the identity this lit invocation acts as. It is the only
// way to read the --by flag: registerActor captures the raw flag pointer and
// never exposes it, so the unresolved $USER fallback cannot reach a
// CreatedBy/actor field — every value a callsite can obtain has already passed
// through resolveIdentity. [LAW:single-enforcer] one boundary resolves the
// actor for every mutating command; [LAW:types-are-the-program] the
// raw-$USER-to-CreatedBy path is unrepresentable, so a new mutating command
// cannot reintroduce the split-provenance bug by forgetting to resolve.
type actorResolver func() string

// registerActor declares the hidden --by fallback flag on fs and returns the
// resolver that reads it. Call the returned resolver after parseFlagSet has run.
// [LAW:one-source-of-truth] the $USER default lives here alone, not at each
// mutating callsite.
func registerActor(fs *cobraFlagSet) actorResolver {
	by := fs.String("by", os.Getenv("USER"), "")
	fs.Hide("by")
	return func() string { return resolveIdentity(*by) }
}

// displayAssignee renders an assignee value for human output; the empty value
// means "nobody owns this" and must read that way rather than vanish.
func displayAssignee(assignee string) string {
	if assignee == "" {
		return "(unassigned)"
	}
	return assignee
}

// transitionBreadcrumbTopics maps transition actions to the quickstart topic
// whose guidance follows naturally from that success: claiming work points at
// the finding-work guidance, finishing (done or close) at the wrap-up
// guidance. Absence from the table is the encoding of "no natural follow-on
// topic" (archive, delete, reopen, ...), not a skipped branch.
// [LAW:dataflow-not-control-flow]
var transitionBreadcrumbTopics = map[model.ActionName]string{
	model.ActionStart: "ready",
	model.ActionDone:  "done",
	model.ActionClose: "done",
}

// transitionSpec describes one lifecycle transition command: its name and how
// its parsed flags become the typed action value. Each command registers only
// the flags its action consumes, so flag misuse on any other transition is the
// parser's unknown-flag error — the per-action runtime flag rejections this
// file used to carry are unrepresentable. [LAW:types-are-the-program]
// [LAW:one-type-per-behavior] The eight transition commands are instances of
// one spec, not eight handlers; variability lives in the spec value.
type transitionSpec struct {
	name string
	// registerFlags declares the action's own flags on fs and returns the
	// builder invoked after parsing, which turns the flag values into the
	// sealed action variant.
	registerFlags func(fs *cobraFlagSet) func() (model.Action, error)
}

// fixedAction is the registerFlags of every transition whose action carries no
// payload and therefore consumes no flags.
func fixedAction(action model.Action) func(fs *cobraFlagSet) func() (model.Action, error) {
	return func(*cobraFlagSet) func() (model.Action, error) {
		return func() (model.Action, error) { return action, nil }
	}
}

var (
	startSpec = transitionSpec{name: "start", registerFlags: func(fs *cobraFlagSet) func() (model.Action, error) {
		// Start is the claim transition and the only action that carries an
		// assignee. The resolver overrides the flag with CLAUDE_CODE_SESSION_ID
		// whenever set; the flag survives only as a fallback for environments
		// without the env var.
		assignee := fs.String("assignee", "", "Assignee fallback when CLAUDE_CODE_SESSION_ID is unset (env always wins when set)")
		return func() (model.Action, error) {
			return model.Start{Assignee: resolveIdentity(*assignee)}, nil
		}
	}}
	doneSpec  = transitionSpec{name: "done", registerFlags: fixedAction(model.Done{})}
	closeSpec = transitionSpec{name: "close", registerFlags: func(fs *cobraFlagSet) func() (model.Action, error) {
		resolution, target := registerCloseOutcomeFlags(fs)
		return func() (model.Action, error) {
			outcome, err := closeOutcomeFromFlags(*resolution, *target, "usage: lit close <id> --resolution <duplicate|superseded|obsolete|wontfix> [--of <canonical-id>] [--reason <text>]")
			if err != nil {
				return nil, err
			}
			return model.Close{Outcome: outcome}, nil
		}
	}}
	openSpec      = transitionSpec{name: "open", registerFlags: fixedAction(model.Reopen{})}
	archiveSpec   = transitionSpec{name: "archive", registerFlags: fixedAction(model.Archive{})}
	unarchiveSpec = transitionSpec{name: "unarchive", registerFlags: fixedAction(model.Unarchive{})}
	deleteSpec    = transitionSpec{name: "delete", registerFlags: fixedAction(model.Delete{})}
	restoreSpec   = transitionSpec{name: "restore", registerFlags: fixedAction(model.Restore{})}
)

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

func runTransition(ctx context.Context, stdout io.Writer, ap *app.App, args []string, spec transitionSpec) error {
	fs := newCobraFlagSet(spec.name)
	reason := fs.String("reason", "", "Transition reason")
	resolveActor := registerActor(fs)
	buildAction := spec.registerFlags(fs)
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	remaining := fs.cmd.Flags().Args()
	usage := fmt.Sprintf("usage: lit %s <id> [--reason <text>]", spec.name)
	if len(remaining) != 1 {
		return errors.New(usage)
	}

	issueID := remaining[0]

	// The pre-transition read feeds the claim-transfer notice below: `start` may
	// take an issue over from a prior owner, and that hand-off must be surfaced.
	prior, err := ap.Store.GetIssue(ctx, issueID)
	if err != nil {
		return err
	}

	action, err := buildAction()
	if err != nil {
		return err
	}

	// [LAW:single-enforcer] The event actor resolves through the same identity
	// rule as the assignee: the agent's session wins, else --by/$USER. History
	// must record who actually performed the transition (claude_<session>), not
	// the shell user, now that ownership survives close as an orthogonal field.
	actor := resolveActor()
	issue, err := ap.Store.Apply(ctx, issueID, store.Change{Action: action, Actor: actor, Reason: *reason})
	if err != nil {
		return err
	}

	// [LAW:no-silent-failure] start rewrites the assignee column; taking an
	// issue over from an existing owner succeeds (intended target-state
	// semantics) but must not do so silently.
	if start, ok := action.(model.Start); ok {
		if priorOwner := prior.AssigneeValue(); priorOwner != "" && priorOwner != start.Assignee {
			if _, err := fmt.Fprintf(stdout, "claim transferred: %s -> %s\n", priorOwner, displayAssignee(start.Assignee)); err != nil {
				return err
			}
		}
	}
	postGuidance, hasPostGuidance, err := loadTransitionGuidance(action.Name(), "post", ap.Workspace.RootDir)
	if err != nil {
		return fmt.Errorf("load post-guidance: %w", err)
	}
	if hasPostGuidance {
		rendered := renderGuidance(postGuidance, issueID, "")
		if _, err := fmt.Fprintln(stdout, rendered); err != nil {
			return err
		}
	}

	if err := printIssueSummary(stdout, issue); err != nil {
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
		if err := printCloseAdjacency(stdout, detail); err != nil {
			return err
		}
	}
	if topic, ok := transitionBreadcrumbTopics[action.Name()]; ok {
		return emitBreadcrumb(stdout, topic)
	}
	return nil
}

// runAssign rewrites the assignee column on an issue without changing status.
// Flows through Store.Apply as an action-less change so the resulting event
// row is a normal field-update event — there is no special "assign" action
// type, just a generic field mutation. [LAW:one-type-per-behavior]
func runAssign(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 2)
	fs := newCobraFlagSet("assign")
	reason := fs.String("reason", "", "Reassignment reason (optional)")
	resolveActor := registerActor(fs)
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 2 || fs.NArg() != 0 {
		return UsageError{Message: "usage: lit assign <id> <new-assignee> [--reason <text>]"}
	}
	id := positional[0]
	newAssignee := strings.TrimSpace(positional[1])
	if newAssignee == "" {
		return errors.New("new assignee cannot be empty")
	}
	issue, err := ap.Store.Apply(ctx, id, store.Change{
		// [LAW:single-enforcer] Actor resolves through the shared identity rule;
		// the second positional arg is the new owner, the actor is who acted.
		Actor: resolveActor(),
		Fields: store.UpdateIssueInput{
			Assignee: &newAssignee,
			Reason:   *reason,
		},
	})
	if err != nil {
		return err
	}
	return printIssueSummary(stdout, issue)
}

var commentFamily = commandFamily[appSubcommand]{
	usage: "usage: lit comment <add|rm> ...",
	subcommands: []subcommandRow[appSubcommand]{
		{name: "add", payload: appSubcommand{access: app.AccessWrite, run: runCommentAdd}},
		{name: "rm", payload: appSubcommand{access: app.AccessWrite, run: runCommentRm}},
	},
}

func runCommentAdd(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("comment add")
	body := fs.String("body", "", "Comment body")
	resolveActor := registerActor(fs)
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit comment add <id> --body <text>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit comment add <id> --body <text>"}
	}
	// [LAW:single-enforcer] A comment is a recorded event; its author resolves
	// through the same identity rule as every other actor.
	comment, err := ap.Store.AddComment(ctx, store.AddCommentInput{IssueID: positional[0], Body: *body, CreatedBy: resolveActor()})
	if err != nil {
		return err
	}
	return printComment(stdout, comment)
}

func runCommentRm(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	fs := newCobraFlagSet("comment rm")
	if err := parseFlagSet(fs, flagArgs, stdout); err != nil {
		return err
	}
	if len(positional) != 1 {
		return UsageError{Message: "usage: lit comment rm <comment-id>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit comment rm <comment-id>"}
	}
	comment, err := ap.Store.DeleteComment(ctx, positional[0])
	if err != nil {
		return err
	}
	return printComment(stdout, comment)
}

func printComment(w io.Writer, c model.Comment) error {
	_, err := fmt.Fprintf(w, "%s %s\n", c.IssueID, c.ID)
	return err
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
//	  {"local_id": "epic-x", "title": "Build X", "type": "epic", "topic": "x", "priority": 0},
//	  {"local_id": "task-1", "parent": "epic-x", "title": "Design", "type": "task", "topic": "x", "priority": 0},
//	  {"local_id": "task-2", "parent": "epic-x", "depends_on": ["task-1"], "title": "Build", "type": "task", "topic": "x", "priority": 0}
//	]
func runImportTree(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("import")
	path := fs.String("path", "", "Path to JSON tree spec file")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if strings.TrimSpace(*path) == "" {
		return UsageError{Message: "usage: lit import --path <file.json>"}
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit import --path <file.json>"}
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read import spec: %w", err)
	}
	specs, err := store.ParseImportTreeSpecs(data)
	if err != nil {
		return err
	}
	result, err := ap.Store.ImportTree(ctx, ap.Workspace.IssuePrefix.Value(), specs)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "imported %d issues\n", len(result.IDMap)); err != nil {
		return err
	}
	for local, real := range result.IDMap {
		if _, err := fmt.Fprintf(stdout, "  %s -> %s\n", local, real); err != nil {
			return err
		}
	}
	return nil
}

func runWorkspace(stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("workspace")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	// [LAW:one-source-of-truth] The ordered slice is the single source of the
	// output's field order; each line is `key: value`, parseable by an agent
	// that needs one field (e.g. `lit workspace | sed -n 's/^traces_dir: //p'`).
	fields := []struct{ key, value string }{
		{"workspace_id", ws.WorkspaceID},
		{"issue_prefix", ws.IssuePrefix.Value()},
		{"git_common_dir", ws.GitCommonDir},
		{"storage_dir", ws.StorageDir},
		{"database_path", ws.DatabasePath},
		{"dolt_repo_path", ws.DoltRepoPath},
		{"traces_dir", automationTraceDir(ws)},
	}
	for _, f := range fields {
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", f.key, f.value); err != nil {
			return err
		}
	}
	return nil
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

func runCompletion(stdout io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New(completionFamily.usage)
	}
	shell, err := completionFamily.resolve(args)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, completionRenderer(shell)())
	return err
}

func runQuickstart(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	_ = ctx
	fs := newCobraFlagSet("quickstart")
	refresh := fs.Bool("refresh", false, "Refresh managed repo assets and report quickstart override status (never overwrites overrides)")
	eject := fs.StringOptional("eject", "all", "", "Eject embedded default(s) to the global override path (comma-separated short names; empty = all)")
	force := fs.Bool("force", false, "With --eject, overwrite existing override files")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return UsageError{Message: quickstartUsage}
	}
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

	if fs.NArg() == 1 {
		// [LAW:dataflow-not-control-flow] Topic dispatch is a value lookup; every topic shares one render path.
		if *refresh || ejectChanged || *force {
			return UsageError{Message: "usage: lit quickstart <topic> takes no flags"}
		}
		templateName, ok := quickstartTopicTemplate(fs.Arg(0))
		if !ok {
			return UsageError{Message: fmt.Sprintf("usage: unknown quickstart topic %q (must be one of: %s)", fs.Arg(0), strings.Join(quickstartTopicTokens(), ", "))}
		}
		guidance, err := renderQuickstartTopic(ws.RootDir, templateName)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, guidance)
		return err
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
		lines = append(lines, formatQuickstartRefreshSummary(refreshReport), "")
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

func parseStateSlice(s string) ([]model.State, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	state, err := model.ParseState(s)
	if err != nil {
		return nil, err
	}
	return []model.State{state}, nil
}

func toSlice(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// loadTransitionGuidance loads a guidance template for the given action/phase.
// Returns the template content, whether it exists, and any I/O error.
// Absent templates are not an error — they simply deactivate the guidance flow.
func loadTransitionGuidance(action model.ActionName, phase, workspaceRoot string) (string, bool, error) {
	content, err := templates.LoadGuidance(string(action), phase, workspaceRoot)
	if err != nil {
		return "", false, err
	}
	if content == "" {
		return "", false, nil
	}
	return strings.TrimSpace(content), true, nil
}

// renderGuidance interpolates <id> and <token> placeholders in a guidance
// template. token may be empty (e.g. for post-guidance, where the apply token
// no longer applies); in that case <token> is replaced with the empty string.
func renderGuidance(template string, issueID string, token string) string {
	out := strings.ReplaceAll(template, "<id>", issueID)
	out = strings.ReplaceAll(out, "<token>", token)
	return out
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
				return nil, UnsupportedError{Message: fmt.Sprintf("unsupported sort direction %q", direction), Feature: "sort-direction"}
			}
		}
		out = append(out, store.SortSpec{Field: field, Desc: desc})
	}
	return out, nil
}

type MergeConflictError struct {
	Message string
}

func (e MergeConflictError) Error() string {
	return e.Message
}

type CorruptionError struct {
	Message string
}

func (e CorruptionError) Error() string { return e.Message }

// UsageError signals wrong CLI usage (bad argument count, unrecognised flag).
// [LAW:types-are-the-program] The type carries the ExitUsage classification so sinks dispatch on type.
type UsageError struct {
	Message string
}

func (e UsageError) Error() string { return e.Message }

// UnknownCommandError signals that the router received a command name it does not recognise.
type UnknownCommandError struct {
	Command string
}

func (e UnknownCommandError) Error() string { return fmt.Sprintf("unknown command %q", e.Command) }

// ValidationError signals that a user-supplied value failed a domain constraint check.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }

// UnsupportedError signals use of a removed or unsupported feature.
// Feature names the unsupported capability (e.g. "--output") for targeted remediation.
type UnsupportedError struct {
	Message string
	Feature string
}

func (e UnsupportedError) Error() string { return e.Message }

// OutsideWorkspaceError signals that the command requires a git repository context.
type OutsideWorkspaceError struct {
	Message string
}

func (e OutsideWorkspaceError) Error() string { return e.Message }

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
