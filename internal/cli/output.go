package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"gopkg.in/yaml.v3"
)

// contextIndent is the single indent used for every context line printed under
// a ready or backlog row. [LAW:one-source-of-truth] one width, referenced by
// both renderers, so a change moves both views together.
const contextIndent = "    "

// The CLI owns history timestamp presentation; store/model keep time.Time as
// canonical data. [LAW:one-source-of-truth]
const historyTimestampLayout = "Jan 2, 2006 3:04 PM MST"

// formatEpicLine renders the "epic:" context text for a ref, and "" — the
// printer's "no line" value — for the absent ref. Formatting is split from
// printing because the backlog must choose between this text and a different
// one for the same row, and both spellings of an epic line have to come from
// here. [LAW:one-source-of-truth]
func formatEpicLine(epic *annotation.ParentEpicRef) string {
	if epic == nil {
		return ""
	}
	return singleLine(fmt.Sprintf("epic: %s  %s", epic.ID, epic.Title))
}

// printEpicLine renders the indented "epic:" context line shown identically
// under ready and backlog rows. A nil ref (issue has no epic parent) emits
// nothing — absence is data, not a caller-side branch.
// [LAW:dataflow-not-control-flow]
func printEpicLine(w io.Writer, indent string, epic *annotation.ParentEpicRef) error {
	return printContextLine(w, indent, formatEpicLine(epic))
}

// epicID names the epic a ref points at, and "" for the absent ref. It sits
// beside printEpicLine so ParentEpicRef's nil case is answered in one file
// rather than at each caller that needs the id to compare.
// [LAW:single-enforcer]
func epicID(epic *annotation.ParentEpicRef) string {
	if epic == nil {
		return ""
	}
	return epic.ID
}

// printContextLine renders one already-formatted indented context line — the
// shape behind context whose text a caller composed, such as the claim line.
// The empty string emits nothing, so callers pass the text rather than
// branching on whether they have any. [LAW:dataflow-not-control-flow]
func printContextLine(w io.Writer, indent, text string) error {
	if text == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "%s%s\n", indent, text)
	return err
}

// printIDListLine renders one indented "<label>: id, id, ..." context line —
// the shared shape behind both "blocked by:" and "unblocks:". An empty list
// emits nothing, so callers pass the list rather than branching on its length.
// [LAW:one-type-per-behavior] both lines are one behavior differing only in
// label and data. [LAW:dataflow-not-control-flow]
func printIDListLine(w io.Writer, indent, label string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "%s%s: %s\n", indent, label, strings.Join(ids, ", "))
	return err
}

func printIssueSummary(w io.Writer, issue model.Issue) error {
	_, err := fmt.Fprintln(w, singleLine(fmt.Sprintf("%s [%s/%s/%s/%s] %s%s", issue.ID, formatIssueState(issue), issue.IssueType, issue.Topic, issue.Priority, issue.Title, formatLabels(issue.Labels))))
	return err
}

// issueListRenderer is one way to print a listing: the issues a query
// selected, rendered from whatever else of the store its shape needs.
type issueListRenderer func(ctx context.Context, w io.Writer, st storage.Store, policy readyPolicy, issues []model.Issue) error

// listFormat is what `lit ls --format` names: a parser of the `--columns`
// expression that returns the renderer the format prints with. A format owns
// the flags its shape reads, so one that has no columns to project refuses a
// `--columns` rather than ignoring it. [LAW:parse-dont-validate]
type listFormat func(columnsExpr string) (issueListRenderer, error)

// listFormats is the whole `--format` vocabulary. The help text, the parser,
// and the rejection message all read it, so a format added here is advertised
// and accepted in one edit. [LAW:one-source-of-truth]
var listFormats = map[string]listFormat{
	"lines": columnar(printIssueLines),
	"table": columnar(printIssueTable),
	"yaml":  updateDocuments,
}

