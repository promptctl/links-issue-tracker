package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// The workable commands (ready, backlog, queue, next) are one query with four
// presentations: they all consume gatherWorkableAnnotated and differ only in
// which knobs they expose, the extra ordering they apply, which rows they keep,
// and how they render. That variability is data — a workableView preset — not
// four run functions re-declaring the same flags.
// [LAW:one-type-per-behavior] one runner; presentation is a value, not a verb.
// [LAW:no-mode-explosion] the four presets below are the closed set; no generic
// fifth command exposes the view as a flag.

// workableKnobs carries the parsed values of every knob a workable view can
// expose. A view that does not expose a knob leaves it at the zero value,
// which every downstream stage already treats as "no narrowing" / "no-op".
// [LAW:dataflow-not-control-flow] downstream stages never ask which knobs
// exist; they consume the values unconditionally.
type workableKnobs struct {
	assignee     string
	issueType    model.IssueType
	status       model.State
	labels       []string
	limit        int
	columns      []string
	continueBias bool
}

// workableView is the preset that specializes the one workable runner into a
// named command. The order/keep/render stages are function values so each
// preset is exactly the existing sort/selection/printer for that command —
// the runner itself stays branchless.
type workableView struct {
	name        string
	hasFilters  bool // --type / --status / --labels
	hasLimit    bool
	hasColumns  bool
	hasContinue bool
	order       func(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations, knobs workableKnobs)
	keep        func(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue
	render      func(w io.Writer, columns []string, rows []annotation.AnnotatedIssue) error
}

// usage derives the positional-argument error string from the knob set, in the
// fixed fragment order filters, continue, assignee, limit, columns.
// [LAW:one-source-of-truth] the knobs a view exposes and the usage line that
// names them cannot drift.
func (v workableView) usage() string {
	parts := []string{"usage: lit " + v.name}
	if v.hasFilters {
		parts = append(parts, "[--type ...] [--status ...] [--labels ...]")
	}
	if v.hasContinue {
		parts = append(parts, "[--continue]")
	}
	parts = append(parts, "[--assignee <user>]")
	if v.hasLimit {
		parts = append(parts, "[--limit N]")
	}
	if v.hasColumns {
		parts = append(parts, "[--columns ...]")
	}
	return strings.Join(parts, " ")
}

func orderCanonical([]annotation.AnnotatedIssue, map[string]store.IssueRelations, workableKnobs) {}

func keepAll(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue { return rows }

// Each view answers a different question over the same query, and the answer
// is encoded entirely in its preset values:
// `ready` — "what should the next agent work on": blocked pushed below
// unblocked (a presentation choice, so the sort lives here, not in the shared
// gather), coaching prose, sectioned.
var readyView = workableView{
	name:       "ready",
	hasFilters: true, hasLimit: true, hasColumns: true,
	order: func(rows []annotation.AnnotatedIssue, _ map[string]store.IssueRelations, _ workableKnobs) {
		sortByBlockingAnnotations(rows)
	},
	keep:   keepAll,
	render: printReadyOutput,
}

// `backlog` — "why is the queue shaped this way": canonical rank order with
// blocked items interleaved at their ranked position, full per-row context.
var backlogView = workableView{
	name:       "backlog",
	hasFilters: true, hasLimit: true, hasColumns: true,
	order:  orderCanonical,
	keep:   keepAll,
	render: printBacklogOutput,
}

// `queue` — "what is the rank-ordered pull sequence I am shaping with lit
// rank": blocked dropped, terse, uncapped, canonical order unmodified.
var queueView = workableView{
	name:       "queue",
	hasFilters: true, hasLimit: true, hasColumns: true,
	order:  orderCanonical,
	keep:   filterPullable,
	render: printQueueOutput,
}

// `next` — "the one leaf to lit start now": optional --continue bias is one
// extra stable sort over the same data; it never changes which rows are
// workable, only where we look first. Empty selection is a loud error, not an
// empty list — the agent asked for work and there is none. Filters make "the
// next workable bug" expressible; --limit/--columns stay off because a
// single-row summary has no row count or column set to vary.
var nextView = workableView{
	name:        "next",
	hasFilters:  true,
	hasContinue: true,
	order: func(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations, knobs workableKnobs) {
		if knobs.continueBias {
			sortByContinueBias(rows, details)
		}
	},
	keep: func(rows []annotation.AnnotatedIssue) []annotation.AnnotatedIssue {
		next, ok := pickFirstReady(rows)
		if !ok {
			return nil
		}
		return []annotation.AnnotatedIssue{next}
	},
	render: func(w io.Writer, _ []string, rows []annotation.AnnotatedIssue) error {
		if len(rows) == 0 {
			return errors.New("no ready work")
		}
		return printNextSummary(w, rows[0])
	},
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
	issueType := optionalString(fs, view.hasFilters, "type", "Filter by issue type")
	status := optionalString(fs, view.hasFilters, "status", "Filter by status: open|in_progress")
	labels := optionalString(fs, view.hasFilters, "labels", "Comma-separated labels all of which must match")
	limit := optionalInt(fs, view.hasLimit, "limit", "Limit results")
	columnsExpr := optionalString(fs, view.hasColumns, "columns", "Comma-separated output columns")
	continueBias := optionalBool(fs, view.hasContinue, "continue", "Bias toward leaves under in-progress epics")
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
	knobs := workableKnobs{
		assignee:     strings.TrimSpace(*assignee),
		issueType:    issueTypeValue,
		status:       statusState,
		labels:       splitCSV(*labels),
		limit:        *limit,
		columns:      parseColumns(*columnsExpr),
		continueBias: *continueBias,
	}
	annotated, details, err := gatherWorkableAnnotated(ctx, ap, workableFilter{
		Assignee:  knobs.assignee,
		IssueType: knobs.issueType,
		Status:    knobs.status,
		Labels:    knobs.labels,
	})
	if err != nil {
		return err
	}
	view.order(annotated, details, knobs)
	rows := view.keep(annotated)
	rows = applyLimit(rows, knobs.limit)
	return view.render(stdout, knobs.columns, rows)
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

func optionalBool(fs *cobraFlagSet, enabled bool, name, usage string) *bool {
	if !enabled {
		return new(bool)
	}
	return fs.Bool(name, false, usage)
}
