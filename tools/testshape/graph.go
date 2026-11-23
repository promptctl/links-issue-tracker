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

// program is what a spawn's program argument resolved to. Each kind decides
// its own touch, so no kind can go uncounted. [LAW:types-are-the-program]
type program interface {
	// touch is the spawn's touch; self is the package's spawn of its own
	// test binary.
	touch(self touch) touch
	// note is what the witness adds after the spawning call.
	note() []string
}

// gitProgram is git, named "git" or by a path ending in /git.
type gitProgram struct{ name string }

// namedProgram is any other program, named by a constant.
type namedProgram struct{ name string }

// selfProgram is this test binary (os.Executable, os.Args[0]).
type selfProgram struct{}

// unresolvedProgram is a program no constant names where it enters: a
// pointer fill, an escaped variable, a call's result, a spawner handed on as
// a value. It is another process, said so in its witness.
type unresolvedProgram struct{}

func (gitProgram) touch(touch) touch        { return touchGit }
func (namedProgram) touch(touch) touch      { return touchProc }
func (selfProgram) touch(self touch) touch  { return self }
func (unresolvedProgram) touch(touch) touch { return touchProc }
func (gitProgram) note() []string           { return nil }
func (namedProgram) note() []string         { return nil }
func (selfProgram) note() []string          { return nil }
func (unresolvedProgram) note() []string    { return []string{unresolvedNote} }

const unresolvedNote = "(program unresolved)"

// spawn is a process a function starts, its program resolved.
type spawn struct {
	program program
	tail    []string // the calls from the function to the spawning call
}

// entry is how a program enters a function from outside it.
type entry int

const (
	viaParam   entry = iota // a parameter, resolved where the function is called
	viaFreeVar              // a captured value, resolved where the closure is built
	// A captured variable's address, resolved where the closure is built from
	// what is stored at it: go/ssa captures a local by its address.
	viaCaptured
)

// slot is one parameter or free variable of a function.
type slot struct {
	via   entry
	index int
}

// hole is a spawn whose program enters through a slot: the function is a
// spawner, and each place that calls or builds it resolves the program.
type hole struct {
	slot
	tail []string
}

// edge is one way control can pass from a vertex. [LAW:types-are-the-program]
// an edge either enters a vertex the walk follows or records a touch it never
// follows; nothing can be both.
type edge interface{ isEdge() }

// follow enters a module function, or a package variable's initializer
// value. asValue: the function was taken as a value, not called.
type follow struct {
	to      ssa.Member
	asValue bool
}

// reachTouch is a marker: its touch is recorded where it is reached, and the
// walk goes no further.
type reachTouch struct {
	touch touch
	name  string
}

func (follow) isEdge()     {}
func (reachTouch) isEdge() {}

// callSite is a static call whose arguments can name a spawned program.
type callSite interface{ isCallSite() }

// spawnerCall is a call of an external spawner; program is the argument
// that names the program.
type spawnerCall struct {
	name    string
	program ssa.Value
}

// helperCall is a call of a module function, which resolves the programs its
// parameters carry.
type helperCall struct {
	callee *ssa.Function
	args   []ssa.Value
}

func (spawnerCall) isCallSite() {}
func (helperCall) isCallSite()  {}

// node is one vertex's facts: a module function's, read from its SSA once,
// or a package variable's, read from the values its initializer stores.
type node struct {
	// at is the function whose values the node's instructions are: the
	// function itself, or the initializer that stores the variable.
	at       *ssa.Function
	edges    []edge
	calls    []callSite
	closures []*ssa.MakeClosure
	cmdPaths []ssa.Value // values stored into an exec.Cmd's Path
	spawns   []spawn
	holes    []hole
}

// initStore is one store into a package variable made by its package's
// initializer.
type initStore struct {
	fn  *ssa.Function
	val ssa.Value
}

// graph is the module's vertices and their edges: computed once from the
// loaded program, then read by every walk. Nothing in it does I/O.
// [LAW:effects-at-boundaries]
type graph struct {
	module  string
	markers markerSet
	nodes   map[ssa.Member]*node
	inits   map[*ssa.Global][]initStore
}