func sortedListFormatNames() []string {
	names := make([]string, 0, len(listFormats))
	for name := range listFormats {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseListFormat is the one checkpoint for the listing's output shape,
// `--format` and the `--columns` it reads, run before the query so a bad value
// costs no store read and no row is printed ahead of a refusal. It returns the
// renderer itself, so nothing downstream holds a format name or a column
// expression that could still be wrong, beside the format's name as the
// vocabulary spells it. A bad format is a model.ValidationError naming the
// valid formats: the same command can never succeed.
// [LAW:parse-dont-validate] [LAW:no-silent-failure]
func parseListFormat(formatExpr, columnsExpr string) (string, issueListRenderer, error) {
	name := strings.ToLower(strings.TrimSpace(formatExpr))
	format, ok := listFormats[name]
	if !ok {
		return "", nil, model.ValidationError{Message: fmt.Sprintf("unsupported --format %q (valid: %s)", name, strings.Join(sortedListFormatNames(), ", "))}
	}
	render, err := format(columnsExpr)
	return name, render, err
}

// columnar is a format that projects the selected columns, one row per issue,
// loading the derived cells the columns name.
func columnar(print func(w io.Writer, issues []model.Issue, columns []columnSpec, cells map[string]derivedColumns) error) listFormat {
	return func(columnsExpr string) (issueListRenderer, error) {
		columns, err := parseColumnSelection(columnsExpr)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, w io.Writer, st storage.Store, policy readyPolicy, issues []model.Issue) error {
			cells, err := listDerivedColumns(ctx, st, policy, columns, issues)
			if err != nil {
				return err
			}
			return print(w, issues, columns, cells)
		}, nil
	}
}

// updateDocuments is the format that prints each listed issue as the bulk
// update document stating its editable state, multi-document YAML in listing
// order: the file `lit import --path` reads back, so reading a selection out,
// editing it, and writing it back is one round trip that edits only what the
// file was edited to say. It projects no columns, so it refuses any.
func updateDocuments(columnsExpr string) (issueListRenderer, error) {
	if strings.TrimSpace(columnsExpr) != "" {
		return nil, UsageError{Message: "--columns does not apply to --format yaml, which prints every editable field of each issue"}
	}
	return func(ctx context.Context, w io.Writer, st storage.Store, _ readyPolicy, issues []model.Issue) error {
		relations, err := fetchIssueRelations(ctx, st, issues)
		if err != nil {
			return err
		}
		// Each document opens with its own `---` and closes its own encoder, so
		// a selection of none prints the empty stream rather than failing to
		// close a stream that never started.
		for _, issue := range issues {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
			enc := yaml.NewEncoder(w)
			enc.SetIndent(2)
			if err := enc.Encode(storage.BulkUpdateDocument(relations[issue.ID])); err != nil {
				return fmt.Errorf("encode the update document of %s: %w", issue.ID, err)
			}
			if err := enc.Close(); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []columnSpec, cells map[string]derivedColumns) error {
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(columnNames(columns), "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, columns, "\t", cells)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []columnSpec, cells map[string]derivedColumns) error {
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, columns, " | ", cells)); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	archivedAt, deletedAt := model.RetentionTimestamps(issue.Retention())
	if _, err := fmt.Fprintf(w, "%s\n%s\n\ntype: %s\ntopic: %s\npriority: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, singleLine(issue.Title), issue.IssueType, issue.Topic, issue.Priority, emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(archivedAt), formatOptionalTime(deletedAt)); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] Capability presence is the type-encoded
	// answer to leaf-vs-container; the printer dispatches once on that single
	// shape signal rather than asking IsContainer or comparing issue types.
	if caps := issue.Capabilities(); caps.Status != nil {
		if _, err := fmt.Fprintf(w, "status: %s\n", caps.Status.Value); err != nil {
			return err
		}
		// Only an in-progress ticket has a phase, so the line appears exactly
		// then, derived from the history the detail already carries.
		if phase, ok := model.PhaseOf(issue, detail.Events); ok {
			if _, err := fmt.Fprintf(w, "phase: %s\n", phase); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "assignee: %s\n", emptyDash(issue.AssigneeValue())); err != nil {
			return err
		}
		// Resolution is closed-only optional data; the line appears exactly when a
		// close recorded one (absent for open/in_progress and for a `done`/legacy
		// close). [LAW:dataflow-not-control-flow] presence of the value, not a mode.
		if caps.Status.Resolution != nil {
			if _, err := fmt.Fprintf(w, "resolution: %s\n", *caps.Status.Resolution); err != nil {
				return err
			}
		}
	} else {
		progress := issue.Progress()
		if _, err := fmt.Fprintf(w, "children: %d closed, %d in_progress, %d open (%d total)\n", progress.Closed, progress.InProgress, progress.Open, progress.Total); err != nil {
			return err
		}
	}
	// "unblocks:" surfaces the same leverage signal `lit backlog` shows inline:
	// IDs of open issues that depend on this one, i.e. would lose this as an
	// open dependency when it closes. Empty list = no leverage; line omitted.
	if ids := openUnblockIDs(detail.Blocks); len(ids) > 0 {
		if _, err := fmt.Fprintf(w, "unblocks: %s\n", strings.Join(ids, ", ")); err != nil {
			return err
		}
	}
	// [LAW:dataflow-not-control-flow] Parent block precedes the leaf description
	// so an agent reading top-to-bottom encounters containing context before
	// the specific leaf details. When the parent has a description, it inlines
	// indented under the parent line.
	if err := printIssueGroup(w, "parent", optionalGroup(detail.Parent)); err != nil {
		return err
	}
	if detail.Parent != nil && detail.Parent.Description != "" {
		if _, err := fmt.Fprintf(w, "%s\n", indentLines(detail.Parent.Description, "  ")); err != nil {
			return err
		}
	}
	if issue.Description != "" {
		if _, err := fmt.Fprintf(w, "\ndescription:\n%s\n", issue.Description); err != nil {
			return err
		}
	}
	if issue.Prompt != "" {
		if _, err := fmt.Fprintf(w, "\nprompt:\n%s\n", issue.Prompt); err != nil {
			return err
		}
	}
	if err := printCurrentPlan(w, detail); err != nil {
		return err
	}
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
		return err
	}
	// No "siblings" group here: when this issue has an epic parent,
	// writeEpicContext's "Epic: ... Children:" block already lists every
	// sibling (plus this ticket itself, marked "(you are here)") in rank
	// order — a strict superset of a bare siblings list. Printing both would
	// repeat the same ids twice for zero added information.
	// [LAW:one-source-of-truth]
	if err := printIssueGroup(w, "blocked_by", detail.BlockedBy); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocks", detail.Blocks); err != nil {
		return err
	}
	// redirect precedes related: it is the canonical "where did this work go"
	// edge, distinct from incidental peer links. The store already excluded it
	// from Related, so the two groups never overlap. [LAW:dataflow-not-control-flow]
	if err := printIssueGroup(w, "redirect", optionalGroup(detail.RedirectTarget)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	// The follow-up link, read from the row in both directions: the ticket
	// this one was filed as a follow-up of, then the tickets filed as
	// follow-ups of this one. Neither is a hierarchy edge, so neither appears
	// under parent or children. [LAW:one-source-of-truth]
	if err := printIssueGroup(w, "follows_up", optionalGroup(detail.FollowsUp)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "follow_ups", detail.FollowUps); err != nil {
		return err
	}
	if len(detail.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "\ncomments:"); err != nil {
			return err
		}
		// A row leads with the comment's id, the handle `lit comment rm` takes,
		// so the read view is the way to it. A body renders in its authored
		// lines, each after the first hung under its "- <id> [author]" row, so
		// only a comment row starts at column 0 and the list stays delimited
		// without escaping the body.
		for _, c := range detail.Comments {
			if _, err := fmt.Fprintf(w, "- %s [%s] %s\n", c.ID, c.CreatedBy, hangLines(c.Body, "  ")); err != nil {
				return err
			}
		}
	}
	// [LAW:one-source-of-truth] `lit show` renders only current state; the
	// field-level transition trail lives behind `lit history` (printIssueHistory)
	// so a cold reader never mistakes a superseded before→after line for a
	// current fact. The shared printHistoryEvents renderer keeps its home there.
	return nil
}

// issueFieldNames is the single definition of which field names `lit show
// --field` accepts and how each renders. It overlaps the --columns vocabulary
// (columns.go) without matching it: it also exposes the multi-line fields
// (description, prompt, plan) the field-limited view exists to serve, which a
// single-line table row cannot carry, and it does not name the relationship
// columns, which are not the issue's own fields. That the two vocabularies
// differ is why `--columns` must name the ones it rejects — `status` and
// `description` are field names a caller reasonably tries as columns.
// [LAW:one-source-of-truth]
var issueFieldNames = map[string]func(model.IssueDetail) string{
	"id":          func(d model.IssueDetail) string { return d.Issue.ID },
	"title":       func(d model.IssueDetail) string { return d.Issue.Title },
	"description": func(d model.IssueDetail) string { return d.Issue.Description },
	"prompt":      func(d model.IssueDetail) string { return d.Issue.Prompt },
	"plan":        func(d model.IssueDetail) string { return d.Issue.Plan },
	"type":        func(d model.IssueDetail) string { return string(d.Issue.IssueType) },
	"topic":       func(d model.IssueDetail) string { return d.Issue.Topic },
	"priority":    func(d model.IssueDetail) string { return d.Issue.Priority.String() },
	"status":      func(d model.IssueDetail) string { return string(d.Issue.State()) },
	"phase":       detailPhase,
	"assignee":    func(d model.IssueDetail) string { return d.Issue.AssigneeValue() },
	"labels":      func(d model.IssueDetail) string { return strings.Join(d.Issue.Labels, ",") },
	"rank":        func(d model.IssueDetail) string { return d.Issue.Rank },
	"lane":        func(d model.IssueDetail) string { return d.Issue.Lane },
	"follows_up":  func(d model.IssueDetail) string { return d.Issue.FollowsUp },
	"created_at":  func(d model.IssueDetail) string { return d.Issue.CreatedAt.Format(time.RFC3339) },
	"updated_at":  func(d model.IssueDetail) string { return d.Issue.UpdatedAt.Format(time.RFC3339) },
}

// detailPhase is the ticket's phase as its own history puts it, empty when
// the ticket has none: a bare --field value has no "-" placeholder, the same
// as an unassigned assignee.
func detailPhase(d model.IssueDetail) string {
	phase, _ := model.PhaseOf(d.Issue, d.Events)
	return string(phase)
}

// sortedIssueFieldNames lists the valid --field names for the unknown-field
// error message, so a typo is answered with the exact accepted vocabulary
// instead of a bare rejection.
func sortedIssueFieldNames() []string {
	names := make([]string, 0, len(issueFieldNames))
	for name := range issueFieldNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// printIssueFields renders exactly the requested fields and nothing else — no
// header, no parent/epic context, no siblings, no children summary — so an
// agent can read a ticket's description (or any other single field) without
// the full `lit show` dump. Fields are validated before anything prints, so
// an unknown name in a multi-field request fails clean rather than emitting
// a partial result. A single field prints its bare value with no label, so
// the output round-trips directly into `lit update --description` and
// similar; multiple fields print as "name: value" lines so a multi-line
// value stays attributable to its field, each value but a long text held to
// its row.
func printIssueFields(w io.Writer, detail model.IssueDetail, fields []string) error {
	type resolvedField struct {
		name  string
		value string
	}
	resolved := make([]resolvedField, 0, len(fields))
	for _, field := range fields {
		name := strings.ToLower(strings.TrimSpace(field))
		getValue, ok := issueFieldNames[name]
		if !ok {
			return UsageError{Message: fmt.Sprintf("unknown --field %q; valid fields: %s", field, strings.Join(sortedIssueFieldNames(), ", "))}
		}
		resolved = append(resolved, resolvedField{name: name, value: getValue(detail)})
	}
	if len(resolved) == 1 {
		_, err := fmt.Fprintln(w, resolved[0].value)
		return err
	}
	// A long text keeps its authored lines; every other value is one row, held
	// to it so a value holding a break cannot print a line that reads as
	// another field's.
	long := map[string]bool{}
	for _, field := range storage.LongTextFields() {
		long[field.Name] = true
	}
	for _, entry := range resolved {
		value := entry.value
		if !long[entry.name] {
			value = singleLine(value)
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", entry.name, value); err != nil {
			return err
		}
	}
	return nil
}

// printHistoryTrail renders the transition trail: one
// "- [actor @ time] action reason" line per event, followed by that event's
// indented change lines, with each plan review's line placed among the events
// at the time it was recorded. A short field's change reads "field: from →
// to"; a long text field's reads "field: vN → vM", the numbers its entry in
// versions gives each text, so the trail says that the text moved without
// reprinting every text it ever held. A review names the plan version it
// judged by the same numbers, and never prints the plan's text. This is the
// single definition of how the transition trail is formatted; the dedicated
// `lit history` command (printIssueHistory) is its only caller.
// [LAW:one-source-of-truth] one renderer, so the trail can never render two
// ways; [LAW:decomposition] carved at the event/detail joint, ready for any
// future surface that needs the same trail.
//
// Every value on a row goes through singleLine, so each event is one row and
// each change one row beneath it, whatever text the event carries.
func printHistoryTrail(w io.Writer, events []model.IssueEvent, reviews []model.PlanReview, versions map[string][]string) error {
	// One timed row per event and per review, sorted stably by time: on a tie
	// the event, written first, stays first.
	type row struct {
		at   time.Time
		text string
	}
	rows := make([]row, 0, len(events)+len(reviews))
	for _, event := range events {
		// Plain field updates carry no Action; "update" is their display label.
		// [LAW:dataflow-not-control-flow] absence of intent is data, not a mode.
		action := event.Action
		if action == "" {
			action = "update"
		}
		text := fmt.Sprintf("- [%s @ %s] %s %s\n", singleLine(event.Actor), formatHistoryTimestamp(event.CreatedAt), singleLine(action), singleLine(event.Reason))
		for _, change := range event.Changes {
			from, to := emptyDash(change.From), emptyDash(change.To)
			if texts, long := versions[change.Field]; long {
				from, to = versionLabel(texts, change.From), versionLabel(texts, change.To)
			}
			text += fmt.Sprintf("    %s: %s → %s\n", singleLine(change.Field), singleLine(from), singleLine(to))
		}
		rows = append(rows, row{at: event.CreatedAt, text: text})
	}
	plans := versions[storage.PlanField().Name]
	for _, review := range reviews {
		rows = append(rows, row{at: review.CreatedAt, text: fmt.Sprintf("- [%s @ %s] verdict %s on plan %s — laws: %s\n",
			singleLine(review.CreatedBy), formatHistoryTimestamp(review.CreatedAt), review.Verdict, planVersionLabel(plans, review.PlanDigest), joinLaws(review.Laws))})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return a.at.Compare(b.at) })
	for _, row := range rows {
		if _, err := io.WriteString(w, row.text); err != nil {
			return err
		}
	}
	return nil
}

// singleLine holds a value to the one row it is printed on: its printable
// lines joined by quotedBreak, the CLI's one rendering of a break inside a
// one-line value, each line's own quotedBreak defused last so the token means
// only a break. No part of the value can start a row of its own and read as
// another event or change. [LAW:one-source-of-truth]
func singleLine(s string) string {
	lines := textLines(s)
	for i, line := range lines {
		lines[i] = strings.ReplaceAll(line, quotedBreak, defusedBreak)
	}
	return strings.Join(lines, quotedBreak)
}

// printIssueHistory renders the standalone `lit history` view: an identifying
// header (id + title) so the output stands alone, then the field-level
// transition trail via the shared printHistoryEvents, then one line per long
// text field that once held a text it no longer holds, naming how many
// versions it has and the command that prints one. Every issue carries at
// least its "created" event, so the trail is never empty in practice; an
// empty Events slice honestly prints just the header rather than fabricating
// a line.
func printIssueHistory(w io.Writer, detail model.IssueDetail) error {
	id := singleLine(detail.Issue.ID)
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nhistory:\n", id, singleLine(detail.Issue.Title)); err != nil {
		return err
	}
	versions := map[string][]string{}
	var superseded []string
	for _, field := range storage.LongTextFields() {
		texts := storage.TextVersions(detail.Issue, detail.Events, field)
		versions[field.Name] = texts
		current := field.Current(detail.Issue)
		if slices.ContainsFunc(texts, func(text string) bool { return text != current }) {
			superseded = append(superseded, fmt.Sprintf("    %s: %s — lit history %s --version %s@<n> prints one\n", field.Name, storage.DescribeVersions(len(texts)), id, field.Name))
		}
	}
	if err := printHistoryTrail(w, detail.Events, detail.PlanReviews, versions); err != nil {
		return err
	}
	if len(superseded) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\nversions:\n%s", strings.Join(superseded, ""))
	return err
}

