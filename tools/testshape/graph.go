package main

import (
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// touch is one way a test leaves its process. The order is the table's
// column order.
type touch int

const (
	touchNetwork touch = iota
	touchFS
	touchClock
	touchGit
	touchStore
	touchProc
	touchCLI
	numTouches
)

var touchNames = [numTouches]string{"network", "fs", "clock", "git", "store", "proc", "cli"}

func (t touch) String() string { return touchNames[t] }

// marker is one function whose call is a touch.
type marker struct {
	name  string
	touch touch
}

// spawner is one function that starts the program its argument names.
type spawner struct {
	name    string
	program int // the index of the argument naming the program
}

// markerSet is the marker data resolved against one module: every name full.
type markerSet struct {
	touches  map[string]touch
	spawners map[string]int
}

func newMarkerSet(module string, moduleMarkers, external []marker, spawns []spawner) markerSet {
	ms := markerSet{touches: make(map[string]touch), spawners: make(map[string]int)}
	for _, m := range moduleMarkers {
		ms.touches[qualify(module, m.name)] = m.touch
	}
	for _, m := range external {
		ms.touches[m.name] = m.touch
	}
	for _, s := range spawns {
		ms.spawners[s.name] = s.program
	}
	return ms
}

// progKind is what a spawn's program argument resolved to.
type progKind int

const (
	progGit   progKind = iota // git, by name or by a path ending in /git
	progOther                 // any other program, or one not known
	progSelf                  // this test binary
	// The program is the enclosing function's parameter or free variable:
	// the function is a spawner, and each place that calls or builds it
	// resolves the program.
	progParam
	progFreeVar
)

// program is one resolution of a spawn's program argument.
type program struct {
	kind  progKind
	name  string // progOther: the program's name, when it is a constant
	index int    // progParam, progFreeVar: which one
	addr  bool   // progFreeVar: the variable holds the program's address
}

// spawn is a process a function starts, its program resolved.
type spawn struct {
	kind progKind // progGit, progOther or progSelf
	name string
	tail []string // the calls from the function to the spawning call
}

// hole is a spawn whose program enters through a parameter or free variable.
type hole struct {
	program
	tail []string
}

// edge is one way control can pass from a function. A marker edge records
// its touch and is never followed.
type edge struct {
	to      *ssa.Function
	dynamic bool // reached as a value, not by a static call
	block   int
	marker  string
	touch   touch
}

// callSite is a static call whose arguments can name a spawned program.
type callSite struct {
	callee  *ssa.Function // a module function; nil for an external spawner
	spawner string        // the external spawner called; "" for a module function
	args    []ssa.Value
}

// node is one module function's facts, read from its SSA once.
type node struct {
	fn        *ssa.Function
	edges     []edge
	loads     []*ssa.Global // function-typed package variables it reads
	stores    []*ssa.Global // package variables it writes; empty for package init
	runsTests bool          // it calls (*testing.M).Run
	calls     []callSite
	closures  []*ssa.MakeClosure
	cmdPaths  []ssa.Value // values stored into an exec.Cmd's Path
	spawns    []spawn
	holes     []hole
}

// globalStore is one store of a function value into a package variable.
type globalStore struct {
	value *ssa.Function
	init  bool // stored by the package's initializer
}

// graph is the module's functions and their edges: computed once from the
// loaded program, then read by every walk. Nothing in it does I/O.
// [LAW:effects-at-boundaries]
type graph struct {
	module  string
	markers markerSet
	nodes   map[*ssa.Function]*node
	stores  map[*ssa.Global][]globalStore
}

// buildGraph reads every module function's SSA into its node, then resolves
// spawned programs through helpers until no new spawner appears.
func buildGraph(l *loaded, ms markerSet) *graph {
	g := &graph{module: l.module, markers: ms, nodes: make(map[*ssa.Function]*node), stores: make(map[*ssa.Global][]globalStore)}
	var fns []*ssa.Function
	for fn := range ssautil.AllFunctions(l.prog) {
		if g.walkable(fn) {
			fns = append(fns, fn)
		}
	}
	// A stable order, so every witness chain is the same on every run.
	sort.Slice(fns, func(i, j int) bool { return fnLess(fns[i], fns[j]) })
	for _, fn := range fns {
		g.nodes[fn] = nil
	}
	// Reading a node can materialize a method wrapper no earlier function
	// named; it is read in turn, so every edge's target has a node.
	for i := 0; i < len(fns); i++ {
		n := g.read(fns[i])
		g.nodes[fns[i]] = n
		for _, e := range n.edges {
			if _, ok := g.nodes[e.to]; e.to != nil && !ok {
				g.nodes[e.to] = nil
				fns = append(fns, e.to)
			}
		}
	}
	g.resolveSpawns(fns)
	return g
}

func fnLess(a, b *ssa.Function) bool {
	if a.Pos() != b.Pos() {
		return a.Pos() < b.Pos()
	}
	return a.String() < b.String()
}

// declaredIn is the import path of the package that declares fn: a wrapper's
// is its method's, an instance's its generic's.
func declaredIn(fn *ssa.Function) string {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		return obj.Pkg().Path()
	}
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if fn.Pkg != nil {
		return fn.Pkg.Pkg.Path()
	}
	return ""
}

