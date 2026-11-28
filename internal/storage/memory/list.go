package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/selector"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ListIssues answers a listing from one value. The pipeline is fixed —
// hydrate, select, order, cap — and every stage always runs: an empty
// selector selects everything, and "no limit" is a cap of everything.
// [LAW:dataflow-not-control-flow]
func (e *Engine) ListIssues(ctx context.Context, listing storage.Listing) ([]model.Issue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listIssues(listing)
}

// SelectIssueIDs answers a selection alone, id ascending.
func (e *Engine) SelectIssueIDs(ctx context.Context, selection storage.Selection) ([]model.ID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	selected, err := e.selected(selection)
	if err != nil {
		return nil, err
	}
	ids := make([]model.ID, 0, len(selected))
	for _, issue := range selected {
		ids = append(ids, issue.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

func (e *Engine) listIssues(listing storage.Listing) ([]model.Issue, error) {
	pos := e.positions()
	order, err := storage.IssueOrdering(listing.Sort, e.rankAncestry(pos))
	if err != nil {
		return nil, err
	}
	selected, err := e.selected(listing.Selection)
	if err != nil {
		return nil, err
	}
	// The ordering is total — every comparison ends in a distinct id — so the
	// result does not depend on the order this slice arrived in, and sorting it
	// twice cannot produce two answers.
	slices.SortStableFunc(selected, order)
	return capLimit(selected, listing.Limit), nil
}

// selected is every issue the selection selects, hydrated.
func (e *Engine) selected(selection storage.Selection) ([]model.Issue, error) {
	r, err := e.reading(selection)
	if err != nil {
		return nil, err
	}
	out := make([]model.Issue, 0, len(r.universe))
	for _, issue := range r.universe {
		if r.holds(selection.Selector.Atoms(), issue) {
			out = append(out, issue)
		}
	}
	return out, nil
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

// rankAncestry reads every framing edge in one pass, by the rule the frame
// lookup applies (frames), keyed as rankAt renders it. Every edge goes in, so
// a child two edges claim is placed and reported exactly as in the SQL engine.
// [LAW:one-source-of-truth]
func (e *Engine) rankAncestry(pos map[model.ID]int) storage.RankAncestry {
	var links []storage.ParentLink
	for _, rel := range e.relations {
		if e.frames(rel) {
			links = append(links, storage.ParentLink{ChildID: rel.SrcID, ParentID: rel.DstID, ParentRank: rankAt(pos[rel.DstID])})
		}
	}
	return storage.NewRankAncestry(links)
}

// reading is the engine as one selection reads it: the issues that exist for
// it, each hydrated, so an atom reads the same derived facts — an epic's
// state, a closed issue's resolution — the listing hands back.
//
// It is the plain reading of a selector the conformance suite holds the SQL
// compiler to, so it is written to be obviously right rather than fast: each
// path is the set of values it reads, computed whole, and each atom a test of
// that set. [LAW:one-source-of-truth]
type reading struct {
	e        *Engine
	universe []model.Issue
	byID     map[model.ID]model.Issue
}

func (e *Engine) reading(selection storage.Selection) (reading, error) {
	pos := e.positions()
	ranked := make([]*record, 0, len(e.order))
	for _, id := range e.order {
		ranked = append(ranked, e.issues[id])
	}
	hydrated, err := e.hydrateAll(ranked, pos)
	if err != nil {
		return reading{}, err
	}
	r := reading{e: e, byID: map[model.ID]model.Issue{}}
	for _, issue := range hydrated {
		if selection.Admits(issue.Retention()) {
			r.universe = append(r.universe, issue)
			r.byID[issue.ID] = issue
		}
	}
	return r, nil
}

// holds is whether every atom holds for issue.
func (r reading) holds(atoms []selector.Atom, issue model.Issue) bool {
	for _, atom := range atoms {
		if !r.atom(atom, issue) {
			return false
		}
	}
	return true
}

func (r reading) atom(atom selector.Atom, issue model.Issue) bool {
	values := r.values(atom.Path(), issue)
	switch test := atom.Test().(type) {
	case selector.Count:
		return test.Holds(len(values))
	case selector.Presence:
		return (len(values) > 0) == (test.Op == selector.Present)
	case selector.Match:
		return slices.ContainsFunc(values, func(v string) bool { return matches(v, test) }) == (test.Op != selector.Ne)
	default:
		panic(fmt.Sprintf("memory: a selector test of type %T has no reading", test))
	}
}

// matches is whether value v satisfies test, != read as = (atom negates it).
func matches(v string, test selector.Match) bool {
	switch test.Op {
	case selector.Eq, selector.Ne:
		return slices.Contains(test.Values, v)
	case selector.Contains:
		return slices.ContainsFunc(test.Values, func(fragment string) bool { return strings.Contains(strings.ToLower(v), fragment) })
	default:
		return selector.Ordered(test.Op, v, test.Values[0])
	}
}

// values is the set of values path reads from issue: each step reads its field
// from every issue the step before it reached, and the path reads the union.
func (r reading) values(path selector.Path, issue model.Issue) []string {
	rows := []model.Issue{issue}
	if path.Anchored() {
		rows = nil
	}
	var values []string
	for _, step := range path.Steps() {
		f := step.Field()
		if f.Of() == nil {
			values = nil
			for _, row := range rows {
				values = append(values, r.fieldValues(f, row)...)
			}
			continue
		}
		var next []model.Issue
		reached := map[model.ID]bool{}
		for _, row := range r.rowsFrom(f, rows) {
			if !reached[row.ID] && r.holds(step.Where().Atoms(), row) {
				reached[row.ID] = true
				next = append(next, row)
			}
		}
		rows, values = next, nil
		for _, row := range rows {
			values = append(values, string(row.ID))
		}
	}
	values = slices.DeleteFunc(values, func(v string) bool { return v == "" })
	slices.Sort(values)
	return slices.Compact(values)
}

// rowsFrom is every issue of the universe f holds for each of from; the
// workspace's issues are the universe itself.
func (r reading) rowsFrom(f *selector.Field, from []model.Issue) []model.Issue {
	if f == storage.WorkspaceIssues {
		return r.universe
	}
	var out []model.Issue
	for _, issue := range from {
		for _, id := range r.related(f, issue) {
			if row, ok := r.byID[id]; ok {
				out = append(out, row)
			}
		}
	}
	return out
}

// related is the ids field f names from issue, whether or not they exist for
// the read; rowsFrom keeps the ones that do.
func (r reading) related(f *selector.Field, issue model.Issue) []model.ID {
	edges := func(kind model.RelationType, from, to func(model.Relation) model.ID) []model.ID {
		var out []model.ID
		for _, rel := range r.e.relations {
			if rel.Type == kind && from(rel) == issue.ID {
				out = append(out, to(rel))
			}
		}
		return out
	}
	src := func(rel model.Relation) model.ID { return rel.SrcID }
	dst := func(rel model.Relation) model.ID { return rel.DstID }
	switch f {
	case storage.IssueParent.Field:
		return edges(model.RelParentChild, src, dst)
	case storage.IssueChildren:
		return edges(model.RelParentChild, dst, src)
	case storage.IssueSiblings:
		var out []model.ID
		for _, parent := range r.rowsFrom(storage.IssueParent.Field, []model.Issue{issue}) {
			for _, sibling := range r.related(storage.IssueChildren, parent) {
				if sibling != issue.ID {
					out = append(out, sibling)
				}
			}
		}
		return out
	case storage.IssueBlockedBy:
		return edges(model.RelBlockedBy, src, dst)
	case storage.IssueBlocks:
		return edges(model.RelBlockedBy, dst, src)
	case storage.IssueRelated:
		return append(edges(model.RelRelatedTo, src, dst), edges(model.RelRelatedTo, dst, src)...)
	case storage.IssueFollowsUp:
		return []model.ID{issue.FollowsUp}
	case storage.IssueFollowUps:
		var out []model.ID
		for _, other := range r.universe {
			if other.FollowsUp == issue.ID {
				out = append(out, other.ID)
			}
		}
		return out
	case storage.IssueRedirectTarget:
		if target := issue.RedirectTargetValue(); target != nil {
			return []model.ID{model.ID(*target)}
		}
		return nil
	default:
		panic(fmt.Sprintf("memory: field %s has no reading", f.Name()))
	}
}

// fieldValues is the values field f holds of issue, a text field's empty text
// being none.
func (r reading) fieldValues(f *selector.Field, issue model.Issue) []string {
	switch f {
	case storage.IssueID.Field:
		return []string{string(issue.ID)}
	case storage.IssueTitle:
		return []string{issue.Title.Raw()}
	case storage.IssueTopic:
		return []string{issue.Topic.Raw()}
	case storage.IssueType.Field:
		return []string{string(issue.IssueType)}
	case storage.IssuePriority:
		return []string{issue.Priority.String()}
	case storage.IssueAssignee:
		return []string{issue.Assignee.Raw()}
	case storage.IssueLane:
		return []string{issue.Lane.Raw()}
	case storage.IssueLabels.Field:
		return model.RawTexts(issue.Labels)
	case storage.IssueStatus.Field:
		return []string{string(issue.State())}
	case storage.IssueResolution:
		if resolution := issue.ResolutionValue(); resolution != nil {
			return []string{string(*resolution)}
		}
		return nil
	case storage.IssueCreatedAt:
		return []string{model.StampText(issue.CreatedAt)}
	case storage.IssueUpdatedAt:
		return []string{model.StampText(issue.UpdatedAt)}
	case storage.IssueClosedAt:
		if closed := issue.ClosedAtValue(); closed != nil {
			return []string{model.StampText(*closed)}
		}
		return nil
	case storage.IssueDescription:
		return []string{issue.Description}
	case storage.IssuePrompt:
		return []string{issue.Prompt}
	case storage.IssuePlan:
		return []string{issue.Plan}
	case storage.IssueText:
		return []string{issue.Title.Raw(), issue.Description, issue.Prompt, issue.Plan, issue.Topic.Raw()}
	case storage.IssuePhase:
		if phase, ok := model.PhaseOf(issue, r.e.eventsFor(issue.ID)); ok {
			return []string{string(phase)}
		}
		return nil
	case storage.IssueComments:
		var ids []string
		for _, comment := range r.e.commentsFor(issue.ID) {
			ids = append(ids, string(comment.ID))
		}
		return ids
	case storage.IssuePlanReviews:
		var ids []string
		for _, review := range r.e.planReviews {
			if review.IssueID == issue.ID {
				ids = append(ids, string(review.ID))
			}
		}
		return ids
	default:
		panic(fmt.Sprintf("memory: field %s has no reading", f.Name()))
	}
}
