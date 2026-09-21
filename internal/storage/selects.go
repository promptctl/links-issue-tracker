package storage

import (
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// IssueCriteria is a ListIssuesFilter reduced to the criteria that are
// properties of an issue itself, with the one step of selection that can fail
// — label canonicalization — already taken. [LAW:parse-dont-validate] the
// predicate below is therefore total: it answers yes or no about every issue
// and has no error to return.
//
// [LAW:decomposition] The joint is between what an issue IS and where it sits:
// status, type, assignee, labels, retention, text and timestamps are readable
// from the issue alone, while parentage and comment-presence are readable only
// from the graph around it. Everything on this side of the joint is shared, so
// a caller narrowing rows it already holds applies the SAME rule the store
// applies rather than a second, similar-looking one of its own.
// [LAW:single-enforcer]
type IssueCriteria struct {
	filter ListIssuesFilter
	labels []string
}

// ParseIssueCriteria canonicalizes the filter's label criteria. It rejects a
// label the vocabulary cannot express, which is the only way selection can
// fail. [LAW:no-silent-failure]
func ParseIssueCriteria(filter ListIssuesFilter) (IssueCriteria, error) {
	labels, err := model.CanonicalizeLabels(filter.LabelsAll)
	if err != nil {
		return IssueCriteria{}, err
	}
	return IssueCriteria{filter: filter, labels: labels}, nil
}

// Selects reports whether the issue satisfies every criterion the value
// carries: each criterion ANDs against the others, each slice ORs within
// itself. The zero value selects every live issue, so a caller asked to
// narrow on nothing runs the same path as one that was.
// [LAW:dataflow-not-control-flow]
//
// Live rather than everything, because retention is the one criterion whose
// empty value is not "no opinion": an unset IncludeArchived excludes archived
// issues rather than admitting them, so the zero value is already a filter.
// Every construction path runs through ParseIssueCriteria with a real filter,
// so nothing builds a raw zero value today -- but this comment is the contract
// that would tell someone it was safe to.
func (c IssueCriteria) Selects(issue model.Issue) bool {
	switch issue.Retention().(type) {
	case model.Archived:
		if !c.filter.IncludeArchived {
			return false
		}
	case model.Deleted:
		if !c.filter.IncludeDeleted {
			return false
		}
	}
	if !matchesStates(issue, c.filter.Statuses) {
		return false
	}
	if !matchesResolutions(issue, c.filter.Resolutions) {
		return false
	}
	if !matchesAny(string(issue.IssueType), issueTypeNames(c.filter.IssueTypes)) {
		return false
	}
	if len(c.filter.ExcludeIssueTypes) > 0 && matchesAny(string(issue.IssueType), issueTypeNames(c.filter.ExcludeIssueTypes)) {
		return false
	}
	if !matchesAny(issue.Assignee, TrimmedNonEmpty(c.filter.Assignees)) {
		return false
	}
	if !matchesAny(issue.ID, TrimmedNonEmpty(c.filter.IDs)) {
		return false
	}
	if c.filter.UpdatedAfter != nil && issue.UpdatedAt.Before(*c.filter.UpdatedAfter) {
		return false
	}
	if c.filter.UpdatedBefore != nil && issue.UpdatedAt.After(*c.filter.UpdatedBefore) {
		return false
	}
	for _, label := range c.labels {
		if !slices.Contains(issue.Labels, label) {
			return false
		}
	}
	for _, term := range c.filter.SearchTerms {
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

// TrimmedNonEmpty drops the blanks a caller may have assembled a criteria
// slice from, so a filter of nothing but whitespace constrains nothing rather
// than selecting nothing.
//
// Exported because both engines have to agree about it, and because a filter
// slice is assembled from CSV flags and query terms at several places that all
// meet the same question: does a whitespace-only filter select everything or
// nothing. A second copy of this rule is a second answer to that. The rule is
// that there is no second site, whoever the readers are — naming them here is
// what would go stale. [LAW:one-source-of-truth] [LAW:single-enforcer]
func TrimmedNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