// walkable reports whether the walk follows fn's body: it has one and the
// module declares it.
func (g *graph) walkable(fn *ssa.Function) bool {
	return len(fn.Blocks) > 0 && inModule(g.module, declaredIn(fn)) && !g.isMarker(fn)
}

// fullName is the full name of the function fn is, or of the generic it
// instantiates; "" for a function literal.
func fullName(fn *ssa.Function) string {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	if f, ok := fn.Object().(*types.Func); ok {
		return f.FullName()
	}
	return ""
}

// markerOf is the touch calling fn makes, if fn is a marker: a listed
// function, an external spawner reached as a value, or a main package's main.
func (g *graph) markerOf(fn *ssa.Function) (string, touch, bool) {
	name := fullName(fn)
	if t, ok := g.markers.touches[name]; ok {
		return g.short(name), t, true
	}
	if _, ok := g.markers.spawners[name]; ok {
		return name + " (program unknown)", touchProc, true
	}
	if f, ok := fn.Object().(*types.Func); ok && f.Name() == "main" && f.Pkg() != nil &&
		f.Pkg().Name() == "main" && f.Parent() == f.Pkg().Scope() && inModule(g.module, f.Pkg().Path()) {
		return g.short(name), touchCLI, true
	}
	return "", 0, false
}

func (g *graph) isMarker(fn *ssa.Function) bool {
	_, _, ok := g.markerOf(fn)
	return ok
}

// edgeTo is the edge to fn, or false when the walk neither follows nor
// records it: a function outside the module that is no marker.
func (g *graph) edgeTo(fn *ssa.Function, dynamic bool, block int) (edge, bool) {
	if name, t, ok := g.markerOf(fn); ok {
		return edge{marker: name, touch: t, block: block}, true
	}
	if g.walkable(fn) {
		return edge{to: fn, dynamic: dynamic, block: block}, true
	}
	return edge{}, false
}

// isInit reports whether fn is, or is a literal inside, a package
// initializer.
func isInit(fn *ssa.Function) bool {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	return fn.Synthetic == "package initializer" || strings.HasPrefix(fn.Name(), "init#")
}

// funcValue is the function v is, when v is a function or a closure of one.
func funcValue(v ssa.Value) *ssa.Function {
	switch v := v.(type) {
	case *ssa.Function:
		return v
	case *ssa.MakeClosure:
		return v.Fn.(*ssa.Function)
	}
	return nil
}

// onlyCalled reports whether every use of the closure mc calls it, so each
// call resolves the programs its parameters name.
func onlyCalled(mc *ssa.MakeClosure) bool {
	for _, r := range *mc.Referrers() {
		c, ok := r.(ssa.CallInstruction)
		if !ok || c.Common().IsInvoke() || c.Common().Value != mc {
			return false
		}
	}
	return true
}

