package main

import (
	"golang.org/x/tools/go/ssa"
)

// reach is everything one walk reached, in the order it reached it.
type reach struct {
	order   []*ssa.Function
	parent  map[*ssa.Function]*ssa.Function // nil for a root
	dynamic map[*ssa.Function]bool          // reached at least once as a value
}

// walk is everything reachable from roots, breadth first, so each function's
// parent chain is a shortest path from a root. replaced are the package
// variables whose initializer is not followed: stored by the test's
// TestMain. A variable that a reached function other than package init
// stores into joins them, which is how a test's replaced seam is honored.
func (g *graph) walk(roots []*ssa.Function, replaced map[*ssa.Global]bool) *reach {
	r := &reach{parent: make(map[*ssa.Function]*ssa.Function), dynamic: make(map[*ssa.Function]bool)}
	var queue []*ssa.Function
	visit := func(to, from *ssa.Function, dynamic bool) {
		if dynamic {
			r.dynamic[to] = true
		}
		if _, seen := r.parent[to]; seen {
			return
		}
		r.parent[to] = from
		r.order = append(r.order, to)
		// A marker is recorded where it is reached and never followed.
		if _, walked := g.nodes[to]; walked {
			queue = append(queue, to)
		}
	}
	for _, root := range roots {
		visit(root, nil, false)
	}
	stored := make(map[*ssa.Global]bool, len(replaced))
	for gl := range replaced {
		stored[gl] = true
	}
	// Each function-typed variable a reached function reads, with the first
	// function that read it, in the order they were read.
	type read struct {
		gl   *ssa.Global
		from *ssa.Function
	}
	var reads []read
	readSeen := make(map[*ssa.Global]bool)
	initFollowed := make(map[*ssa.Global]bool)
	for {
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			n := g.nodes[fn]
			for _, gl := range n.stores {
				stored[gl] = true
			}
			for _, e := range n.edges {
				if e.to != nil {
					visit(e.to, fn, e.dynamic)
				}
			}
			for _, gl := range n.loads {
				if !readSeen[gl] {
					readSeen[gl] = true
					reads = append(reads, read{gl, fn})
				}
				for _, s := range g.stores[gl] {
					if !s.init && s.value != nil && g.walkableOrMarker(s.value) {
						visit(s.value, fn, true)
					}
				}
			}
		}
		// Only once nothing more is reachable is it known which variables
		// the walk replaced; an initializer is followed for the rest.
		progressed := false
		for _, rd := range reads {
			if stored[rd.gl] || initFollowed[rd.gl] {
				continue
			}
			initFollowed[rd.gl] = true
			for _, s := range g.stores[rd.gl] {
				if s.init && s.value != nil && g.walkableOrMarker(s.value) {
					visit(s.value, rd.from, true)
					progressed = true
				}
			}
		}
		if !progressed {
			return r
		}
	}
}

// walkableOrMarker reports whether the walk visits fn as a node.
func (g *graph) walkableOrMarker(fn *ssa.Function) bool {
	_, ok := g.nodes[fn]
	return ok || g.isMarker(fn)
}

// hit is one touch a walk found, with the chain that reaches it.
type hit struct {
	touch touch // touchSelf: the test binary itself, decided by TestMain
	chain []string
}

// touchSelf is a spawn of the test binary itself: a CLI run or another
// process, as the package's TestMain decides. It is never a column.
const touchSelf = numTouches

// hits are the touches r reached, each with the first chain found, in walk
// order: marker edges of reached functions, the spawns they make, and a
// spawner reached as a value, whose program is not known.
func (g *graph) hits(r *reach) []hit {
	var hs []hit
	for _, fn := range r.order {
		n, ok := g.nodes[fn]
		if !ok {
			// A marker reached as a value stored in a variable.
			name, t, _ := g.markerOf(fn)
			hs = append(hs, hit{t, append(g.chain(r, r.parent[fn]), name)})
			continue
		}
		for _, e := range n.edges {
			if e.to == nil {
				hs = append(hs, hit{e.touch, append(g.chain(r, fn), e.marker)})
			}
		}
		for _, s := range n.spawns {
			t := map[progKind]touch{progGit: touchGit, progOther: touchProc, progSelf: touchSelf}[s.kind]
			hs = append(hs, hit{t, append(g.chain(r, fn), s.tail...)})
		}
		if r.dynamic[fn] {
			for _, h := range n.holes {
				if h.kind == progParam {
					hs = append(hs, hit{touchProc, append(g.chain(r, fn), append(h.tail, "(program unknown)")...)})
				}
			}
		}
	}
	return hs
}