// buildGraph reads every package initializer's stores, every seam variable's
// initializer value, and every module function, then resolves spawned
// programs through helpers until no new spawner appears.
func buildGraph(l *loaded, ms markerSet) *graph {
	g := &graph{module: l.module, markers: ms, nodes: make(map[ssa.Member]*node), inits: make(map[*ssa.Global][]initStore)}
	for _, p := range l.prog.AllPackages() {
		if inModule(g.module, p.Pkg.Path()) {
			g.readInitializer(p)
		}
	}
	var order []ssa.Member
	var fns []*ssa.Function
	for fn := range ssautil.AllFunctions(l.prog) {
		if g.walkable(fn) {
			fns = append(fns, fn)
		}
	}
	// A stable order, so every witness chain is the same on every run.
	sort.Slice(fns, func(i, j int) bool { return memberLess(fns[i], fns[j]) })
	for _, fn := range fns {
		g.nodes[fn] = nil
	}
	var globals []*ssa.Global
	for gl := range g.inits {
		if isSeam(gl) {
			globals = append(globals, gl)
		}
	}
	sort.Slice(globals, func(i, j int) bool { return memberLess(globals[i], globals[j]) })
	for _, gl := range globals {
		g.nodes[gl] = nil
	}
	// Reading a vertex can materialize a method wrapper nothing else named;
	// it is read in turn, so every edge's target has a node.
	enqueue := func(n *node) {
		for _, e := range n.edges {
			if f, ok := e.(follow); ok {
				if _, seen := g.nodes[f.to]; !seen {
					g.nodes[f.to] = nil
					fns = append(fns, f.to.(*ssa.Function))
				}
			}
		}
	}
	for _, gl := range globals {
		n := g.readValue(gl)
		g.nodes[gl] = n
		order = append(order, gl)
		enqueue(n)
	}
	for i := 0; i < len(fns); i++ {
		n := g.readFunc(fns[i])
		g.nodes[fns[i]] = n
		order = append(order, fns[i])
		enqueue(n)
	}
	g.resolveSpawns(order)
	return g
}

func memberLess(a, b ssa.Member) bool {
	if a.Pos() != b.Pos() {
		return a.Pos() < b.Pos()
	}
	return a.String() < b.String()
}

// readInitializer records every store into a package variable that p's
// initializer makes: the package's init function, which holds the
// variables' initializers, and the init functions it calls, which the
// package declares.
func (g *graph) readInitializer(p *ssa.Package) {
	init := p.Func("init")
	initFns := []*ssa.Function{init}
	for _, b := range init.Blocks {
		for _, instr := range b.Instrs {
			if c, ok := instr.(*ssa.Call); ok {
				if callee := c.Call.StaticCallee(); callee != nil && callee.Pkg == p {
					initFns = append(initFns, callee)
				}
			}
		}
	}
	for _, fn := range initFns {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				if st, ok := instr.(*ssa.Store); ok {
					if gl, ok := st.Addr.(*ssa.Global); ok {
						g.inits[gl] = append(g.inits[gl], initStore{fn: fn, val: st.Val})
					}
				}
			}
		}
	}
}

// isSeam reports whether loading gl can run code: it holds a function or an
// interface value.
func isSeam(gl *ssa.Global) bool {
	switch gl.Type().(*types.Pointer).Elem().Underlying().(type) {
	case *types.Signature, *types.Interface:
		return true
	}
	return false
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

// markerOf is the touch reaching fn makes, if fn is a marker: a listed
// function, an external spawner reached as a value, or a main package's main.
func (g *graph) markerOf(fn *ssa.Function) (reachTouch, bool) {
	name := fullName(fn)
	if t, ok := g.markers.touches[name]; ok {
		return reachTouch{t, name}, true
	}
	if _, ok := g.markers.spawners[name]; ok {
		return reachTouch{touchProc, name + " " + unresolvedNote}, true
	}
	if f, ok := fn.Object().(*types.Func); ok && f.Name() == "main" && f.Pkg() != nil &&
		f.Pkg().Name() == "main" && f.Parent() == f.Pkg().Scope() && inModule(g.module, f.Pkg().Path()) {
		return reachTouch{touchCLI, name}, true
	}
	return reachTouch{}, false
}

func (g *graph) isMarker(fn *ssa.Function) bool {
	_, ok := g.markerOf(fn)
	return ok
}

// addEdge adds n's edge to fn: its touch if fn is a marker, a follow if the
// walk enters it, and nothing for any other function outside the module.
func (g *graph) addEdge(n *node, fn *ssa.Function, asValue bool) {
	if m, ok := g.markerOf(fn); ok {
		n.edges = append(n.edges, m)
	} else if g.walkable(fn) {
		n.edges = append(n.edges, follow{fn, asValue})
	}
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

// onlyRead reports whether the closure mc only reads the variable at addr
// that it captures.
func onlyRead(mc *ssa.MakeClosure, addr ssa.Value) bool {
	fvs := mc.Fn.(*ssa.Function).FreeVars
	for i, b := range mc.Bindings {
		if b == addr && !onlyLoaded(fvs[i]) {
			return false
		}
	}
	return true
}

// onlyLoaded reports whether every use of the address v loads through it.
func onlyLoaded(v ssa.Value) bool {
	for _, r := range *v.Referrers() {
		switch r := r.(type) {
		case *ssa.UnOp:
			if r.Op != token.MUL {
				return false
			}
		case *ssa.DebugRef:
		default:
			return false
		}
	}
	return true
}

// readFunc is fn's node: every edge out of its body and the call sites that
// can name a spawned program.
func (g *graph) readFunc(fn *ssa.Function) *node {
	n := &node{at: fn}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			g.readInstr(n, instr)
		}
	}
	return n
}