// read is fn's node: every edge out of it, the package variables it reads
// and writes, and the call sites that can name a spawned program.
func (g *graph) read(fn *ssa.Function) *node {
	n := &node{fn: fn}
	add := func(to *ssa.Function, dynamic bool, block int) {
		if e, ok := g.edgeTo(to, dynamic, block); ok {
			n.edges = append(n.edges, e)
		}
	}
	initFn := isInit(fn)
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			var callee *ssa.Function
			if c, ok := instr.(ssa.CallInstruction); ok {
				common := c.Common()
				if common.IsInvoke() {
					// A call through an interface reaches no implementation;
					// only an interface method that is itself a marker counts.
					if t, ok := g.markers.touches[common.Method.FullName()]; ok {
						n.edges = append(n.edges, edge{marker: common.Method.FullName(), touch: t, block: b.Index})
					}
				} else if callee = common.StaticCallee(); callee != nil {
					name := fullName(callee)
					switch {
					case name == mRun:
						n.runsTests = true
					case g.isSpawner(name):
						n.calls = append(n.calls, callSite{spawner: name, args: common.Args})
					default:
						add(callee, false, b.Index)
						if g.walkable(callee) {
							n.calls = append(n.calls, callSite{callee: callee, args: common.Args})
						}
					}
				}
			}
			switch instr := instr.(type) {
			case *ssa.MakeClosure:
				fnv := instr.Fn.(*ssa.Function)
				add(fnv, !onlyCalled(instr), b.Index)
				if g.walkable(fnv) {
					n.closures = append(n.closures, instr)
				}
			case *ssa.MakeInterface:
				// Whoever converts a value to an interface carries every
				// method the value could then be called by.
				for _, m := range g.methods(fn.Prog, instr.X.Type()) {
					add(m, true, b.Index)
				}
			case *ssa.UnOp:
				if gl, ok := instr.X.(*ssa.Global); ok && instr.Op == token.MUL && isFuncVar(gl) {
					n.loads = append(n.loads, gl)
				}
			case *ssa.Store:
				if gl, ok := instr.Addr.(*ssa.Global); ok {
					if !initFn {
						n.stores = append(n.stores, gl)
					}
					if isFuncVar(gl) {
						g.stores[gl] = append(g.stores[gl], globalStore{value: funcValue(instr.Val), init: initFn})
					}
				}
				if isCmdPath(instr.Addr) {
					n.cmdPaths = append(n.cmdPaths, instr.Val)
				}
			}
			// Every other function this one uses as a value.
			for _, op := range instr.Operands(nil) {
				if f, ok := (*op).(*ssa.Function); ok && f != callee {
					if _, isClosure := instr.(*ssa.MakeClosure); !isClosure {
						add(f, true, b.Index)
					}
				}
			}
		}
	}
	return n
}

// isSpawner reports whether name is an external spawner.
func (g *graph) isSpawner(name string) bool {
	_, ok := g.markers.spawners[name]
	return ok
}

// methods are the module-declared methods in t's method set: those a value
// of t converted to an interface can be called by. A type declared outside
// the module brings none, as no call outside the module is followed.
func (g *graph) methods(prog *ssa.Program, t types.Type) []*ssa.Function {
	base := t
	if p, ok := base.(*types.Pointer); ok {
		base = p.Elem()
	}
	named, ok := base.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || !inModule(g.module, named.Obj().Pkg().Path()) {
		return nil
	}
	mset := prog.MethodSets.MethodSet(t)
	var fns []*ssa.Function
	for i := range mset.Len() {
		if f := prog.MethodValue(mset.At(i)); f != nil {
			fns = append(fns, f)
		}
	}
	return fns
}

// isFuncVar reports whether gl is a package variable of function type.
func isFuncVar(gl *ssa.Global) bool {
	_, ok := gl.Type().(*types.Pointer).Elem().Underlying().(*types.Signature)
	return ok
}

// isCmdPath reports whether addr is the Path field of an exec.Cmd.
func isCmdPath(addr ssa.Value) bool {
	fa, ok := addr.(*ssa.FieldAddr)
	if !ok {
		return false
	}
	st, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := st.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path()+"."+named.Obj().Name() != cmdType {
		return false
	}
	return named.Underlying().(*types.Struct).Field(fa.Field).Name() == cmdPathField
}

// resolveSpawns resolves every spawn's program to a fixed point: a function
// that passes its parameter or free variable on as a program is a spawner
// itself, and the function that calls or builds it resolves it in turn,
// through as many helpers as it takes.
func (g *graph) resolveSpawns(fns []*ssa.Function) {
	for changed := true; changed; {
		changed = false
		for _, fn := range fns {
			n := g.nodes[fn]
			for _, f := range g.flows(n) {
				for _, p := range resolve(f.value, f.addr, fn, map[ssa.Value]bool{}) {
					changed = n.record(p, f.tail) || changed
				}
			}
		}
	}
}

// flow is one value that names a spawned program: tail is the calls from the
// function that holds the value to the spawn.
type flow struct {
	value ssa.Value
	addr  bool
	tail  []string
}