// printCurrentPlan prints the plan as it stands, under the version number lit
// history shows it by and the digest lit verdict names it by, and the reviews
// of that version — the verdicts that
// speak to the plan a reader is looking at. Reviews of other versions say
// nothing about this one, and live in lit history. No plan prints nothing.
func printCurrentPlan(w io.Writer, detail model.IssueDetail) error {
	plan := detail.Issue.Plan
	if plan == "" {
		return nil
	}
	digest := model.DigestPlan(plan)
	label := planVersionLabel(storage.TextVersions(detail.Issue, detail.Events, storage.PlanField()), digest)
	if _, err := fmt.Fprintf(w, "\nplan (%s, digest %s):\n%s\n", label, digest, plan); err != nil {
		return err
	}
	var current []model.PlanReview
	for _, review := range detail.PlanReviews {
		if review.Judges(plan) {
			current = append(current, review)
		}
	}
	if len(current) == 0 {
		_, err := fmt.Fprintf(w, "reviews of plan %s: none — lit verdict records one\n", label)
		return err
	}
	if _, err := fmt.Fprintf(w, "reviews of plan %s:\n", label); err != nil {
		return err
	}
	for _, review := range current {
		if _, err := fmt.Fprintf(w, "- %s [%s @ %s] laws: %s\n", review.Verdict, singleLine(review.CreatedBy), formatHistoryTimestamp(review.CreatedAt), joinLaws(review.Laws)); err != nil {
			return err
		}
	}
	return nil
}

