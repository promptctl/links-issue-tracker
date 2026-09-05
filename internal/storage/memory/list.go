package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ListIssues answers the listing surface's whole variability from one value.
//
// The pipeline is fixed — hydrate, select, order, cap — and every stage always
// runs; a filter that constrains nothing is an empty criterion, not a stage
// that is skipped, and "no limit" is a cap of everything.
// [LAW:dataflow-not-control-flow]
func (e *Engine) ListIssues(ctx context.Context, filter storage.ListIssuesFilter) ([]model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listIssues(filter)
}

func (e *Engine) listIssues(filter storage.ListIssuesFilter) ([]model.Issue, error) {
	order, err := issueOrdering(filter.SortBy)
	if err != nil {
		return nil, err
	}
	labelCriteria, err := canonicalLabels(filter.LabelsAll)
	if err != nil {
		return nil, err
	}
	pos := e.positions()
	ranked := make([]*record, 0, len(e.order))
	for _, id := range e.order {
		ranked = append(ranked, e.issues[id])
	}
	hydrated, err := e.hydrateAll(ranked, pos)
	if err != nil {
		return nil, err
	}
	selected := make([]model.Issue, 0, len(hydrated))
	for _, issue := range hydrated {
		if e.selects(issue, filter, labelCriteria) {
			selected = append(selected, issue)
		}
	}
	// The ordering is total — every comparison ends in a distinct id — so the
	// result does not depend on the order this slice arrived in, and sorting it
	// twice cannot produce two answers.
	slices.SortStableFunc(selected, order)
	return capLimit(selected, filter.Limit), nil
}

// selects is the whole selection rule: every criterion ANDs against the
// others, and every slice ORs within itself, so adding a criterion can only
// ever narrow a listing.
func (e *Engine) selects(issue model.Issue, filter storage.ListIssuesFilter, labelCriteria []string) bool {
	switch issue.Retention().(type) {
	case model.Archived:
		if !filter.IncludeArchived {
			return false
		}
	case model.Deleted:
		if !filter.IncludeDeleted {
			return false
		}
	}
	if !matchesStates(issue, filter.Statuses) {
		return false
	}
	if !matchesResolutions(issue, filter.Resolutions) {
		return false
	}
	if !matchesAny(string(issue.IssueType), issueTypeNames(filter.IssueTypes)) {
		return false
	}
	if len(filter.ExcludeIssueTypes) > 0 && matchesAny(string(issue.IssueType), issueTypeNames(filter.ExcludeIssueTypes)) {
		return false
	}
	if !matchesAny(issue.Assignee, trimmedNonEmpty(filter.Assignees)) {
		return false
	}
	if !matchesAny(issue.ID, trimmedNonEmpty(filter.IDs)) {
		return false
	}
	if filter.UpdatedAfter != nil && issue.UpdatedAt.Before(*filter.UpdatedAfter) {
		return false
	}
	if filter.UpdatedBefore != nil && issue.UpdatedAt.After(*filter.UpdatedBefore) {
		return false
	}
	if filter.HasComments != nil && *filter.HasComments != (len(e.commentsFor(issue.ID)) > 0) {
		return false
	}
	for _, label := range labelCriteria {
		if !slices.Contains(issue.Labels, label) {
			return false
		}
	}
	for _, term := range filter.SearchTerms {
		if !matchesSearch(issue, term) {
			return false
		}
	}
	return true
}

// matchesAny reports whether value is in criteria, treating an empty criteria
// list as "do not constrain on this axis" — the zero value of every filter
// slice, and why a listing needs no mode flags to say it wants everything.
func matchesAny(value string, criteria []string) bool {
	return len(criteria) == 0 || slices.Contains(criteria, value)
}

// matchesStates compares against the DERIVED state, never a stored one: a
// container's state is a reading of its children, so filtering on anything
// else would answer about an epic with a value nothing derives.
func matchesStates(issue model.Issue, wanted []model.State) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, state := range wanted {
		if model.DefaultOpen(string(state)) == issue.State() {
			return true
		}
	}
	return false
}

// matchesResolutions selects on the close outcome the lifecycle carries. An
// issue with no resolution — open, in progress, or closed as plain done —
// matches no non-empty criteria set.
func matchesResolutions(issue model.Issue, wanted []model.Resolution) bool {
	if len(wanted) == 0 {
		return true
	}
	resolution := issue.ResolutionValue()
	if resolution == nil {
		return false
	}
	return slices.Contains(wanted, *resolution)
}

