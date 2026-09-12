package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// backlog is the one remaining consumer of gatherWorkableAnnotated's shared
// query through the workableView preset shape below. `next` used to be a
// second preset here (order/keep/render over the same rows); claim routing
// gave it a genuinely different shape — a multi-step precedence over claim
// standings producing a discriminated outcome, not a single-row keep() over
// an ordered list — so it forked into its own file (next.go) rather than
// stretching this preset to fit a shape it wasn't designed for.
// [LAW:carrying-cost] (The retired ready/queue views were two further
// presets over this same query; retiring them was a surface change, not a
// query change.)

// workableKnobs carries the parsed values of every knob a workable view can
// expose. A view that does not expose a knob leaves it at the zero value,
// which every downstream stage already treats as "no narrowing" / "no-op".
// [LAW:dataflow-not-control-flow] downstream stages never ask which knobs
// exist; they consume the values unconditionally.
type workableKnobs struct {
	assignee  string
	issueType model.IssueType
	status    model.State
	labels    []string
	limit     int
	columns   []columnSpec
	all       bool
}

// workableView is the preset that specializes the one workable runner into a
// named command. The order/keep/render stages are function values so each
// preset is exactly the existing sort/selection/printer for that command —
// the runner itself stays branchless.
type workableView struct {
	name       string
	hasFilters bool // --type / --status / --labels
	hasLimit   bool
	hasColumns bool
	order      func(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, knobs workableKnobs)
	keep       func(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue
	// render receives the relationship cells already derived from the rows and
	// the graph, so a view that lets its caller NAME a relation column cannot
	// render one without the data behind it. The runner owns that derivation
	// rather than each renderer, which is what keeps the next view added here
	// from re-introducing a projection whose `parent` and `blocked` cells are
	// permanently "-". [LAW:one-source-of-truth]
	// rows are what the view prints; gathered is every workable row the pipeline
	// produced, before the scope or --limit narrowed it. They are separate
	// parameters because the per-row facts a renderer prints are not all facts
	// about the printed rows: "what closing this unblocks" and the rank-inversion
	// count are properties of the whole workable set, and computing them from the
	// view makes them shrink as the view does, silently. [LAW:one-source-of-truth]
	render func(w io.Writer, columns []columnSpec, rows, gathered []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, cells map[string]derivedColumns, cc claimContext, notice focusNotice) error
	// occasion builds the workflow event this view fires once render has
	// already succeeded on the same rows — backlog's is a constant (a
	// backlog-wide view names no single ticket), next's reads the one row
	// render just printed. [LAW:one-type-per-behavior] a third preset
	// function alongside order/keep/render, not a branch on view identity.
	occasion func(rows []annotation.AnnotatedIssue) workflows.Occasion
}

// usage derives the positional-argument error string from the knob set, in the
// fixed fragment order filters, assignee, all, limit, columns.
// [LAW:one-source-of-truth] the knobs a view exposes and the usage line that
// names them cannot drift.
func (v workableView) usage() string {
	parts := []string{"usage: lit " + v.name}
	if v.hasFilters {
		parts = append(parts, "[--type ...] [--status ...] [--labels ...]")
	}
	parts = append(parts, "[--assignee <user>]", "[--all]")
	if v.hasLimit {
		parts = append(parts, "[--limit N]")
	}
	if v.hasColumns {
		parts = append(parts, "[--columns ...]")
	}
	return strings.Join(parts, " ")
}

// readinessColumnsFor builds the derived cells for annotated rows. `parent`
// comes from the graph; `blocked` comes from ClassifyReadiness.
// [LAW:one-source-of-truth] the annotation registry decides what blocks, and
// rendering may not carry a shorter list; deriving this cell from DependsOn
// edges alone carried exactly that shorter list, and it disagreed on screen for
// any row gated by an earlier sibling, a missing field, or needs-design.
//
// This is the ONLY producer of a blocked cell. It used to serve the workable
// views alone while `lit ls` built its own from dependency edges, so one column
// name meant two different things depending on which command printed it
// (links-columns-4hdq). `lit ls` now runs the annotation pipeline when `blocked`
// is projected and lands here too, which is why there is one function rather
// than a shorter sibling — the divergence closed by deleting the second answer,
// not by teaching both of them to agree.
func readinessColumnsFor(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations) map[string]derivedColumns {
	out := make(map[string]derivedColumns, len(rows))
	for _, row := range rows {
		out[row.ID] = derivedColumns{
			parentID: parentIDOf(details[row.ID]),
			blocked:  !ClassifyReadiness(row.Annotations).IsReady(),
		}
	}
	return out
}

func orderCanonical([]annotation.AnnotatedIssue, map[string]storage.IssueRelations, workableKnobs) {}

func keepAll(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue { return rows }

// Each view answers a different question over the same query, and the answer
// is encoded entirely in its preset values:
// `backlog` — "why is the queue shaped this way": canonical rank order with
// blocked items interleaved at their ranked position, full per-row context.
var backlogView = workableView{
	name:       "backlog",
	hasFilters: true, hasLimit: true, hasColumns: true,
	order:    orderCanonical,
	keep:     keepAll,
	render:   printBacklogOutput,
	occasion: func([]annotation.AnnotatedIssue) workflows.Occasion { return backlogOccasion() },
}

// workableRun adapts a preset to the registry's appRunFn shape.
func workableRun(view workableView) appRunFn {
	return func(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
		return runWorkable(ctx, stdout, ap, args, view)
	}
}

// runWorkable is the single runner behind every workable command. It declares
// the shared flags exactly once, marshals the workableFilter exactly once, and
// executes the fixed pipeline parse → gather → order → keep → limit → render.
// [LAW:single-enforcer] the flag surface and its marshaling live only here.
func runWorkable(ctx context.Context, stdout io.Writer, ap *app.App, args []string, view workableView) error {
	fs := newCobraFlagSet(view.name)
	assignee := fs.String("assignee", "", "Filter by assignee")
	// [LAW:no-mode-explosion] This flag's cap is the focus label: it selects
	// between two values of ONE scope, it is deleted the day `focus` is, and no
	// stage downstream branches on it — runWorkable resolves it to a scope and
	// the pipeline consumes that. It exists so a scope stays a groove with the
	// way out written on it rather than a wall an agent can only escape by
	// deleting someone else's label.
	all := fs.Bool("all", false, "Ignore the focus scope and list the whole queue")
	issueType := optionalString(fs, view.hasFilters, "type", "Filter by issue type")
	status := optionalString(fs, view.hasFilters, "status", "Filter by status: open|in_progress")
	labels := optionalString(fs, view.hasFilters, "labels", "Comma-separated labels all of which must match")
	limit := optionalInt(fs, view.hasLimit, "limit", "Limit results")
	columnsExpr := optionalString(fs, view.hasColumns, "columns", columnsFlagUsage())
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: view.usage()}
	}
	statusState, err := parseWorkableStatus(*status)
	if err != nil {
		return err
	}
	issueTypeValue, err := parseWorkableType(*issueType)
	if err != nil {
		return err
	}
	// Parsed alongside the other flag boundaries, and so before the staleness
	// warning below prints: a bad column name must not reach the caller as a
	// rejection that already emitted output.
	columns, err := parseColumnSelection(*columnsExpr)
	if err != nil {
		return err
	}
	// Backlog is exactly the "ordinary read command" surface links-sync-pgct.2
	// targets: printed first, so unpushed/unfetched drift is the first thing
	// on screen rather than a diagnostic nobody runs. (`next` — next.go —
	// prints the same warning at the same position, independently, since it
	// no longer runs through this pipeline.)
	if err := printSyncStalenessWarning(ctx, stdout, ap.Workspace, ap.Store, time.Now()); err != nil {
		return err
	}
	knobs := workableKnobs{
		assignee:  strings.TrimSpace(*assignee),
		issueType: issueTypeValue,
		status:    statusState,
		labels:    splitCSV(*labels),
		limit:     *limit,
		columns:   columns,
		all:       *all,
	}
	annotated, details, focus, err := gatherWorkableAnnotated(ctx, ap, workableFilter{
		Assignee:  knobs.assignee,
		IssueType: knobs.issueType,
		Status:    knobs.status,
		Labels:    knobs.labels,
	})
	if err != nil {
		return err
	}
	// The scope narrows MEMBERSHIP and nothing else, which is why it runs before
	// ordering rather than as one more sort: what is left is then in stored rank
	// order and the preamble that says so is true again. --all resolves to the
	// unfocused scope — the same value an unlabeled workspace produces — so one
	// partition serves every case and nothing downstream learns the flag exists.
	// [LAW:dataflow-not-control-flow]
	scoped, excluded := focus.scopeFor(knobs.all).partition(annotated)
	view.order(scoped, details, knobs)
	kept := view.keep(scoped)
	rows := applyLimit(kept, knobs.limit)
	// Built AFTER the trim it reports, not beside the partition: --limit cuts
	// rows the scope kept, so a notice constructed two lines up could only ever
	// describe half the gap between what was gathered and what is printed — and
	// printed "Nothing is hidden" over the other half.
	//
	// trimmed spans keep → limit, not scope → limit, because the sentence it
	// feeds names --limit as the cause. keepAll is identity today, so the two
	// spans are equal and no output changes; they stop being equal the moment a
	// view keeps a subset, and the wider span would then report that view's own
	// drops as a --limit trim — this ticket's defect, one narrowing further out.
	// The endpoints say which narrowing is being measured.
	// [LAW:one-source-of-truth]
	notice := focusNotice{
		scope:   focus,
		applied: !knobs.all,
		hidden:  len(excluded),
		trimmed: len(kept) - len(rows),
		escape:  "`lit " + view.name + " --all`",
	}
	cc, err := gatherClaimContext(ctx, stdout, ap)
	if err != nil {
		return err
	}
	// Derived unconditionally from the rows and graph data already gathered
	// above: no extra query, and no branch deciding whether the renderer gets
	// its data. [LAW:dataflow-not-control-flow]
	if err := view.render(stdout, knobs.columns, rows, annotated, details, readinessColumnsFor(rows, details), cc, notice); err != nil {
		return err
	}
	return workflows.Dispatch(stdout, os.Stderr, ap.Workspace, view.occasion(rows))
}