// planVersionLabel is how a plan version reads wherever a review names it: v
// and the number texts, the plan's versions, gives the text whose digest it
// is.
func planVersionLabel(texts []string, digest model.PlanDigest) string {
	at := slices.IndexFunc(texts, func(text string) bool { return model.DigestPlan(text) == digest })
	if at < 0 {
		return "of a text its history does not hold (" + string(digest) + ")"
	}
	return "v" + strconv.Itoa(at+1)
}

// joinLaws is how a review's cited laws read: model.JoinLaws, or - for none.
func joinLaws(laws []model.Law) string {
	return emptyDash(model.JoinLaws(laws))
}

// versionLabel is how the trail shows one text of a long text field: v and
// its version number in texts, or - for the empty text.
func versionLabel(texts []string, text string) string {
	if text == "" {
		return "-"
	}
	return "v" + strconv.Itoa(slices.Index(texts, text)+1)
}

// versionRef names one recorded text of a long text field, written
// <field>@<n> — the number `lit history` shows it by.
type versionRef struct {
	field  storage.LongTextField
	number int
}

// parseVersionRef is the one place a --version value becomes a versionRef:
// the field must be a long text field and the number a version number in its
// plain decimal spelling, counted from 1. A version past the ones recorded is
// a lookup miss the record answers, not a malformed reference.
// [LAW:parse-dont-validate]
func parseVersionRef(raw string) (versionRef, error) {
	name, digits, _ := strings.Cut(raw, "@")
	fields := storage.LongTextFields()
	at := slices.IndexFunc(fields, func(field storage.LongTextField) bool { return field.Name == name })
	number, err := strconv.Atoi(digits)
	if at < 0 || err != nil || number < 1 || strconv.Itoa(number) != digits {
		names := make([]string, len(fields))
		for i, field := range fields {
			names[i] = field.Name
		}
		return versionRef{}, UsageError{Message: fmt.Sprintf("--version takes <field>@<n>: a long text field (%s) and a version number lit history shows, counted from 1; got %q", strings.Join(names, ", "), raw)}
	}
	return versionRef{field: fields[at], number: number}, nil
}