// chain is the path from a root to fn.
func (g *graph) chain(r *reach, fn *ssa.Function) []string {
	var c []string
	for ; fn != nil; fn = r.parent[fn] {
		c = append(c, g.display(fn))
	}
	for i, j := 0, len(c)-1; i < j; i, j = i+1, j-1 {
		c[i], c[j] = c[j], c[i]
	}
	return c
}

// replacedBy are the package variables that functions r reached store into.
func (g *graph) replacedBy(r *reach) map[*ssa.Global]bool {
	out := make(map[*ssa.Global]bool)
	for _, fn := range r.order {
		if n := g.nodes[fn]; n != nil {
			for _, gl := range n.stores {
				out[gl] = true
			}
		}
	}
	return out
}

// runsTests reports whether running fn can run the test binary's tests.
func (g *graph) runsTests(fn *ssa.Function) bool {
	if n := g.nodes[fn]; n == nil {
		return false
	}
	for _, f := range g.walk([]*ssa.Function{fn}, nil).order {
		if n := g.nodes[f]; n != nil && n.runsTests {
			return true
		}
	}
	return false
}

// childRunsCLI reports whether a child of the test binary that TestMain
// sends away from its tests runs the CLI: whether TestMain's blocks that
// cannot reach m.Run — the branch a child takes, ended by os.Exit — reach a
// CLI run. Without that branch, a child re-runs the binary's tests: the
// helper-process idiom, another process.
func (g *graph) childRunsCLI(testMain *ssa.Function) bool {
	child := g.childBlocks(testMain)
	var roots []*ssa.Function
	for _, e := range g.nodes[testMain].edges {
		if !child[e.block] {
			continue
		}
		if e.to == nil {
			if e.touch == touchCLI {
				return true
			}
			continue
		}
		roots = append(roots, e.to)
	}
	for _, h := range g.hits(g.walk(roots, nil)) {
		if h.touch == touchCLI {
			return true
		}
	}
	return false
}

// childBlocks are testMain's blocks from which no path reaches a call that
// runs the tests, a block's path ending at a call that does not return.
func (g *graph) childBlocks(testMain *ssa.Function) map[int]bool {
	type fate struct{ runs, exits bool }
	fates := make([]fate, len(testMain.Blocks))
	for _, b := range testMain.Blocks {
		fates[b.Index] = g.blockFate(b)
	}
	canRun := make([]bool, len(testMain.Blocks))
	for changed := true; changed; {
		changed = false
		for _, b := range testMain.Blocks {
			if canRun[b.Index] {
				continue
			}
			f := fates[b.Index]
			run := f.runs
			if !f.exits {
				for _, s := range b.Succs {
					run = run || canRun[s.Index]
				}
			}
			if run {
				canRun[b.Index], changed = true, true
			}
		}
	}
	child := make(map[int]bool)
	for i, run := range canRun {
		if !run {
			child[i] = true
		}
	}
	return child
}

// blockFate reads b in order up to its first call that runs the tests or
// does not return.
func (g *graph) blockFate(b *ssa.BasicBlock) (f struct{ runs, exits bool }) {
	for _, instr := range b.Instrs {
		var targets []*ssa.Function
		if c, ok := instr.(ssa.CallInstruction); ok && !c.Common().IsInvoke() {
			if callee := c.Common().StaticCallee(); callee != nil {
				name := fullName(callee)
				if name == mRun {
					f.runs = true
					return f
				}
				for _, nr := range noReturn {
					if name == nr {
						f.exits = true
						return f
					}
				}
				targets = append(targets, callee)
			}
		}
		for _, op := range instr.Operands(nil) {
			if fn := funcValue(*op); fn != nil {
				targets = append(targets, fn)
			}
		}
		for _, t := range targets {
			if g.runsTests(t) {
				f.runs = true
				return f
			}
		}
	}
	return f
}
