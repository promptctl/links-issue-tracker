package store

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/rank"
)

// rankedIssue is one row of the sequence the repair permutes: an issue id and
// the rank string that currently places it.
type rankedIssue struct {
	id   string
	rank string
}

// rankRewrite names one issue whose rank must change, and the value to write.
type rankRewrite struct {
	id      string
	newRank string
}

// blocksCycleError reports a precedence constraint set that no rank order can
// satisfy: placing every dependency above its dependent would require placing
// each member of the cycle above itself.
type blocksCycleError struct{ path []string }

func (e *blocksCycleError) Error() string {
	return fmt.Sprintf("blocks dependency cycle %s — a cycle has no valid rank order; break it by removing one edge with 'lit dep rm'", strings.Join(e.path, " -> "))
}

// repairRankOrder returns the rank writes that make every dependency outrank
// its dependent while moving as few issues as possible.
//
// [LAW:effects-at-boundaries] The new order and the writes that realize it are
// values computed here; the caller owns the transaction that applies them, so
// the whole repair is testable with no database.
func repairRankOrder(order []rankedIssue, edges []blocksEdge) ([]rankRewrite, error) {
	target, err := stableTopoOrder(order, edges)
	if err != nil {
		return nil, err
	}
	return rankRewrites(order, target)
}