// matchesSearch is the free-text criterion: one case-insensitive substring
// across the fields a searcher means by "the ticket said something about X",
// topic included.
func matchesSearch(issue model.Issue, term string) bool {
	needle := strings.ToLower(strings.TrimSpace(term))
	if needle == "" {
		return true
	}
	for _, haystack := range []string{issue.Title, issue.Description, issue.Prompt, issue.Topic} {
		if strings.Contains(strings.ToLower(haystack), needle) {
			return true
		}
	}
	return false
}

func issueTypeNames(types []model.IssueType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, string(t))
	}
	return out
}

// trimmedNonEmpty drops the blanks a caller may have assembled a criteria
// slice from, so a filter of nothing but whitespace constrains nothing rather
// than selecting nothing.
func trimmedNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// capLimit truncates the ordered result rather than sampling it, so a limited
// listing is always the head of the unlimited one. A limit of zero is the
// absence of a limit, not a limit of zero.
func capLimit(issues []model.Issue, limit int) []model.Issue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
}

// issueSortKeys is the closed set of fields a listing may be ordered by, each
// bound to the comparison that reads it. Membership here is what makes an
// unknown sort field an error at the boundary rather than an ordering nobody
// asked for. [LAW:parse-dont-validate] What each key orders is the contract's
// to say, not this engine's; see storage.SortFields, which is why "status"
// reads the derived state rather than a field.
var issueSortKeys = map[string]func(a, b model.Issue) int{
	"id":         func(a, b model.Issue) int { return strings.Compare(a.ID, b.ID) },
	"title":      func(a, b model.Issue) int { return strings.Compare(a.Title, b.Title) },
	"status":     func(a, b model.Issue) int { return strings.Compare(string(a.State()), string(b.State())) },
	"priority":   func(a, b model.Issue) int { return cmp.Compare(a.Priority, b.Priority) },
	"rank":       func(a, b model.Issue) int { return strings.Compare(a.Rank, b.Rank) },
	"type":       func(a, b model.Issue) int { return strings.Compare(string(a.IssueType), string(b.IssueType)) },
	"topic":      func(a, b model.Issue) int { return strings.Compare(a.Topic, b.Topic) },
	"assignee":   func(a, b model.Issue) int { return strings.Compare(a.Assignee, b.Assignee) },
	"created_at": func(a, b model.Issue) int { return a.CreatedAt.Compare(b.CreatedAt) },
	"updated_at": func(a, b model.Issue) int { return a.UpdatedAt.Compare(b.UpdatedAt) },
}

// issueOrdering composes the requested sort specs into one comparison.
//
// Two rules make the result a TOTAL order rather than a partial one, which is
// what lets this engine and the Dolt engine agree row for row. No specs is the
// canonical ordering, expressed as the spec list it stands for rather than as
// a branch that skips the sort [LAW:dataflow-not-control-flow]; and id
// ascending is appended as the final key always, so no tie is ever left for
// the engine's incidental slice order to settle
// [LAW:no-ambient-temporal-coupling]. Both are stated in the contract on
// storage.IssueReader.ListIssues.
func issueOrdering(specs []storage.SortSpec) (func(a, b model.Issue) int, error) {
	if len(specs) == 0 {
		specs = []storage.SortSpec{{Field: "rank"}}
	}
	compares := make([]func(a, b model.Issue) int, 0, len(specs)+1)
	for _, spec := range specs {
		field := strings.ToLower(strings.TrimSpace(spec.Field))
		compare, ok := issueSortKeys[field]
		if !ok {
			return nil, fmt.Errorf("unsupported sort field %q", spec.Field)
		}
		if spec.Desc {
			ascending := compare
			compare = func(a, b model.Issue) int { return -ascending(a, b) }
		}
		compares = append(compares, compare)
	}
	compares = append(compares, func(a, b model.Issue) int { return strings.Compare(a.ID, b.ID) })
	return func(a, b model.Issue) int {
		for _, compare := range compares {
			if result := compare(a, b); result != 0 {
				return result
			}
		}
		return 0
	}, nil
}