// printFieldVersion prints the text one version of a long text field held,
// bare, so it round-trips into `lit update --description` the way
// `lit show --field` output does.
func printFieldVersion(w io.Writer, detail model.IssueDetail, ref versionRef) error {
	texts := storage.TextVersions(detail.Issue, detail.Events, ref.field)
	if ref.number > len(texts) {
		return storage.VersionNotFoundError{IssueID: detail.Issue.ID, Field: ref.field.Name, Number: ref.number, Held: len(texts)}
	}
	_, err := fmt.Fprintln(w, texts[ref.number-1])
	return err
}

// optionalGroup adapts a single optional issue — a redirect target, a parent —
// to the slice printIssueGroup renders, so every such group reuses the one
// definition of the "- id [standing] title" line format and the omit-when-empty
// rule. A nil issue yields the empty slice, which printIssueGroup omits.
// [LAW:one-type-per-behavior] The redirect and the parent differ only in the
// label printIssueGroup is given; both are one optional issue rendered as a
// group, so one adapter serves them. A hand-rolled parent line would print no
// standing at all — a closed epic parent reading exactly like an open one.
func optionalGroup(issue *model.Issue) []model.Issue {
	if issue == nil {
		return nil
	}
	return []model.Issue{*issue}
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		// The standing marker is why the group is worth reading at all: the
		// agent learns where each referenced issue stands inline, without a
		// second 'lit show' per id.
		if _, err := fmt.Fprintln(w, singleLine(fmt.Sprintf("- %s [%s] %s", issue.ID, issueStanding(issue), issue.Title))); err != nil {
			return err
		}
	}
	return nil
}

