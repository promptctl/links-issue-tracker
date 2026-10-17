package store

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// rankedIssue is one row of the sequence the repair permutes: an issue id and
// the rank string that currently places it.
type rankedIssue struct {
	id   string
	rank string
}

// compareRankedIssues is key order: rank, then id.
func compareRankedIssues(a, b rankedIssue) int {
	return cmp.Or(strings.Compare(a.rank, b.rank), strings.Compare(a.id, b.id))
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
	return fmt.Sprintf("blocked-by dependency cycle %s — a cycle has no valid rank order (%s); break it by removing one edge with 'lit dep rm'", strings.Join(e.path, " -> "), cycleMembers)
}

// cycleMembers says what a cycle's path names. The repair sorts edges lifted
// onto frame-mates, so two epics whose issues wait on each other's are a cycle
// as surely as two issues are, and the path names the epics.
// [LAW:one-source-of-truth] The refusal and Doctor's warning read it alike.
const cycleMembers = "an epic on it stands for the issues inside it, which its rank orders as one"

// repairRankOrder returns the rank writes that make every dependency outrank
// its dependent in tree order: the fewest writes that realize the stable
// topological order of order under constraints.
//
// order is every placed issue in key order, and constraints are the edges as
// rankConstraints lifts them, so each joins two frame-mates. The writes leave
// keys ascending along the sorted order, so every frame reads its share of
// that order back, and each mover is spaced between its neighbours in key
// order — inside its frame's gap, as a rank verb's room beside an anchor is —
// so no two issues share a key.
//
// [LAW:effects-at-boundaries] The new order and the writes that realize it are
// values computed here; the caller owns the transaction that applies them, so
// the whole repair is testable with no database.
func repairRankOrder(order []rankedIssue, constraints []blocksEdge) ([]rankRewrite, error) {
	target, err := stableTopoOrder(order, constraints)
	if err != nil {
		return nil, err
	}
	return rankRewrites(order, target)
}

// liveEdges keeps the blocked-by edges between two live issues, deduped. An
// edge with an endpoint outside live — closed, archived, or deleted work —
// says nothing about where live work sits, and one edge counted twice would
// leave its dependent blocked by a dependency that is only placed once.
func liveEdges(edges []blocksEdge, live map[string]struct{}) []blocksEdge {
	seen := make(map[blocksEdge]struct{}, len(edges))
	out := make([]blocksEdge, 0, len(edges))
	for _, e := range edges {
		_, depLive := live[e.dependency]
		_, dependentLive := live[e.dependent]
		if _, dup := seen[e]; dup || !depLive || !dependentLive {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// rankConstraints lifts each edge onto the pair of frame-mates tree order
// decides it by (storage.RankAncestry.Representatives), deduped: a key orders
// an issue among its frame-mates alone, so an edge between two frames is kept
// by ranking their representatives, as a rank verb asked about the same two
// issues moves them. An edge between a container and an issue inside it is
// dropped, because the tree orders that pair and no key can.
func rankConstraints(edges []blocksEdge, ancestry storage.RankAncestry) []blocksEdge {
	seen := make(map[blocksEdge]struct{}, len(edges))
	out := make([]blocksEdge, 0, len(edges))
	for _, e := range edges {
		dependent, dependency, comparable := ancestry.Representatives(e.dependent, e.dependency)
		lifted := blocksEdge{dependent: dependent, dependency: dependency}
		if _, dup := seen[lifted]; dup || !comparable {
			continue
		}
		seen[lifted] = struct{}{}
		out = append(out, lifted)
	}
	return out
}

// projectEdges keeps the edges whose endpoints both stand in the given order:
// live work is classified before the order is read, so an issue deleted in
// between is in one and not the other. [LAW:parse-dont-validate] Settled
// here, so everything downstream indexes the order directly: every edge it
// sees is in range.
func projectEdges(edges []blocksEdge, position map[string]int) []blocksEdge {
	out := make([]blocksEdge, 0, len(edges))
	for _, e := range edges {
		_, depInOrder := position[e.dependency]
		_, dependentInOrder := position[e.dependent]
		if depInOrder && dependentInOrder {
			out = append(out, e)
		}
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
	position := positions(order)
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
// updated_at on — rows that did not move. anchorRun chooses the longest such
// run, comparing significant ranks; every issue outside it is spaced into the
// gap its neighbouring anchors leave.
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
// These are the issues the repair does not write, so the count of everything
// else is the honest answer to "how many issues did this move".
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

// invertedEdges returns the edges tree order contradicts over the issues in
// order: those whose dependency lists after the dependent it blocks.
//
// [LAW:single-enforcer] storage.RankAncestry.Inverted decides, the predicate
// the queue's rank_inversion annotation reads, so Doctor's count and the queue
// cannot disagree about what an inversion is; and repairRankOrder returns an
// order for which this set is empty.
func invertedEdges(order []rankedIssue, ancestry storage.RankAncestry, edges []blocksEdge) []blocksEdge {
	issues := make(map[string]model.Issue, len(order))
	for _, item := range order {
		issues[item.id] = model.Issue{ID: item.id, Rank: item.rank}
	}
	inverted := make([]blocksEdge, 0)
	for _, e := range projectEdges(edges, positions(order)) {
		if ancestry.Inverted(issues[e.dependent], issues[e.dependency]) {
			inverted = append(inverted, e)
		}
	}
	return inverted
}

// blocksCycle returns one blocked-by dependency cycle among the constraints
// over order, as a repeated-endpoint path, or nil when they are acyclic.
//
// [LAW:single-enforcer] It asks findBlocksCycle about exactly the constraints
// stableTopoOrder sorts, so Doctor reports a cycle precisely when the repair
// refuses on one, and names the path the refusal names.
func blocksCycle(order []rankedIssue, constraints []blocksEdge) []string {
	return findBlocksCycle(projectEdges(constraints, positions(order)))
}

// positions maps each issue id in order to its index: the coordinate every
// constraint over that order is compared in.
func positions(order []rankedIssue) map[string]int {
	position := make(map[string]int, len(order))
	for at, item := range order {
		position[item.id] = at
	}
	return position
}