// parseWorkableStatus is the strict trust boundary for --status: blank means
// "no narrowing", and only the states a workable row can hold are legal.
// Closed is rejected rather than accepted-and-empty — a filter whose result is
// empty by construction is a question the user didn't mean to ask.
// [LAW:no-silent-failure] lenient DefaultOpen coercion stays at ingestion
// boundaries (store/import); the CLI flag fails loudly instead.
func parseWorkableStatus(raw string) (model.State, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	state, err := model.ParseState(raw)
	if err != nil || state == model.StateClosed {
		return "", UsageError{Message: fmt.Sprintf("invalid --status %q (valid: open, in_progress)", raw)}
	}
	return state, nil
}

// parseWorkableType is the strict trust boundary for --type on the workable
// commands: blank means "no narrowing", anything else must be sealed
// vocabulary. A typo'd type fails loudly instead of flowing into the query and
// reporting "no ready work". [LAW:no-silent-failure]
func parseWorkableType(raw string) (model.IssueType, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	t, err := model.ParseIssueType(raw)
	if err != nil {
		return "", UsageError{Message: fmt.Sprintf("invalid --type %q: %v", raw, err)}
	}
	return t, nil
}

// optionalString registers the flag only when the view exposes it; an
// unexposed knob reads as the zero value, which the pipeline treats as "no
// narrowing". A hidden-but-registered flag would silently accept input the
// command does not honor, so unexposed means unknown-flag, loudly.
// [LAW:no-silent-failure]
func optionalString(fs *cobraFlagSet, enabled bool, name, usage string) *string {
	if !enabled {
		return new(string)
	}
	return fs.String(name, "", usage)
}

func optionalInt(fs *cobraFlagSet, enabled bool, name, usage string) *int {
	if !enabled {
		return new(int)
	}
	return fs.Int(name, 0, usage)
}
