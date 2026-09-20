package memory

import (
	"cmp"
	"context"
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
	order, err := storage.IssueOrdering(filter.SortBy, issueSortKeys)
	if err != nil {
		return nil, err
	}
	criteria, err := storage.ParseIssueCriteria(filter)
	if err != nil {
		return nil, err
	}
	for _, id := range filter.ParentIDs {
		if _, err := e.mustRecord(id); err != nil {
			return nil, err
		}
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
		if e.selects(issue, filter, criteria) {
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
//
// [LAW:one-source-of-truth] The criteria an issue answers for itself live in
// storage.IssueCriteria, which is what a caller narrowing rows it already holds
// applies. Re-deciding here what `--type bug` selects would put that answer in
// two places, and the second one only has to be wrong once.
//
// What remains is the half no issue can answer alone: membership under a
// parent, and whether anything has been said about it. Both are readings of the
// engine's own edge and comment tables.
func (e *Engine) selects(issue model.Issue, filter storage.ListIssuesFilter, criteria storage.IssueCriteria) bool {
	if !criteria.Selects(issue) {
		return false
	}
	if !e.matchesParents(issue.ID, filter.ParentIDs) {
		return false
	}
	if filter.HasComments != nil && *filter.HasComments != (len(e.commentsFor(issue.ID)) > 0) {
		return false
	}
	return true
}

// matchesParents reads membership off the parent-child edge alone, whatever the
// parent's retention: the edge is what makes an issue a child, so a deleted
// parent's children are still its children to a listing that names it. Empty
// criteria constrain nothing, as on every other axis.
func (e *Engine) matchesParents(childID string, parentIDs []string) bool {
	if len(parentIDs) == 0 {
		return true
	}
	for _, rel := range e.relations {
		if rel.Type == model.RelParentChild && rel.SrcID == childID && slices.Contains(parentIDs, rel.DstID) {
			return true
		}
	}
	return false
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

// issueSortKeys is this engine's reading of the contract's sort vocabulary; see
// storage.SortFields for what each key orders, which is why "status" compares
// derived state rather than a field.
var issueSortKeys = storage.SortBindings{
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
