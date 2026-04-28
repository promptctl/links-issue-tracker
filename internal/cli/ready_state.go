package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

// readyBlockingKinds defines which annotation kinds block readiness.
// [LAW:one-source-of-truth] Single definition of what "blocks readiness" for the ready command.
var readyBlockingKinds = []annotation.Kind{
	annotation.MissingField,
	annotation.OpenDependency,
}

func isReadyBlocked(annotations []annotation.Annotation) bool {
	return annotation.HasAny(annotations, readyBlockingKinds...)
}

// newFieldAnnotator validates requiredFields against model.Issue JSON fields,
// then returns an annotator that checks those fields on each issue.
func newFieldAnnotator(requiredFields []string) (annotation.Annotator, error) {
	validFields := issueJSONFieldNames()
	for _, field := range requiredFields {
		if _, ok := validFields[field]; !ok {
			return nil, fmt.Errorf("required field %q does not exist on issue", field)
		}
	}
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		fields, err := issueFieldValues(issue)
		if err != nil {
			return nil, err
		}
		var annotations []annotation.Annotation
		for _, field := range requiredFields {
			if !isRequiredFieldSet(fields[field]) {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.MissingField,
					Message: field,
				})
			}
		}
		return annotations, nil
	}, nil
}

// fetchIssueDetails fetches IssueDetail for every listed issue into a map.
// [LAW:single-enforcer] One pre-pass is the single source of per-row detail
// data for the ready pipeline; both annotation and enrichment read from it.
// [LAW:dataflow-not-control-flow] The fetch is unconditional and happens
// once; downstream stages are pure map lookups over the result.
func fetchIssueDetails(ctx context.Context, st *store.Store, issues []model.Issue) (map[string]model.IssueDetail, error) {
	details := make(map[string]model.IssueDetail, len(issues))
	for _, issue := range issues {
		detail, err := st.GetIssueDetail(ctx, issue.ID)
		if err != nil {
			return nil, err
		}
		details[issue.ID] = detail
	}
	return details, nil
}

// newBlockerAnnotator returns an annotator that checks open dependency blockers
// and flags rank inversions where a dependency is ranked below the dependent.
// The annotator is pure: it reads from the shared details map rather than
// fetching from the store, so fetch cost is paid once upstream in
// fetchIssueDetails.
func newBlockerAnnotator(details map[string]model.IssueDetail) annotation.Annotator {
	// [LAW:dataflow-not-control-flow] Dependency lookup runs for every issue;
	// empty blockers list means no annotations, not a skipped operation.
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		detail := details[issue.ID]
		// Collect open blockers and sort by ID for stable annotation ordering.
		var openDeps []model.Issue
		for _, dep := range detail.DependsOn {
			if dep.State() != model.StateClosed {
				openDeps = append(openDeps, dep)
			}
		}
		sort.Slice(openDeps, func(i, j int) bool { return openDeps[i].ID < openDeps[j].ID })
		var annotations []annotation.Annotation
		for _, dep := range openDeps {
			annotations = append(annotations, annotation.Annotation{
				Kind:    annotation.OpenDependency,
				Message: dep.ID,
			})
			// Rank inversion: dependency should be ranked above (lower rank) the dependent.
			if dep.Rank > issue.Rank {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.RankInversion,
					Message: dep.ID,
				})
			}
		}
		return annotations, nil
	}
}

// newOrphanedAnnotator returns an annotator that flags in_progress issues
// with no update in the given threshold as orphaned.
func newOrphanedAnnotator(threshold time.Duration) annotation.Annotator {
	return func(_ context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		if issue.State() != model.StateInProgress {
			return nil, nil
		}
		age := time.Since(issue.UpdatedAt)
		if age < threshold {
			return nil, nil
		}
		return []annotation.Annotation{{
			Kind:    annotation.Orphaned,
			Message: fmt.Sprintf("in_progress for %s with no update", age.Truncate(time.Minute)),
		}}, nil
	}
}

