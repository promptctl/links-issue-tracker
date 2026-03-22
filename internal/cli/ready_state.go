package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bmf/links-issue-tracker/internal/annotation"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

// readyBlockingKinds defines which annotation kinds block readiness.
// [LAW:one-source-of-truth] Single definition of what "blocks readiness" for the ready command.
var readyBlockingKinds = []annotation.Kind{
	annotation.MissingField,
	annotation.BlockedBy,
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

// newBlockerAnnotator returns an annotator that checks open dependency blockers
// and flags priority inversions where a blocker has worse priority than the dependent.
func newBlockerAnnotator(st *store.Store) annotation.Annotator {
	// [LAW:dataflow-not-control-flow] Dependency lookup runs for every issue;
	// empty blockers list means no annotations, not a skipped operation.
	return func(ctx context.Context, issue model.Issue) ([]annotation.Annotation, error) {
		detail, err := st.GetIssueDetail(ctx, issue.ID)
		if err != nil {
			return nil, err
		}
		// Collect open blockers and sort by ID for stable annotation ordering.
		var openDeps []model.Issue
		for _, dep := range detail.DependsOn {
			if dep.Status != "closed" {
				openDeps = append(openDeps, dep)
			}
		}
		sort.Slice(openDeps, func(i, j int) bool { return openDeps[i].ID < openDeps[j].ID })
		var annotations []annotation.Annotation
		for _, dep := range openDeps {
			annotations = append(annotations, annotation.Annotation{
				Kind:    annotation.BlockedBy,
				Message: dep.ID,
			})
			if dep.Priority > issue.Priority {
				annotations = append(annotations, annotation.Annotation{
					Kind:    annotation.PriorityInversion,
					Message: fmt.Sprintf("%s (priority %d)", dep.ID, dep.Priority),
				})
			}
		}
		return annotations, nil
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

// readyBlockReason formats blocking annotations as a human-readable reason string.
// Only includes annotations whose Kind is in readyBlockingKinds.
func readyBlockReason(annotations []annotation.Annotation) string {
	reasons := make([]string, 0, len(annotations))
	for _, a := range annotations {
		if !annotation.HasAny([]annotation.Annotation{a}, readyBlockingKinds...) {
			continue
		}
		switch a.Kind {
		case annotation.MissingField:
			reasons = append(reasons, fmt.Sprintf("Field %s not set", a.Message))
		case annotation.BlockedBy:
			reasons = append(reasons, fmt.Sprintf("Blocked by ticket %s", a.Message))
		}
	}
	return strings.Join(reasons, "; ")
}

// printReadyOutput prints annotated issues in Ready / Not Ready sections.
// The partition is derived from the isReadyBlocked predicate at the render boundary.
func printReadyOutput(w io.Writer, format string, columns []string, issues []annotation.AnnotatedIssue) error {
	resolved := resolveColumns(columns)
	var ready, blocked []annotation.AnnotatedIssue
	for i := range issues {
		if isReadyBlocked(issues[i].Annotations) {
			blocked = append(blocked, issues[i])
		} else {
			ready = append(ready, issues[i])
		}
	}

	if _, err := fmt.Fprintln(w, "Ready"); err != nil {
		return err
	}
	if err := printAnnotatedRows(w, format, resolved, ready); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Not Ready"); err != nil {
		return err
	}
	if err := printAnnotatedRows(w, format, resolved, blocked); err != nil {
		return err
	}
	return printPriorityInversions(w, issues)
}

func printAnnotatedRows(w io.Writer, format string, columns []string, issues []annotation.AnnotatedIssue) error {
	if len(issues) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	hasReasons := false
	for _, issue := range issues {
		if readyBlockReason(issue.Annotations) != "" {
			hasReasons = true
			break
		}
	}
	switch format {
	case "", "lines":
		return printAnnotatedLines(w, issues, columns, hasReasons)
	case "table":
		return printAnnotatedTable(w, issues, columns, hasReasons)
	default:
		return fmt.Errorf("unsupported --format %q", format)
	}
}

func printAnnotatedLines(w io.Writer, issues []annotation.AnnotatedIssue, columns []string, hasReasons bool) error {
	for _, entry := range issues {
		line := formatIssueColumns(entry.Issue, columns, " | ")
		if hasReasons {
			if reason := readyBlockReason(entry.Annotations); reason != "" {
				line += " | " + reason
			}
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func printAnnotatedTable(w io.Writer, issues []annotation.AnnotatedIssue, columns []string, hasReasons bool) error {
	headers := append([]string{}, columns...)
	if hasReasons {
		headers = append(headers, "reason")
	}
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(headers, "\t"))); err != nil {
		return err
	}
	for _, entry := range issues {
		line := formatIssueColumns(entry.Issue, columns, "\t")
		if hasReasons {
			if reason := readyBlockReason(entry.Annotations); reason != "" {
				line += "\t" + reason
			}
		}
		if _, err := fmt.Fprintln(tw, line); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// printPriorityInversions prints a summary of priority inversions found across all issues.
func printPriorityInversions(w io.Writer, issues []annotation.AnnotatedIssue) error {
	type inversion struct {
		issueID   string
		issuePri  int
		blockerID string
	}
	var inversions []inversion
	for _, issue := range issues {
		for _, a := range issue.Annotations {
			if a.Kind != annotation.PriorityInversion {
				continue
			}
			inversions = append(inversions, inversion{
				issueID:   issue.ID,
				issuePri:  issue.Priority,
				blockerID: a.Message,
			})
		}
	}
	if len(inversions) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Priority inversions (%d):\n", len(inversions)); err != nil {
		return err
	}
	for _, inv := range inversions {
		if _, err := fmt.Fprintf(w, "  %s (P%d) blocked by %s — blocker is lower priority\n", inv.issueID, inv.issuePri, inv.blockerID); err != nil {
			return err
		}
	}
	return nil
}

