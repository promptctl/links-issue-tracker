package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// waitLoop is a loop of links readiness enforces, in order: each link's prereq
// is the next link's waiter, and the last link's prereq is the first's waiter.
// Every issue on it waits on itself, so none of them can ever start.
type waitLoop []waitLink

// doctorWaitLoops is lit doctor's wait-loop check: the loops found and their
// count for the status line, or "unchecked" when the hierarchy holds a loop,
// since the walk readiness runs climbs the hierarchy and would not return.
func doctorWaitLoops(ctx context.Context, st storage.Store, report storage.HealthReport) (string, []waitLoop, error) {
	if len(report.ParentCycle) > 0 {
		return "unchecked", nil, nil
	}
	loops, err := findWaitLoops(ctx, st)
	if err != nil {
		return "", nil, err
	}
	return strconv.Itoa(len(loops)), loops, nil
}

// findWaitLoops returns the loops among unfinished issues that wait on one
// another, as loopsIn picks them.
//
// [LAW:one-source-of-truth] The links are the ones readiness gates on:
// fetchWaitLinks expands them and settleWaits drops the passed-down blockers
// that would close a loop, exactly as heldAncestry does for lit backlog and lit
// next. A loop named here is one those commands enforce.
func findWaitLoops(ctx context.Context, st storage.Store) ([]waitLoop, error) {
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{
		Statuses: []model.State{model.StateOpen, model.StateInProgress},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	memo, _ := memoizeRelations(st.GetRelationsByIDs)
	graph, err := fetchWaitGraph(ctx, memo, ids)
	if err != nil {
		return nil, err
	}
	holds := heldAgainst(settleWaits(graph, nil))
	enforced := make(map[string][]waitLink, len(graph))
	for waiter, links := range graph {
		enforced[waiter] = slices.DeleteFunc(slices.Clone(links), func(link waitLink) bool { return !holds(link) })
	}
	return loopsIn(enforced), nil
}

// loopsIn returns, in id order, the shortest loop through each issue that waits
// on itself and lies on no loop already named, so every issue a loop holds
// appears on at least one of them.
func loopsIn(graph map[string][]waitLink) []waitLoop {
	named := map[string]bool{}
	var loops []waitLoop
	for _, start := range slices.Sorted(maps.Keys(graph)) {
		if named[start] {
			continue
		}
		loop := shortestLoop(graph, start)
		for _, link := range loop {
			named[link.waiter] = true
		}
		if len(loop) > 0 {
			loops = append(loops, loop)
		}
	}
	return loops
}

// shortestLoop returns the shortest loop from start back to itself, or nil
// when start waits on nothing that leads back to it.
func shortestLoop(graph map[string][]waitLink, start string) waitLoop {
	via := map[string]waitLink{}
	for frontier := []string{start}; len(frontier) > 0; {
		var next []string
		for _, waiter := range frontier {
			for _, link := range sortedLinks(graph[waiter]) {
				if link.prereq == start {
					loop := waitLoop{link}
					for at := link.waiter; at != start; at = via[at].waiter {
						loop = append(loop, via[at])
					}
					slices.Reverse(loop)
					return loop
				}
				if _, seen := via[link.prereq]; seen {
					continue
				}
				via[link.prereq] = link
				next = append(next, link.prereq)
			}
		}
		frontier = next
	}
	return nil
}

// sortedLinks orders links by prereq, so a loop reads the same on every run
// whatever order the store returned its edges in.
func sortedLinks(links []waitLink) []waitLink {
	return slices.SortedFunc(slices.Values(links), func(a, b waitLink) int { return strings.Compare(a.prereq, b.prereq) })
}

// waitPhrases names a link of each kind as "<waiter> <phrase> <prereq>", so
// the reader can tell which edge to cut: a dependency, the epic's blocker, the
// child's parent, or the rank order.
var waitPhrases = [...]string{
	waitDependency:     "depends on",
	waitInherited:      "is held back by its epic's blocker",
	waitChild:          "waits on its child",
	waitEarlierSibling: "waits on earlier sibling",
}

// printWaitLoops writes one line per loop, naming every link that composes it.
func printWaitLoops(w io.Writer, loops []waitLoop) error {
	for _, loop := range loops {
		clauses := make([]string, len(loop))
		for i, link := range loop {
			clauses[i] = link.waiter + " " + waitPhrases[link.kind] + " " + link.prereq
		}
		if _, err := fmt.Fprintf(w, "wait loop: %s — none of these can start until one of the links is removed\n", strings.Join(clauses, ", ")); err != nil {
			return err
		}
	}
	return nil
}