func issueJSONFieldNames() map[string]struct{} {
	// [LAW:one-source-of-truth] model.Issue JSON tags are the canonical ready-field schema.
	issueType := reflect.TypeOf(model.Issue{})
	fields := make(map[string]struct{}, issueType.NumField())
	for i := 0; i < issueType.NumField(); i++ {
		field := issueType.Field(i)
		if !field.IsExported() {
			continue
		}
		name := issueJSONFieldName(field)
		if name == "" {
			continue
		}
		fields[name] = struct{}{}
	}
	fields["status"] = struct{}{}
	fields["assignee"] = struct{}{}
	fields["closed_at"] = struct{}{}
	return fields
}

func issueJSONFieldName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	switch name {
	case "":
		return field.Name
	case "-":
		return ""
	default:
		return name
	}
}

func issueFieldValues(issue model.Issue) (map[string]any, error) {
	payload, err := json.Marshal(issue)
	if err != nil {
		return nil, fmt.Errorf("marshal issue fields: %w", err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(payload, &values); err != nil {
		return nil, fmt.Errorf("unmarshal issue fields: %w", err)
	}
	return values, nil
}

func isRequiredFieldSet(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// enrichWithParentEpic populates ParentEpic on every row whose parent is
// type=epic. Rows with no parent or a non-epic parent get nil — the omitempty
// tag drops them from JSON output and the renderer skips them.
// [LAW:dataflow-not-control-flow] Every row flows through the same lookup;
// variability lives in whether the parent exists and its type, not in whether
// the enrichment step runs. (links-agent-epic-model-uew.2)
func enrichWithParentEpic(rows []annotation.AnnotatedIssue, details map[string]model.IssueDetail) {
	for i := range rows {
		detail := details[rows[i].ID]
		if detail.Parent == nil || !detail.Parent.IsContainer() {
			continue
		}
		rows[i].ParentEpic = &annotation.ParentEpicRef{
			ID:    detail.Parent.ID,
			Title: detail.Parent.Title,
		}
	}
}

// sortByCompositeRank orders rows by (effective_epic_rank, own_rank) so all
// leaves under a higher-ranked epic appear before any leaves under a
// lower-ranked epic — staying in one epic's context before moving to the
// next. A leaf with no parent, or a parent that is not an epic, uses its
// own rank as its epic-position, which interleaves it with epic groups at
// the correct position.
// [LAW:dataflow-not-control-flow] The sort key is a pure function of each
// row and the shared details map; variability lives in the values, not in
// whether some rows skip the sort. (links-agent-epic-model-uew.4)
func sortByCompositeRank(rows []annotation.AnnotatedIssue, details map[string]model.IssueDetail) {
	epicRank := func(issue model.Issue) string {
		parent := details[issue.ID].Parent
		if parent != nil && parent.IsContainer() {
			return parent.Rank
		}
		return issue.Rank
	}
	sort.SliceStable(rows, func(i, j int) bool {
		iEpic, jEpic := epicRank(rows[i].Issue), epicRank(rows[j].Issue)
		if iEpic != jEpic {
			return iEpic < jEpic
		}
		return rows[i].Rank < rows[j].Rank
	})
}

// sortByReadiness places issues without blocking annotations first,
// preserving the original store ordering within each group.
func sortByReadiness(issues []annotation.AnnotatedIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		iBlocked := isReadyBlocked(issues[i].Annotations)
		jBlocked := isReadyBlocked(issues[j].Annotations)
		return !iBlocked && jBlocked
	})
}

func applyLimit(issues []annotation.AnnotatedIssue, limit int) []annotation.AnnotatedIssue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
}

// sortByContinueBias is a stable sort that pulls leaves whose parent epic is
// currently in_progress to the front, preserving composite-rank order within
// each group. The bias absorbs "what is the agent's current focus" as a sort
// key over data, not as a branch in the consumer.
// [LAW:dataflow-not-control-flow] Same comparator runs over every pair; the
// parent-epic state decides ordering, not whether the comparator runs.
func sortByContinueBias(rows []annotation.AnnotatedIssue, details map[string]model.IssueDetail) {
	inProgressEpic := func(row annotation.AnnotatedIssue) bool {
		parent := details[row.ID].Parent
		return parent != nil && parent.IsContainer() && parent.State() == model.StateInProgress
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return inProgressEpic(rows[i]) && !inProgressEpic(rows[j])
	})
}

