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

// waitHop is every link readiness enforces from one issue to another. There is
// usually one, but a sibling can also depend on the sibling ahead of it, and
// cutting one of two links between a pair leaves the pair waiting.
type waitHop struct {
	waiter, prereq string
	kinds          []waitKind
}

// waitLoop is a loop of hops, in order: each hop's prereq is the next hop's
// waiter, and the last hop's prereq is the first's waiter. Every issue on it
// waits on itself, so readiness holds all of them.
type waitLoop []waitHop

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
	hops := make(map[string][]waitHop, len(graph))
	for waiter, links := range graph {
		kinds := map[string][]waitKind{}
		for _, link := range links {
			if holds(link) {
				kinds[link.prereq] = append(kinds[link.prereq], link.kind)
			}
		}
		// Sorted once here, so a loop reads the same on every run whatever
		// order the store returned its edges in.
		for _, prereq := range slices.Sorted(maps.Keys(kinds)) {
			hops[waiter] = append(hops[waiter], waitHop{waiter: waiter, prereq: prereq, kinds: kinds[prereq]})
		}
	}
	return loopsIn(hops), nil
}

// loopsIn returns, in id order, the shortest loop through each issue that waits
// on itself and lies on no loop already named, so every issue a loop holds
// appears on at least one of them.
func loopsIn(graph map[string][]waitHop) []waitLoop {
	named := map[string]bool{}
	var loops []waitLoop
	for _, start := range stuck(graph) {
		if named[start] {
			continue
		}
		loop := shortestLoop(graph, start)
		for _, hop := range loop {
			named[hop.waiter] = true
		}
		if len(loop) > 0 {
			loops = append(loops, loop)
		}
	}
	return loops
}

// stuck returns, sorted, the issues that can never finish: what is left once
// every issue whose prereqs can all finish is peeled away. Only those can be on
// a loop, so a backlog with none costs one pass over its links.
func stuck(graph map[string][]waitHop) []string {
	pending := make(map[string]int, len(graph))
	waitedOnBy := map[string][]string{}
	for waiter, hops := range graph {
		pending[waiter] = len(hops)
		for _, hop := range hops {
			waitedOnBy[hop.prereq] = append(waitedOnBy[hop.prereq], waiter)
		}
	}
	var free []string
	for prereq := range waitedOnBy {
		if _, waits := graph[prereq]; !waits {
			free = append(free, prereq)
		}
	}
	for len(free) > 0 {
		done := free[len(free)-1]
		free = free[:len(free)-1]
		for _, waiter := range waitedOnBy[done] {
			if pending[waiter]--; pending[waiter] == 0 {
				free = append(free, waiter)
			}
		}
	}
	var left []string
	for waiter, n := range pending {
		if n > 0 {
			left = append(left, waiter)
		}
	}
	slices.Sort(left)
	return left
}

// shortestLoop returns the shortest loop from start back to itself, or nil
// when start waits on nothing that leads back to it.
func shortestLoop(graph map[string][]waitHop, start string) waitLoop {
	via := map[string]waitHop{}
	for frontier := []string{start}; len(frontier) > 0; {
		var next []string
		for _, waiter := range frontier {
			for _, hop := range graph[waiter] {
				if hop.prereq == start {
					loop := waitLoop{hop}
					for at := hop.waiter; at != start; at = via[at].waiter {
						loop = append(loop, via[at])
					}
					slices.Reverse(loop)
					return loop
				}
				if _, seen := via[hop.prereq]; seen {
					continue
				}
				via[hop.prereq] = hop
				next = append(next, hop.prereq)
			}
		}
		frontier = next
	}
	return nil
}

// waitPhrases names a link of each kind as "<waiter> <phrase> <prereq>", so
// the reader can tell which edge to cut: a dependency, the child's parent, or
// the rank order. An inherited link never closes a loop, since settleWaits
// drops each one that would; its phrase is here so that the day one does, it
// is named like the rest.
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
		for i, hop := range loop {
			links := make([]string, len(hop.kinds))
			for j, kind := range hop.kinds {
				links[j] = waitPhrases[kind] + " " + hop.prereq
			}
			clauses[i] = hop.waiter + " " + strings.Join(links, " and ")
		}
		if _, err := fmt.Fprintf(w, "wait loop: %s — none of these is ready until one of the links is removed or one of the issues is closed\n", strings.Join(clauses, ", ")); err != nil {
			return err
		}
	}
	return nil
}
