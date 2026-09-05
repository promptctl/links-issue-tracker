package storage

import (
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// SortBindings is an engine's map from the vocabulary in [SortFields] to the
// comparison that reads each key. The set of keys is the contract's; what a key
// reads is the engine's, which is why the conformance suite checks one against
// the other instead of trusting two copies of a closed set to stay equal.
type SortBindings map[string]func(a, b model.Issue) int

// IssueOrdering folds a listing's sort specs into the single comparison
// [IssueReader.ListIssues] promises to order by: no specs is rank ascending,
// and id ascending is the last key always.
//
// Those two rules are the contract's own — normative, not something an engine
// discovers — so an engine implementing them privately is one rule keeping two
// homes, free to drift the moment either is edited alone. They live here for
// the same reason [SortFields] does. [LAW:one-source-of-truth] What stays with
// the engine is the [SortBindings] it passes: that is the part an engine can
// get wrong by itself, and the part the conformance suite exists to prove.
//
// An unsupported field is an error rather than an ordering nobody asked for,
// and what comes back is a comparator — a value that cannot exist until the
// specs have been checked. [LAW:parse-dont-validate]
func IssueOrdering(specs []SortSpec, bindings SortBindings) (func(a, b model.Issue) int, error) {
	if len(specs) == 0 {
		specs = []SortSpec{{Field: "rank"}}
	}
	compares := make([]func(a, b model.Issue) int, 0, len(specs)+1)
	for _, spec := range specs {
		field := strings.ToLower(strings.TrimSpace(spec.Field))
		compare, ok := bindings[field]
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
