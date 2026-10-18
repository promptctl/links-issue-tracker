package storage

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// sortKeys is the closed set of keys a listing may be ordered by, and the
// contract's statement of what each one orders, written as the comparison
// itself.
//
// Every key orders the issue's own recorded value except two. "status" orders
// the DERIVED lifecycle state — the same reading the status FILTER takes, so
// one listing means one thing by the word. [LAW:one-source-of-truth] A
// container holds no stored status; its state is a reading of its children, so
// ordering on a column would file every container at an end of the listing
// whatever state it derives to. "rank" orders by tree order
// ([RankAncestry.Compare]), because an issue's own key is comparable only
// within its frame. Text compares byte-wise and a time by the instant it
// denotes — never by a collation, nor by the spelling an engine stores.
//
// The table belongs to the contract rather than to each engine because an
// engine has nothing left to decide by the time it runs: every comparison
// reads a hydrated [model.Issue] or the ancestry, and the engine has built
// both already. A copy per engine would be one text twice, and a defect copied
// into both passes the differential check on both, so the copies would buy
// drift and no proof. What each key reads is pinned instead by the conformance
// suite, against literal orders. internal/storage/memory's package doc states
// where this falls from the engines' side.
func sortKeys(ancestry RankAncestry) map[string]func(a, b model.Issue) int {
	return map[string]func(a, b model.Issue) int{
		"id":         func(a, b model.Issue) int { return strings.Compare(a.ID, b.ID) },
		"title":      func(a, b model.Issue) int { return strings.Compare(a.Title, b.Title) },
		"status":     func(a, b model.Issue) int { return strings.Compare(string(a.State()), string(b.State())) },
		"priority":   func(a, b model.Issue) int { return cmp.Compare(a.Priority, b.Priority) },
		"rank":       ancestry.Compare,
		"type":       func(a, b model.Issue) int { return strings.Compare(string(a.IssueType), string(b.IssueType)) },
		"topic":      func(a, b model.Issue) int { return strings.Compare(a.Topic, b.Topic) },
		"assignee":   func(a, b model.Issue) int { return strings.Compare(a.Assignee, b.Assignee) },
		"created_at": func(a, b model.Issue) int { return a.CreatedAt.Compare(b.CreatedAt) },
		"updated_at": func(a, b model.Issue) int { return a.UpdatedAt.Compare(b.UpdatedAt) },
	}
}

// IssueOrdering folds a listing's sort specs into the single comparison
// [IssueReader.ListIssues] promises to order by: each spec reads its key from
// [sortKeys], no specs is rank ascending, and id ascending is the last key
// always. The ancestry is the one input an engine supplies, because tree order
// reads the keys of containers the listing may have filtered out.
//
// Those rules are the contract's own — normative, not something an engine
// discovers — so an engine implementing them privately is one rule keeping two
// homes, free to drift the moment either is edited alone.
// [LAW:one-source-of-truth]
//
// An unsupported field is an error rather than an ordering nobody asked for,
// and what comes back is a comparator — a value that cannot exist until the
// specs have been checked. [LAW:parse-dont-validate]
func IssueOrdering(specs []SortSpec, ancestry RankAncestry) (func(a, b model.Issue) int, error) {
	if len(specs) == 0 {
		specs = []SortSpec{{Field: "rank"}}
	}
	keys := sortKeys(ancestry)
	compares := make([]func(a, b model.Issue) int, 0, len(specs)+1)
	for _, spec := range specs {
		field := strings.ToLower(strings.TrimSpace(spec.Field))
		compare, ok := keys[field]
		if !ok {
			return nil, model.ValidationError{Message: fmt.Sprintf("unsupported sort field %q", spec.Field)}
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

// EventOrdering is the single comparison [IssueReader.ListAllEvents] and
// [IssueReader.ListEvents] promise to order history by: oldest first by the
// INSTANT a stamp denotes, ties broken by event id ascending.
//
// It lives here for the same reason [IssueOrdering] does — the rule is the
// contract's own, so an engine implementing it privately is one rule keeping
// two homes, and two homes drift silently: an engine that orders a varchar
// created_at in SQL sorts a timestamp by its SPELLING. RFC3339Nano trims
// trailing zeros, so the earlier of two instants can render as the shorter
// string and sort after the later one — the engines then answer the same
// question differently, which the differential oracle reads as divergence
// rather than as the defect it is. [LAW:one-source-of-truth]
//
// Comparing instants is what the rule means; the encoding an engine happens to
// store is not the contract's business, and no engine gets to substitute it.
func EventOrdering(a, b model.IssueEvent) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
}

// PlanReviewOrdering is the order [IssueReader.GetIssueDetail] and an export
// carry plan reviews in: EventOrdering's rule, for the same reason — oldest
// first by the instant, ties by id.
func PlanReviewOrdering(a, b model.PlanReview) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
}