// pickFirstReady returns the first row in the ready partition: open status
// and no blocking annotations. The predicate is stated positively (StateOpen)
// rather than as "not in_progress and not blocked" so the implementation
// matches the docstring literally and stays correct if the upstream filter
// ever widens the set of statuses it lets through. The agent should not
// `lit start` an in-progress or blocked leaf — those need `lit done` or
// unblocking first.
func pickFirstReady(rows []annotation.AnnotatedIssue) (annotation.AnnotatedIssue, bool) {
	for _, row := range rows {
		if row.State() != model.StateOpen {
			continue
		}
		if isReadyBlocked(row.Annotations) {
			continue
		}
		return row, true
	}
	return annotation.AnnotatedIssue{}, false
}

// printNextSummary renders one workable leaf for `lit next` text output: the
// standard id+state+topic+title line, indented epic context if any, and inline
// dependency annotations so the agent knows what context to load before
// `lit start`.
func printNextSummary(w io.Writer, v any) error {
	row := v.(annotation.AnnotatedIssue)
	columns := resolveColumns(nil)
	line := formatIssueColumns(row.Issue, columns, "  ")
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	return printInlineDeps(w, row, nil)
}

// readyPreamble is printed before the ready list to give agents context about
// how to interpret and act on the backlog.
// [LAW:one-source-of-truth] Single definition of ready preamble text.
const readyPreamble = `This is the backlog. Pick the top item, but read every item so you understand the context.
Dependencies explain the WHY behind what you are building.
Design for the consumers who will use what you build. A poor foundation becomes
an immediate liability. Downstream tickets are your real acceptance criteria —
not just "does this work in isolation" but "does this set up the next layer for
success." Structure your implementation to make downstream tickets trivially easy,
even if the ticket doesn't specify it (but only if it aligns with the ticket).`

const readyMaxItems = 10

// buildUnblocksMap derives a reverse dependency index from OpenDependency annotations.
// For each dependency ID, it returns the IDs of open issues that depend on it.
// [LAW:dataflow-not-control-flow] The map is derived from existing annotation data;
// no extra store queries needed.
func buildUnblocksMap(issues []annotation.AnnotatedIssue) map[string][]string {
	m := make(map[string][]string)
	for _, issue := range issues {
		for _, a := range issue.Annotations {
			if a.Kind == annotation.OpenDependency {
				m[a.Message] = append(m[a.Message], issue.ID)
			}
		}
	}
	return m
}

// dependencyIDs extracts the IDs of open dependencies from an issue's annotations.
func dependencyIDs(annotations []annotation.Annotation) []string {
	var ids []string
	for _, a := range annotations {
		if a.Kind == annotation.OpenDependency {
			ids = append(ids, a.Message)
		}
	}
	return ids
}

// printReadyOutput partitions annotated issues into in-progress, ready, and blocked
// sections. Ready issues are shown with a preamble and inline dependency context,
// followed by in-progress work, then a count-by-reason summary for blocked issues.
func printReadyOutput(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	resolved := resolveColumns(columns)
	var inProgress, ready, blocked []annotation.AnnotatedIssue
	for i := range issues {
		switch {
		case issues[i].State() == model.StateInProgress:
			inProgress = append(inProgress, issues[i])
		case isReadyBlocked(issues[i].Annotations):
			blocked = append(blocked, issues[i])
		default:
			ready = append(ready, issues[i])
		}
	}

	unblocksMap := buildUnblocksMap(issues)

	if err := printReadySection(w, resolved, ready, unblocksMap); err != nil {
		return err
	}
	if err := printInProgressSection(w, resolved, inProgress); err != nil {
		return err
	}
	if err := printBlockedSummary(w, blocked); err != nil {
		return err
	}
	return printRankInversions(w, issues)
}