// readValue is gl's node: what its package initializer stored in it, and
// the instructions that computed it, so `var now = makeClock()` reaches
// makeClock and whatever makeClock builds.
func (g *graph) readValue(gl *ssa.Global) *node {
	stores := g.inits[gl]
	n := &node{at: stores[0].fn}
	seen := make(map[ssa.Instruction]bool)
	var read func(instr ssa.Instruction)
	read = func(instr ssa.Instruction) {
		if seen[instr] {
			return
		}
		seen[instr] = true
		g.readInstr(n, instr)
		for _, op := range instr.Operands(nil) {
			if computed, ok := (*op).(ssa.Instruction); ok {
				read(computed)
			}
		}
	}
	for _, s := range stores {
		switch v := s.val.(type) {
		case *ssa.Function:
			g.addEdge(n, v, true)
		case ssa.Instruction:
			read(v)
		}
	}
	return n
}

// readInstr adds instr's edges and call sites to n.
func (g *graph) readInstr(n *node, instr ssa.Instruction) {
	var callee *ssa.Function
	if c, ok := instr.(ssa.CallInstruction); ok {
		common := c.Common()
		if common.IsInvoke() {
			// A call through an interface reaches no implementation; only an
			// interface method that is itself a marker counts.
			if t, ok := g.markers.touches[common.Method.FullName()]; ok {
				n.edges = append(n.edges, reachTouch{t, common.Method.FullName()})
			}
		} else if callee = common.StaticCallee(); callee != nil {
			name := fullName(callee)
			if p, ok := g.markers.spawners[name]; ok {
				n.calls = append(n.calls, spawnerCall{name, common.Args[p]})
			} else {
				g.addEdge(n, callee, false)
				if g.walkable(callee) {
					n.calls = append(n.calls, helperCall{callee, common.Args})
				}
			}
		}
	}
	switch instr := instr.(type) {
	case *ssa.MakeClosure:
		fn := instr.Fn.(*ssa.Function)
		g.addEdge(n, fn, !onlyCalled(instr))
		if g.walkable(fn) {
			n.closures = append(n.closures, instr)
		}
	case *ssa.MakeInterface:
		// Whoever converts a value to an interface carries every method the
		// value could then be called by.
		for _, m := range g.methods(instr.Parent().Prog, instr.X.Type()) {
			g.addEdge(n, m, true)
		}
	case *ssa.UnOp:
		if gl, ok := instr.X.(*ssa.Global); ok && instr.Op == token.MUL {
			if _, ok := g.nodes[gl]; ok {
				n.edges = append(n.edges, follow{to: gl})
			}
		}
	case *ssa.Store:
		if isCmdPath(instr.Addr) {
			n.cmdPaths = append(n.cmdPaths, instr.Val)
		}
	}
	// Every other function this one takes as a value.
	if _, isClosure := instr.(*ssa.MakeClosure); !isClosure {
		for _, op := range instr.Operands(nil) {
			if f, ok := (*op).(*ssa.Function); ok && f != callee {
				g.addEdge(n, f, true)
			}
		}
	}
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
// itself, and the vertex that calls or builds it resolves it in turn,
// through as many helpers as it takes.
func (g *graph) resolveSpawns(order []ssa.Member) {
	for changed := true; changed; {
		changed = false
		for _, v := range order {
			n := g.nodes[v]
			for _, f := range g.flows(n) {
				resolve := g.resolve
				if f.atAddr {
					resolve = g.resolveAddr
				}
				programs, slots := resolve(f.value, n.at, map[ssa.Value]bool{})
				for _, p := range programs {
					n.addSpawn(spawn{p, f.tail})
				}
				for _, s := range slots {
					changed = n.addHole(hole{s, f.tail}) || changed
				}
			}
		}
	}
}

// flow is one value that names a spawned program: tail is the calls from the
// vertex that holds the value to the spawn. atAddr: the value is the address
// of the variable that holds the program.
type flow struct {
	value  ssa.Value
	atAddr bool
	tail   []string
}

func (g *graph) flows(n *node) []flow {
	var fs []flow
	for _, c := range n.calls {
		switch c := c.(type) {
		case spawnerCall:
			fs = append(fs, flow{c.program, false, []string{c.name}})
		case helperCall:
			for _, h := range g.nodes[c.callee].holes {
				if h.via == viaParam {
					fs = append(fs, flow{c.args[h.index], false, append([]string{c.callee.String()}, h.tail...)})
				}
			}
		}
	}
	for _, mc := range n.closures {
		closure := mc.Fn.(*ssa.Function)
		for _, h := range g.nodes[closure].holes {
			if h.via != viaParam {
				fs = append(fs, flow{mc.Bindings[h.index], h.via == viaCaptured, append([]string{closure.String()}, h.tail...)})
			}
		}
	}
	for _, v := range n.cmdPaths {
		fs = append(fs, flow{v, false, []string{cmdType + "." + cmdPathField}})
	}
	return fs
}

func (n *node) addSpawn(s spawn) {
	if !slices.ContainsFunc(n.spawns, func(have spawn) bool { return have.program == s.program }) {
		n.spawns = append(n.spawns, s)
	}
}

// addHole reports whether n gained the hole, the one change that can
// resolve more spawns.
func (n *node) addHole(h hole) bool {
	if slices.ContainsFunc(n.holes, func(have hole) bool { return have.slot == h.slot }) {
		return false
	}
	n.holes = append(n.holes, h)
	return true
}

// resolve is what programs v names in fn, and the slots of fn it names them
// through. seen stops a cycle of phis.
func (g *graph) resolve(v ssa.Value, fn *ssa.Function, seen map[ssa.Value]bool) ([]program, []slot) {
	if seen[v] {
		return nil, nil
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			name := constant.StringVal(v.Value)
			if name == "git" || strings.HasSuffix(name, "/git") {
				return []program{gitProgram{name}}, nil
			}
			return []program{namedProgram{name}}, nil
		}
	case *ssa.Parameter:
		return nil, []slot{{viaParam, slices.Index(fn.Params, v)}}
	case *ssa.FreeVar:
		return nil, []slot{{viaFreeVar, slices.Index(fn.FreeVars, v)}}
	case *ssa.UnOp:
		if v.Op == token.MUL {
			return g.resolveAddr(v.X, fn, seen)
		}
	case *ssa.ChangeType:
		return g.resolve(v.X, fn, seen)
	case *ssa.Convert:
		return g.resolve(v.X, fn, seen)
	case *ssa.Phi:
		var ps []program
		var ss []slot
		for _, e := range v.Edges {
			p, s := g.resolve(e, fn, seen)
			ps, ss = append(ps, p...), append(ss, s...)
		}
		return ps, ss
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if callee := call.Call.StaticCallee(); callee != nil {
				switch fullName(callee) {
				case selfExecutable:
					return []program{selfProgram{}}, nil
				case lookPath:
					return g.resolve(call.Call.Args[0], fn, seen)
				}
			}
		}
	}
	return []program{unresolvedProgram{}}, nil
}