// issueStanding is the one standing word for where an issue stands: the
// retention axis when it dominates, otherwise the status axis carrying the
// close's recorded reason.
//
// State() alone is shape-agnostic — leaves return their owned status, containers
// return state derived from children — but it is only half the truth: a deleted
// ticket's status is still "open", so rendering State() alone would print dead
// blockers as "[open]" and send readers hunting for an id that appears in no
// listing. A frozen issue's status describes work nobody may do, which is why
// the retention name replaces it rather than joining it.
//
// The close reason joins the status because "closed" alone is a directional
// lie: dropping it can only make a body of work look MORE finished than it is,
// never less, so a wontfix declination would read as completed work and an
// agent would act on the wrong picture. A `lit done` close records no reason
// and renders the bare word — the absence is the data, not a fifth member of
// the sealed set.
// [LAW:one-source-of-truth] Every surface that names a referenced issue's state
// reads this, so the epic plan's markers and the relationship groups cannot
// disagree about one ticket; Frozen and RetentionName stay the sole owners of
// "out of the flow" and of the words for it.
func issueStanding(issue model.Issue) string {
	if model.Frozen(issue.Retention()) {
		return model.RetentionName(issue.Retention())
	}
	return string(issue.State()) + resolutionSuffix(issue.ResolutionValue())
}

