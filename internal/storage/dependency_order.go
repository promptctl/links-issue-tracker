package storage

import (
	"maps"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// DependencyEdge is one blocked-by relation read as a precedence constraint:
// the dependent must be ranked below the dependency, i.e. the dependency comes
// first. A rank order is a total order over issues, and a total order that
// satisfies every blocked-by edge exists if and only if the precedence graph is
// acyclic. A cycle is therefore not a transient rank stall but an
// unsatisfiable constraint set — no assignment can place every dependency
// above its dependent once the dependencies form a loop.
type DependencyEdge struct {
	Dependent  string // src — ranked below
	Dependency string // dst — ranked above
}

// DependencyOrder is tree order's reading of the blocked-by edges between live
// issues: which of them a rank order can satisfy, and which no rank order can.
//
// A key orders an issue among its frame-mates alone, so tree order decides an
// edge between two frames by the frame-mates that stand for its two ends
// ([RankAncestry.Representatives]): the edge lifted onto them. Lifted edges can
// run both ways between frame-mates where no edge between issues loops — a
// task in one epic waits on a task in a second epic, which waits on another
// task in the first. That plan is workable, and no order of the two epics lists
// every dependency first. Such frame-mates are interleaved: each strongly
// connected set of the lifted edges is one group, and an edge lifted inside a
// group is neither a rank inversion nor a constraint on the rank repair,
// because every arrangement of the group's keys puts some edge in it wrong. A
// group that a loop of the issues' own edges runs through is a dependency
// cycle rather than an interleaved plan: no work in that loop can start.
//
// [LAW:single-enforcer] The one definition of a rank inversion and of what the
// repair must satisfy: the queue's rank_inversion annotation, lit doctor's
// count and the repair all read this value, so after a repair nothing is left
// for the queue to advertise the repair for.
type DependencyOrder struct {
	ancestry    RankAncestry
	edges       map[DependencyEdge]struct{}
	constraints []DependencyEdge
	interleaved [][]string
	group       map[string]int
}

// NewDependencyOrder reads edges, the blocked-by edges between live issues, in
// tree order.
func NewDependencyOrder(ancestry RankAncestry, edges []DependencyEdge) DependencyOrder {
	order := DependencyOrder{ancestry: ancestry, edges: make(map[DependencyEdge]struct{}, len(edges)), group: map[string]int{}}
	liftedFrom := make(map[DependencyEdge]DependencyEdge, len(edges))
	seen := make(map[DependencyEdge]struct{}, len(edges))
	lifted := make([]DependencyEdge, 0, len(edges))
	for _, e := range edges {
		order.edges[e] = struct{}{}
		dependent, dependency, comparable := ancestry.Representatives(e.Dependent, e.Dependency)
		if !comparable {
			continue
		}
		edge := DependencyEdge{Dependent: dependent, Dependency: dependency}
		liftedFrom[e] = edge
		if _, dup := seen[edge]; !dup {
			seen[edge] = struct{}{}
			lifted = append(lifted, edge)
		}
	}
	groups := stronglyConnected(lifted)
	for i, members := range groups {
		for _, id := range members {
			order.group[id] = i
		}
	}
	// An edge on a loop of the issues' own edges joins two members of one
	// strongly connected set of them; the group it lifts into is that loop
	// seen from above, not an interleaving.
	loop := map[string]int{}
	for i, members := range stronglyConnected(edges) {
		for _, id := range members {
			loop[id] = i
		}
	}
	looped := map[int]bool{}
	for e, edge := range liftedFrom {
		dependentLoop, onLoop := loop[e.Dependent]
		dependencyLoop, alsoOnLoop := loop[e.Dependency]
		if onLoop && alsoOnLoop && dependentLoop == dependencyLoop && order.together(edge.Dependent, edge.Dependency) {
			looped[order.group[edge.Dependent]] = true
		}
	}
	for i, members := range groups {
		if !looped[i] {
			order.interleaved = append(order.interleaved, members)
		}
	}
	for _, edge := range lifted {
		if !order.together(edge.Dependent, edge.Dependency) {
			order.constraints = append(order.constraints, edge)
		}
	}
	return order
}

// Inverted reports whether tree order lists dependency after dependent where
// some rank order would list it first: a rank inversion, a dependency ranked
// below the work that waits on it, which the rank repair clears. An edge
// between a container and an issue inside it is never one, because the tree
// orders that pair and no key can; nor is an edge inside a group of
// frame-mates whose lifted edges loop; nor is an edge the order was not built
// from, so a closed issue's edges are none, as they are none of the repair's.
func (o DependencyOrder) Inverted(dependent, dependency model.Issue) bool {
	_, read := o.edges[DependencyEdge{Dependent: dependent.ID, Dependency: dependency.ID}]
	dependentRep, dependencyRep, comparable := o.ancestry.Representatives(dependent.ID, dependency.ID)
	return read && comparable && !o.together(dependentRep, dependencyRep) && o.ancestry.Compare(dependency, dependent) > 0
}

// Constraints are the edges lifted onto frame-mates, deduped, that a rank order
// can satisfy all at once: every one but those inside a group, which leaves no
// cycle among them.
func (o DependencyOrder) Constraints() []DependencyEdge {
	return o.constraints
}

// Interleaved lists the interleaved groups, each by member id, in order of
// their first members: every group of frame-mates whose lifted edges loop,
// less those a loop of the issues' own edges runs through.
func (o DependencyOrder) Interleaved() [][]string {
	return o.interleaved
}

func (o DependencyOrder) together(x, y string) bool {
	gx, inX := o.group[x]
	gy, inY := o.group[y]
	return inX && inY && gx == gy
}

// stronglyConnected returns every strongly connected set of more than one node
// among edges (Tarjan's algorithm), each sorted by id and the sets by their
// first member, so the same edges name the same groups on every run.
func stronglyConnected(edges []DependencyEdge) [][]string {
	next := map[string][]string{}
	for _, e := range edges {
		next[e.Dependent] = append(next[e.Dependent], e.Dependency)
	}
	nodes := slices.Sorted(maps.Keys(next))
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var groups [][]string
	var visit func(v string)
	visit = func(v string) {
		index[v] = len(index)
		low[v] = index[v]
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range next[v] {
			if _, visited := index[w]; !visited {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var group []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			group = append(group, w)
			if w == v {
				break
			}
		}
		if len(group) > 1 {
			slices.Sort(group)
			groups = append(groups, group)
		}
	}
	for _, v := range nodes {
		if _, visited := index[v]; !visited {
			visit(v)
		}
	}
	slices.SortFunc(groups, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return groups
}
