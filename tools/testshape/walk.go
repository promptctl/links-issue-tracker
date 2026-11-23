package main

import (
	"fmt"

	"golang.org/x/tools/go/ssa"
)

// reach is everything one walk reached, in the order it reached it.
type reach struct {
	order   []ssa.Member
	parent  map[ssa.Member]ssa.Member // nil for a root
	asValue map[ssa.Member]bool       // reached at least once as a value
}

// walk is everything reachable from roots, breadth first, so each vertex's
// parent chain is a shortest path from a root.
func (g *graph) walk(roots []ssa.Member) *reach {
	r := &reach{parent: make(map[ssa.Member]ssa.Member), asValue: make(map[ssa.Member]bool)}
	var queue []ssa.Member
	visit := func(to, from ssa.Member, asValue bool) {
		if asValue {
			r.asValue[to] = true
		}
		if _, seen := r.parent[to]; seen {
			return
		}
		r.parent[to] = from
		r.order = append(r.order, to)
		queue = append(queue, to)
	}
	for _, root := range roots {
		visit(root, nil, false)
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, e := range g.nodes[v].edges {
			if f, ok := e.(follow); ok {
				visit(f.to, v, f.asValue)
			}
		}
	}
	return r
}

// hit is one touch a walk found: the vertex that makes it and the calls from
// there to the touch.
type hit struct {
	touch touch
	at    ssa.Member
	tail  []string
}

// hits are the touches r reached, in walk order: the markers reached
// vertices call, the spawns they make, and the spawns of a function reached
// as a value, whose program no caller resolves. self is the touch of a
// spawn of the test binary itself.
func (g *graph) hits(r *reach, self touch) []hit {
	var hs []hit
	for _, v := range r.order {
		n := g.nodes[v]
		for _, e := range n.edges {
			if m, ok := e.(reachTouch); ok {
				hs = append(hs, hit{m.touch, v, []string{m.name}})
			}
		}
		for _, s := range n.spawns {
			hs = append(hs, hit{s.program.touch(self), v, spawnTail(s.tail, s.program)})
		}
		if r.asValue[v] {
			for _, h := range n.holes {
				if h.via == viaParam {
					p := unresolvedProgram{}
					hs = append(hs, hit{p.touch(self), v, spawnTail(h.tail, p)})
				}
			}
		}
	}
	return hs
}

// witnessOf is h's witness: the path from r's root to the vertex that makes
// the touch, then the calls to it.
func witnessOf(r *reach, h hit) *witness {
	var path []fmt.Stringer
	for v := h.at; v != nil; v = r.parent[v] {
		path = append(path, v)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return &witness{path: path, tail: h.tail}
}