// resolutionSuffix renders a recorded close reason as the tail of a standing
// word — ":wontfix" — and "" for a close that recorded none, which is the
// identity for the concatenation above rather than a case its caller steers
// around. [LAW:dataflow-not-control-flow] absence is a value here, exactly as
// in formatEpicLine and laneTag.
//
// ":" and not "+": the "+" in formatIssueState means both lifecycle axes are
// true of one ticket ("open+deleted"), while a resolution is the closed state's
// own payload — it exists on no other state — so it refines the word rather
// than standing beside it. [LAW:comments-carry-meaning] the distinction is the
// notation's whole meaning and is invisible in the code.
func resolutionSuffix(resolution *model.Resolution) string {
	if resolution == nil {
		return ""
	}
	return ":" + string(*resolution)
}

// derivedColumns carries the per-issue facts that cannot be read off the issue
// row — one cell per column above sourceIssue. Each field has exactly one
// producer, and the two producers sit at different rungs of the ladder:
// parentID comes from the canonical graph (storage.IssueRelations) so the list
// view never reinterprets edge semantics, and blocked comes from
// ClassifyReadiness so it cannot carry a shorter list than the annotation
// registry. Nothing else may write either field. [LAW:one-source-of-truth]
//
// The zero value is the honest answer for an issue whose derived data was not
// loaded (no parent, not blocked), which is exactly what a nil map yields on
// lookup — and it is reachable only for a projection that named no derived
// column, because columnSourceFor makes the loader satisfy the maximum rung any
// selected column asks for.
type derivedColumns struct {
	parentID string
	blocked  bool
}

// The columns a projection can name — and the data each is computed from — are
// the column registry's to state; see columns.go.

func formatIssueColumns(issue model.Issue, columns []columnSpec, delimiter string, cells map[string]derivedColumns) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		// Reading a nil map yields the zero derivedColumns — "-" for an issue
		// whose derived data wasn't loaded — so no renderer needs a guard.
		values = append(values, column.render(issue, cells[issue.ID]))
	}
	return singleLine(strings.Join(values, delimiter))
}

// blockedLabel renders the blocked indicator as a self-describing token rather
// than a bare boolean, so the default headerless `lines` format stays legible
// (`id | blocked`) without relying on a column header.
func blockedLabel(blocked bool) string {
	if blocked {
		return "blocked"
	}
	return "-"
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func printLabels(w io.Writer, labels []string) error {
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

func formatHistoryTimestamp(value time.Time) string {
	return value.Local().Format(historyTimestampLayout)
}

// humanizeCoarseDuration renders a non-negative duration as a coarse,
// human-legible phrase — days/hours/minutes bucketed at 48h/2h/2m, so "how
// long ago" reads consistently across every surface that reports an age
// (sync-divergence age in SyncFailure.agePhrase, build age in runVersion).
// [LAW:single-enforcer] the one place this bucketing convention is defined;
// callers with their own "unknown"/zero handling wrap this rather than
// reimplementing the thresholds.
func humanizeCoarseDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	default:
		return "under a minute"
	}
}