// projectEdges keeps the blocks edges that constrain the given order, deduped.
// An edge with an endpoint outside the order — closed, archived, or deleted
// work — says nothing about where live work sits, and one edge counted twice
// would leave its dependent blocked by a dependency that is only placed once.
// [LAW:parse-dont-validate] Both facts are settled here, so everything
// downstream indexes positions directly: every edge it sees is in range and
// counted exactly once.
func projectEdges(edges []blocksEdge, position map[string]int) []blocksEdge {
	seen := make(map[blocksEdge]struct{}, len(edges))
	out := make([]blocksEdge, 0, len(edges))
	for _, e := range edges {
		_, depInOrder := position[e.dependency]
		_, dependentInOrder := position[e.dependent]
		if !depInOrder || !dependentInOrder {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// stableTopoOrder returns the ids of order rearranged so that every dependency
// precedes its dependent, choosing at each step the issue that stood earliest
// in order among those whose dependencies are all placed.
//
// So an issue falls behind one that stood after it only while it waits on a
// dependency, and an order that already satisfies every edge comes back
// unchanged. Positions are compared, never ids.
func stableTopoOrder(order []rankedIssue, edges []blocksEdge) ([]string, error) {
	position := make(map[string]int, len(order))
	for at, item := range order {
		position[item.id] = at
	}
	constraints := projectEdges(edges, position)
	dependents := make([][]int, len(order))
	blockedBy := make([]int, len(order))
	for _, e := range constraints {
		dep, dependent := position[e.dependency], position[e.dependent]
		dependents[dep] = append(dependents[dep], dependent)
		blockedBy[dependent]++
	}

	// `ready` holds the positions whose dependencies are all placed, kept
	// ascending so its head is always the earliest-standing candidate.
	ready := make([]int, 0, len(order))
	for at, count := range blockedBy {
		if count == 0 {
			ready = append(ready, at)
		}
	}
	sorted := make([]string, 0, len(order))
	for len(ready) > 0 {
		at := ready[0]
		ready = ready[1:]
		sorted = append(sorted, order[at].id)
		for _, next := range dependents[at] {
			blockedBy[next]--
			if blockedBy[next] > 0 {
				continue
			}
			ready = slices.Insert(ready, sort.SearchInts(ready, next), next)
		}
	}
	if len(sorted) < len(order) {
		// Every unplaced issue is still waiting on a dependency that is itself
		// waiting, which is a cycle by definition. Returning the partial
		// sequence would be an answer-shaped void — an order that looks like an
		// order and silently omits work. [LAW:no-silent-failure]
		return nil, &blocksCycleError{path: findBlocksCycle(constraints)}
	}
	return sorted, nil
}

// rankRewrites returns the rank writes that realize target, one per issue the
// new order actually moved.
//
// The issues whose stored ranks already ascend along target need no write: they
// are in the right relative order, and rewriting them would touch — and restamp
// updated_at on — rows that did not move. The longest such run is a longest
// strictly-increasing subsequence of target by stored rank; every issue outside
// it is spaced into the gap its neighbouring anchors leave.
func rankRewrites(order []rankedIssue, target []string) ([]rankRewrite, error) {
	rankOf := make(map[string]string, len(order))
	for _, item := range order {
		rankOf[item.id] = item.rank
	}
	anchored := make([]bool, len(target))
	for _, at := range anchorRun(target, rankOf) {
		anchored[at] = true
	}

	rewrites := make([]rankRewrite, 0, len(target))
	// Walk target as runs of movers delimited by anchors, closing the final run
	// at the sentinel index past the end. An absent bound is the empty string,
	// which the spacing primitive already reads as "past that end" — so a run
	// at either extreme, and a target with no anchors at all, need no special
	// case here. [LAW:dataflow-not-control-flow]
	lower, start := "", 0
	for at := 0; at <= len(target); at++ {
		if at < len(target) && !anchored[at] {
			continue
		}
		upper := ""
		if at < len(target) {
			upper = rankOf[target[at]]
		}
		movers := target[start:at]
		newRanks, err := rank.SpacedRanksBetween(lower, upper, len(movers))
		if err != nil {
			return nil, fmt.Errorf("space %d rank(s) between %q and %q: %w", len(movers), lower, upper, err)
		}
		for i, id := range movers {
			rewrites = append(rewrites, rankRewrite{id: id, newRank: newRanks[i]})
		}
		lower, start = upper, at+1
	}
	return rewrites, nil
}

// anchorRun returns the indices in target whose stored ranks already ascend —
// a longest subsequence of target strictly increasing by significant rank.
// These are the issues the repair leaves alone, so the count of everything else
// is the honest answer to "how many issues did this move".
//
// The movers between two anchors are spaced into the gap they bound, and only
// ranks whose significant parts differ leave one. [LAW:one-source-of-truth]
// rank.Significant is that definition of room, so anchors are compared by it.
// An issue whose significant rank is not rank.Valid — unranked, or all zeros —
// bounds no gap at all, so it is always a mover and gets a real rank.
func anchorRun(target []string, rankOf map[string]string) []int {
	keys := make([]string, len(target))
	for i, id := range target {
		keys[i] = rank.Significant(rankOf[id])
	}
	// tails[k] is the index in target of the smallest-keyed issue that ends an
	// ascending run of length k+1; prev[i] links i back to its predecessor in
	// the run it extends. Standard patience-sort reconstruction.
	tails := make([]int, 0, len(target))
	prev := make([]int, len(target))
	for i := range prev {
		prev[i] = -1
	}
	for i, key := range keys {
		if !rank.Valid(key) {
			continue
		}
		k := sort.Search(len(tails), func(k int) bool { return keys[tails[k]] >= key })
		if k > 0 {
			prev[i] = tails[k-1]
		}
		if k == len(tails) {
			tails = append(tails, i)
			continue
		}
		tails[k] = i
	}
	if len(tails) == 0 {
		return nil
	}
	run := make([]int, len(tails))
	for i, at := len(tails)-1, tails[len(tails)-1]; i >= 0; i, at = i-1, prev[at] {
		run[i] = at
	}
	return run
}

// invertedEdges returns the constraints the given order contradicts: those
// placing a dependency below the dependent it blocks.
//
// [LAW:single-enforcer] This is the exact predicate repairRankOrder removes —
// it returns an order for which this set is empty — computed from the same
// projection. Doctor's count and the repaired state cannot disagree about what
// an inversion is, because only one function decides.
func invertedEdges(order []rankedIssue, edges []blocksEdge) []blocksEdge {
	position := make(map[string]int, len(order))
	for at, item := range order {
		position[item.id] = at
	}
	inverted := make([]blocksEdge, 0)
	for _, e := range projectEdges(edges, position) {
		if position[e.dependency] > position[e.dependent] {
			inverted = append(inverted, e)
		}
	}
	return inverted
}