func (g *graph) flows(n *node) []flow {
	var fs []flow
	for _, c := range n.calls {
		if c.callee == nil {
			fs = append(fs, flow{value: c.args[g.markers.spawners[c.spawner]], tail: []string{c.spawner}})
			continue
		}
		for _, h := range g.nodes[c.callee].holes {
			if h.kind == progParam {
				fs = append(fs, flow{value: c.args[h.index], tail: append([]string{g.display(c.callee)}, h.tail...)})
			}
		}
	}
	for _, mc := range n.closures {
		closure := mc.Fn.(*ssa.Function)
		for _, h := range g.nodes[closure].holes {
			if h.kind == progFreeVar {
				fs = append(fs, flow{value: mc.Bindings[h.index], addr: h.addr, tail: append([]string{g.display(closure)}, h.tail...)})
			}
		}
	}
	for _, v := range n.cmdPaths {
		fs = append(fs, flow{value: v, tail: []string{cmdType + "." + cmdPathField}})
	}
	return fs
}

// record adds p, reached through tail, to n: a hole if p is still n's own
// parameter or free variable, a spawn otherwise. It reports whether n gained
// a hole, the one change that can resolve more spawns.
func (n *node) record(p program, tail []string) bool {
	switch p.kind {
	case progParam, progFreeVar:
		if slices.ContainsFunc(n.holes, func(h hole) bool { return h.program == p }) {
			return false
		}
		n.holes = append(n.holes, hole{p, tail})
		return true
	default:
		if !slices.ContainsFunc(n.spawns, func(s spawn) bool { return s.kind == p.kind && s.name == p.name }) {
			n.spawns = append(n.spawns, spawn{kind: p.kind, name: p.name, tail: tail})
		}
		return false
	}
}

// resolve is what program v names in fn; with addr, v holds the program's
// address. seen stops a cycle of phis.
func resolve(v ssa.Value, addr bool, fn *ssa.Function, seen map[ssa.Value]bool) []program {
	if seen[v] {
		return nil
	}
	seen[v] = true
	if addr {
		return resolveAddr(v, fn, seen)
	}
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value == nil || v.Value.Kind() != constant.String {
			return []program{{kind: progOther}}
		}
		name := constant.StringVal(v.Value)
		if name == "git" || strings.HasSuffix(name, "/git") {
			return []program{{kind: progGit, name: name}}
		}
		return []program{{kind: progOther, name: name}}
	case *ssa.Parameter:
		return []program{{kind: progParam, index: slices.Index(fn.Params, v)}}
	case *ssa.FreeVar:
		return []program{{kind: progFreeVar, index: slices.Index(fn.FreeVars, v)}}
	case *ssa.UnOp:
		if v.Op == token.MUL {
			return resolveAddr(v.X, fn, seen)
		}
	case *ssa.ChangeType:
		return resolve(v.X, false, fn, seen)
	case *ssa.Convert:
		return resolve(v.X, false, fn, seen)
	case *ssa.Phi:
		var ps []program
		for _, e := range v.Edges {
			ps = append(ps, resolve(e, false, fn, seen)...)
		}
		return ps
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if callee := call.Call.StaticCallee(); callee != nil {
				switch fullName(callee) {
				case selfExecutable:
					return []program{{kind: progSelf}}
				case lookPath:
					return resolve(call.Call.Args[0], false, fn, seen)
				}
			}
		}
	}
	return []program{{kind: progOther}}
}

// resolveAddr is what program the variable at addr holds in fn.
func resolveAddr(addr ssa.Value, fn *ssa.Function, seen map[ssa.Value]bool) []program {
	switch a := addr.(type) {
	case *ssa.Alloc:
		var ps []program
		for _, r := range *a.Referrers() {
			if st, ok := r.(*ssa.Store); ok && st.Addr == a {
				ps = append(ps, resolve(st.Val, false, fn, seen)...)
			}
		}
		return ps
	case *ssa.FreeVar:
		return []program{{kind: progFreeVar, index: slices.Index(fn.FreeVars, a), addr: true}}
	case *ssa.IndexAddr:
		// os.Args[0] is this binary.
		if load, ok := a.X.(*ssa.UnOp); ok && load.Op == token.MUL {
			if gl, ok := load.X.(*ssa.Global); ok && gl.Object() != nil && gl.Object().Pkg() != nil &&
				gl.Object().Pkg().Path()+"."+gl.Object().Name() == selfArgs {
				if c, ok := a.Index.(*ssa.Const); ok && c.Int64() == 0 {
					return []program{{kind: progSelf}}
				}
			}
		}
	}
	return []program{{kind: progOther}}
}

// display is fn's name with the module path left off.
func (g *graph) display(fn *ssa.Function) string {
	return g.short(fn.String())
}

// short is name with the module path left off.
func (g *graph) short(name string) string {
	return strings.ReplaceAll(name, g.module+"/", "")
}