// stalenessThresholdClause renders the parenthetical every staleness surface
// carries: how old a thing must be before that surface speaks up. "at least",
// never "over" — every staleness gate stays silent on `age < threshold`, so a
// value sitting exactly on the threshold warns, and "(over 7 days)" is false at
// that reachable age. The qualifier is bound to the threshold here instead of
// retyped per site.
// [LAW:single-enforcer] the one place the wording and the comparison are
// paired; TestEveryStalenessSurfaceAgreesAtItsBoundary holds every surface to
// calling it, at the one age where the wording can lie.
func stalenessThresholdClause(threshold time.Duration) string {
	return "at least " + humanizeCoarseDuration(threshold) + " old"
}

// openUnblockIDs returns the IDs of issues from blocks that are still live —
// the set this issue's closure would actually unblock from a "ready" perspective.
func openUnblockIDs(blocks []model.Issue) []string {
	ids := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if !b.InPlay() {
			continue
		}
		ids = append(ids, b.ID)
	}
	return ids
}

// liveIssues returns the live members of issues, preserving order. The full set
// stays intact upstream (lit show needs every sibling); callers that want only
// the actionable neighborhood — the close/done adjacency view — filter here.
func liveIssues(issues []model.Issue) []model.Issue {
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.InPlay() {
			out = append(out, issue)
		}
	}
	return out
}

// printCloseAdjacency renders a just-closed ticket's live neighborhood at the
// capture moment: its parent, the siblings still in play, related neighbors,
// and the dependents this close unblocked. Each group is omitted when empty, so
// closing an isolated ticket prints nothing. These are relationship FACTS, not a
// cue to act — the post-close guidance already carries the "why".
// [LAW:one-source-of-truth] Reuses lit show's group renderer and its
// now-unblocked-dependents derivation rather than minting a second
// representation of the same graph.
func printCloseAdjacency(w io.Writer, detail model.IssueDetail) error {
	if err := printIssueGroup(w, "parent", optionalGroup(detail.Parent)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "siblings", liveIssues(detail.Siblings)); err != nil {
		return err
	}
	// Surface the redirect at the close moment too: closing as duplicate/
	// superseded, the freshest fact is where the work went. Same store-shaped
	// IssueDetail, same omit-when-empty group as lit show.
	if err := printIssueGroup(w, "redirect", optionalGroup(detail.RedirectTarget)); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if ids := openUnblockIDs(detail.Blocks); len(ids) > 0 {
		if _, err := fmt.Fprintf(w, "\nunblocks: %s\n", strings.Join(ids, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueState(issue model.Issue) string {
	// State() is shape-agnostic: leaves return their owned status, containers
	// return the state derived from children. The close reason refines the
	// status word exactly as it does in issueStanding ("closed:wontfix"), for
	// the same directional reason: a bare "closed" reads a declination as
	// finished work. [LAW:one-source-of-truth] resolutionSuffix owns that
	// notation for both composers.
	parts := []string{string(issue.State()) + resolutionSuffix(issue.ResolutionValue())}
	// [LAW:types-are-the-program] Retention is a sum, so at most one tag applies.
	// [LAW:one-source-of-truth] Frozen owns the predicate and RetentionName the
	// word; this surface picks only the composition — it appends where
	// issueStanding replaces, because a ticket's own line carries both axes
	// ("closed:wontfix+archived") while a line naming another ticket carries
	// one standing.
	if model.Frozen(issue.Retention()) {
		parts = append(parts, model.RetentionName(issue.Retention()))
	}
	return strings.Join(parts, "+")
}

// indentLines prefixes every line of s with prefix, preserving internal line
// breaks. [LAW:one-source-of-truth] hangLines owns the per-line rule; a block
// indent is a hanging indent whose first line carries the prefix too.
func indentLines(s, prefix string) string {
	return prefix + hangLines(s, prefix)
}

// hangLines prefixes every line of s after the first with prefix: a hanging
// indent for text that continues a line the caller has already started.
// Trailing newlines are stripped so callers that append their own "\n"
// (e.g., via Fprintf) do not produce a stray prefix-only line at the end.
func hangLines(s, prefix string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+prefix)
}