// resolveAddr is what programs the variable at addr holds in fn.
func (g *graph) resolveAddr(addr ssa.Value, fn *ssa.Function, seen map[ssa.Value]bool) ([]program, []slot) {
	switch a := addr.(type) {
	case *ssa.Alloc:
		// A local whose address never leaves fn, but to closures that only
		// read it, holds what fn stores in it; one whose address escapes can
		// be filled anywhere.
		var ps []program
		var ss []slot
		for _, r := range *a.Referrers() {
			switch r := r.(type) {
			case *ssa.Store:
				if r.Addr != a {
					return []program{unresolvedProgram{}}, nil
				}
				p, s := g.resolve(r.Val, fn, seen)
				ps, ss = append(ps, p...), append(ss, s...)
			case *ssa.MakeClosure:
				if !onlyRead(r, a) {
					return []program{unresolvedProgram{}}, nil
				}
			case *ssa.UnOp, *ssa.DebugRef:
			default:
				return []program{unresolvedProgram{}}, nil
			}
		}
		if len(ps)+len(ss) > 0 {
			return ps, ss
		}
	case *ssa.FreeVar:
		if onlyLoaded(a) {
			return nil, []slot{{viaCaptured, slices.Index(fn.FreeVars, a)}}
		}
	case *ssa.Global:
		// A package variable holds what its initializer stored.
		var ps []program
		for _, s := range g.inits[a] {
			p, _ := g.resolve(s.val, s.fn, seen)
			ps = append(ps, p...)
		}
		if len(ps) > 0 {
			return ps, nil
		}
	case *ssa.IndexAddr:
		// os.Args[0] is this binary.
		if load, ok := a.X.(*ssa.UnOp); ok && load.Op == token.MUL {
			if gl, ok := load.X.(*ssa.Global); ok && gl.Object() != nil && gl.Object().Pkg() != nil &&
				gl.Object().Pkg().Path()+"."+gl.Object().Name() == selfArgs {
				if c, ok := a.Index.(*ssa.Const); ok && c.Int64() == 0 {
					return []program{selfProgram{}}, nil
				}
			}
		}
	}
	return []program{unresolvedProgram{}}, nil
}
