package storage

import (
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A hierarchy is a forest: every issue reaches a root by walking up its parent
// edges, and an epic's state is derived from its children's. A parent cycle has
// no root, so no epic in it has a state to derive. The write boundary refuses
// the edge that would close a loop (rejectParentCycle), a write that lands an
// export whole cuts every loop the export holds (BreakParentCycles), a sync's
// lift of a historical commit cuts every loop that commit holds (Unloop), and a
// loop stored before these rules existed is refused by every read that would
// derive from it (ParentCycleError) and named by Doctor. [LAW:single-enforcer]

// ParentCycleRepair is the repair every report of a stored parent loop names.
// A loop is usually leaves, and a leaf cannot be detached, so the first way is
// the reparent: it replaces every parent the member holds, and it climbs only
// above the epic it names, so that epic must sit outside the loop. The second
// way is for a member that is an epic belonging at the top level.
// [LAW:one-source-of-truth] Doctor, the write boundary and the read refusal
// name one repair.
const ParentCycleRepair = "break it with 'lit parent set --child <one member> --parent <an epic outside the loop>', which replaces every parent that member holds, or with 'lit parent clear <member>' for a member that is an epic belonging at the top level"

// ParentCycleError is a read's refusal of a hierarchy that loops: Cycle names
// the loop's members in walk order, each the child of the next and the last
// the child of the first. The CLI classifies it by its type, as corruption
// whose repair the message carries. [LAW:types-are-the-program]
type ParentCycleError struct {
	Cycle []string
}

func (e ParentCycleError) Error() string {
	return fmt.Sprintf("parent cycle: %s (a hierarchy has no root once it loops, so no epic in it has a state to derive — %s)", strings.Join(e.Cycle, " -> "), ParentCycleRepair)
}

// ParentCycle returns the members of one loop in the hierarchy, in walk order,
// or nothing when the parent graph is a forest. parentOf maps each child to
// every parent edge it holds.
//
// A child can hold more than one parent edge in restored data, so the walk is a
// depth-first search rather than a single chain, and it carries its path so the
// loop can be named by its members rather than merely detected. A node still on
// the current path is the loop; a node already settled reaches a root, and its
// ancestry is never walked twice, which keeps the whole scan linear in edges.
func ParentCycle(parentOf map[string][]string) []string {
	const (
		unvisited = iota
		onPath
		settled
	)
	state := make(map[string]int, len(parentOf))
	starts := make([]string, 0, len(parentOf))
	for child := range parentOf {
		starts = append(starts, child)
	}
	// The map's iteration order is random; a cycle report that names the same
	// members in a different order on every run is a fact nobody can act on.
	slices.Sort(starts)
	for _, start := range starts {
		if state[start] != unvisited {
			continue
		}
		// path is the chain of nodes currently being descended; taken[i] counts
		// how many of path[i]'s parents have been followed already.
		path := []string{start}
		taken := []int{0}
		state[start] = onPath
		for len(path) > 0 {
			at := len(path) - 1
			parents := parentOf[path[at]]
			if taken[at] >= len(parents) {
				state[path[at]] = settled
				path, taken = path[:at], taken[:at]
				continue
			}
			parent := parents[taken[at]]
			taken[at]++
			switch state[parent] {
			case onPath:
				for i, id := range path {
					if id == parent {
						return append([]string{}, path[i:]...)
					}
				}
			case unvisited:
				state[parent] = onPath
				path = append(path, parent)
				taken = append(taken, 0)
			}
		}
	}
	return nil
}

// Unlooped is one parent edge a landing cut because it closed a loop: the
// child that held it, the parent it named, and the loop it closed, in walk
// order. The child keeps any other parent it holds; a leaf left with none is
// moved into an epic by HomeStranded, which runs after.
type Unlooped struct {
	IssueID string   `json:"issue_id"`
	Parent  string   `json:"parent"`
	Cycle   []string `json:"cycle"`
}

// Unloop names the parent edges that cut every loop in the parent graph
// parentOf maps out, in the order it cuts them. In each loop the edge that goes
// is the one held by the member with the greatest id, onto the next member of
// the loop: a choice every clone computes identically from the same data, so
// two clones landing one backlog, or one clone reading the same commit twice,
// cut the same edge. Each pass cuts one edge, so it ends. A forest names
// nothing. parentOf is left as it was.
//
// [LAW:one-source-of-truth] The landing (BreakParentCycles) and the store's
// lift of a historical commit cut by this one rule.
func Unloop(parentOf map[string][]string) []Unlooped {
	remaining := make(map[string][]string, len(parentOf))
	for child, parents := range parentOf {
		remaining[child] = slices.Clone(parents)
	}
	var unlooped []Unlooped
	for cycle := ParentCycle(remaining); len(cycle) > 0; cycle = ParentCycle(remaining) {
		at := slices.Index(cycle, slices.Max(cycle))
		cut := Unlooped{IssueID: cycle[at], Parent: cycle[(at+1)%len(cycle)], Cycle: cycle}
		remaining[cut.IssueID] = slices.DeleteFunc(remaining[cut.IssueID], func(parent string) bool { return parent == cut.Parent })
		unlooped = append(unlooped, cut)
	}
	return unlooped
}

// BreakParentCycles returns export with every parent loop cut by Unloop's rule,
// and names each edge it cut, in the order it cut them. An export with no loop
// comes back unchanged with nothing named.
//
// It is pure. [LAW:effects-at-boundaries]
func BreakParentCycles(export model.Export) (model.Export, []Unlooped) {
	parentOf := map[string][]string{}
	for _, relation := range export.Relations {
		if relation.Type == model.RelParentChild {
			parentOf[relation.SrcID] = append(parentOf[relation.SrcID], relation.DstID)
		}
	}
	unlooped := Unloop(parentOf)
	if len(unlooped) == 0 {
		return export, nil
	}
	out := export
	out.Relations = slices.DeleteFunc(slices.Clone(export.Relations), func(relation model.Relation) bool {
		return relation.Type == model.RelParentChild && slices.ContainsFunc(unlooped, func(cut Unlooped) bool {
			return relation.SrcID == cut.IssueID && relation.DstID == cut.Parent
		})
	})
	return out, unlooped
}