// printReadySection prints the preamble, separator, and numbered ready items
// with inline dependency info. Caps output at readyMaxItems. Coaching/cue
// output is part of the command's normal output and goes to stdout — stderr
// is for errors only.
func printReadySection(w io.Writer, columns []string, ready []annotation.AnnotatedIssue, unblocksMap map[string][]string) error {
	if _, err := fmt.Fprintln(w, readyPreamble); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, strings.Repeat("─", 80)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	display := ready
	if len(display) > readyMaxItems {
		display = display[:readyMaxItems]
	}

	if len(display) == 0 {
		_, err := fmt.Fprintln(w, "(none ready)")
		return err
	}

	// [LAW:dataflow-not-control-flow] Every ready issue flows through the same
	// numbered-line + dependency rendering path. Empty dependency slices produce
	// no output lines, not skipped operations.
	for i, entry := range display {
		line := fmt.Sprintf("%2d. %s", i+1, formatIssueColumns(entry.Issue, columns, "  "))
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if err := printInlineDeps(w, entry, unblocksMap); err != nil {
			return err
		}
	}

	if len(ready) > readyMaxItems {
		if _, err := fmt.Fprintf(w, "\n(%d more ready tickets not shown)\n", len(ready)-readyMaxItems); err != nil {
			return err
		}
	}
	return nil
}

// printInlineDeps prints "depends on:" and "unblocks:" lines indented under a ready item.
func printInlineDeps(w io.Writer, entry annotation.AnnotatedIssue, unblocksMap map[string][]string) error {
	const indent = "    "
	deps := dependencyIDs(entry.Annotations)
	unblocks := unblocksMap[entry.ID]

	if entry.ParentEpic != nil {
		if _, err := fmt.Fprintf(w, "%sepic: %s  %s\n", indent, entry.ParentEpic.ID, entry.ParentEpic.Title); err != nil {
			return err
		}
	}
	if len(deps) > 0 {
		if _, err := fmt.Fprintf(w, "%sdepends on: %s\n", indent, strings.Join(deps, ", ")); err != nil {
			return err
		}
	}
	if len(unblocks) > 0 {
		if _, err := fmt.Fprintf(w, "%sunblocks: %s\n", indent, strings.Join(unblocks, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func printInProgressSection(w io.Writer, columns []string, issues []annotation.AnnotatedIssue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nIn Progress"); err != nil {
		return err
	}
	for _, entry := range issues {
		line := formatIssueColumns(entry.Issue, columns, " | ")
		line += " | Last Update: " + inProgressSuffix(entry)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func inProgressSuffix(entry annotation.AnnotatedIssue) string {
	age := time.Since(entry.UpdatedAt).Truncate(time.Minute)
	suffix := fmt.Sprintf("%s", age)
	if annotation.HasAny(entry.Annotations, annotation.Orphaned) {
		suffix += " (ORPHANED)"
	}
	return suffix
}

// printBlockedSummary prints a compact count-by-reason summary of blocked issues.
func printBlockedSummary(w io.Writer, blocked []annotation.AnnotatedIssue) error {
	if len(blocked) == 0 {
		return nil
	}
	// Count issues per blocking reason kind. An issue with multiple blocking
	// kinds counts once per kind.
	counts := map[annotation.Kind]int{}
	for _, issue := range blocked {
		seen := map[annotation.Kind]bool{}
		for _, a := range issue.Annotations {
			if !annotation.HasAny([]annotation.Annotation{a}, readyBlockingKinds...) {
				continue
			}
			if seen[a.Kind] {
				continue
			}
			seen[a.Kind] = true
			counts[a.Kind]++
		}
	}
	if _, err := fmt.Fprintf(w, "\nBlocked (%d):\n", len(blocked)); err != nil {
		return err
	}
	// Print in a stable order based on readyBlockingKinds.
	for _, kind := range readyBlockingKinds {
		n, ok := counts[kind]
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %d: %s\n", n, kind.String()); err != nil {
			return err
		}
	}
	return nil
}

// printRankInversions prints a count-only warning when dependencies are ranked
// below the issues they block, with instructions to fix.
func printRankInversions(w io.Writer, issues []annotation.AnnotatedIssue) error {
	count := 0
	for _, issue := range issues {
		for _, a := range issue.Annotations {
			if a.Kind == annotation.RankInversion {
				count++
			}
		}
	}
	if count == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\nWarning: %d rank inversion(s) — dependencies ranked below their dependents. Run `lit doctor --fix` to repair. <agent-instructions>Run it yourself, do NOT ask the user — it is idempotent and safe.</agent-instructions>\n", count)
	return err
}
