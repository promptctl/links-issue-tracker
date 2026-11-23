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

// decl is one declaration of one package: its import path and its name
// within the package.
type decl struct{ pkg, name string }

// markerGroup is functions of one package whose call is one touch.
type markerGroup struct {
	pkg   string
	touch touch
	names []string
}

// spawner is one function that starts the program its argument names.
type spawner struct {
	fn      decl
	program int // the index of the argument naming the program
}

// markerSet is the marker data checked against the packages it names, every
// name full. Only resolveMarkers builds one.
type markerSet struct {
	module   string
	touches  map[string]touch
	spawners map[string]int
}

// program is what a spawn's program argument resolved to. Each kind decides
// its own touch, so no kind can go uncounted. [LAW:types-are-the-program]
type program interface {
	// touch is the spawn's touch; self is the package's spawn of its own
	// test binary.
	touch(self touch) touch
	// String is the program as its witness names it.
	String() string
}

// gitProgram is git, named so or by a path ending in /git.
type gitProgram struct{ name string }

// namedProgram is any other program, named by a constant.
type namedProgram struct{ name string }

// selfProgram is this test binary (selfExecutable, selfArgs[0]).
type selfProgram struct{}

// unresolvedProgram is a program no constant names where it enters: a
// pointer fill, an escaped variable, a call's result, a spawner handed on as
// a value. It is another process, said so in its witness.
type unresolvedProgram struct{}

func (gitProgram) touch(touch) touch        { return touchGit }
func (namedProgram) touch(touch) touch      { return touchProc }
func (selfProgram) touch(self touch) touch  { return self }
func (unresolvedProgram) touch(touch) touch { return touchProc }
func (p gitProgram) String() string         { return p.name }
func (p namedProgram) String() string       { return p.name }
func (selfProgram) String() string          { return "this test binary" }
func (unresolvedProgram) String() string    { return "program unresolved" }

// spawnTail is a spawn's calls with its program named on the spawning call,
// the last of them.
func spawnTail(tail []string, p program) []string {
	out := slices.Clone(tail)
	out[len(out)-1] += " (" + p.String() + ")"
	return out
}

// spawn is a process a vertex starts, its program resolved.
type spawn struct {
	program program
	tail    []string // the calls from the vertex to the spawning call
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

// follow enters a module function or a package variable. asValue: the
// function was taken as a value, not called.
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

// node is one vertex's facts, read once from its instructions: a module
// function's body, or the slice of the package initializer that computes a
// package variable.
type node struct {
	edges    []edge
	calls    []callSite
	closures []*ssa.MakeClosure
	cmdPaths []ssa.Value // values stored into an exec.Cmd's Path
	spawns   []spawn
	holes    []hole
}

// graph is the module's vertices and their edges: computed once from the
// loaded program, then read by every walk. Nothing in it does I/O.
// [LAW:effects-at-boundaries]
type graph struct {
	module  string
	markers markerSet
	nodes   map[ssa.Member]*node
	// held is, per package variable, the values its initializer stores into
	// it whole.
	held map[*ssa.Global][]ssa.Value
}

// buildGraph reads every module function and every package variable's
// initializer slice, then resolves spawned programs through helpers until no
// new spawner appears.
func buildGraph(l *loaded, ms markerSet) *graph {
	g := &graph{module: l.module, markers: ms, nodes: make(map[ssa.Member]*node), held: make(map[*ssa.Global][]ssa.Value)}
	var fns []*ssa.Function
	for fn := range ssautil.AllFunctions(l.prog) {
		if g.walkable(fn) {
			fns = append(fns, fn)
		}
	}
	// A stable order, so every witness chain is the same on every run.
	sort.Slice(fns, func(i, j int) bool { return memberLess(fns[i], fns[j]) })
	initSlices := g.readInitializers(l.prog, writers(fns))
	var globals []*ssa.Global
	for gl := range initSlices {
		globals = append(globals, gl)
	}
	sort.Slice(globals, func(i, j int) bool { return memberLess(globals[i], globals[j]) })
	for _, gl := range globals {
		g.nodes[gl] = nil
	}
	for _, fn := range fns {
		g.nodes[fn] = nil
	}
	var order []ssa.Member
	// Reading a vertex can materialize a method wrapper nothing else named;
	// it is read in turn, so every edge's target has a node.
	read := func(v ssa.Member, instrs []ssa.Instruction) {
		n := &node{}
		for _, instr := range instrs {
			g.readInstr(n, instr)
		}
		g.nodes[v] = n
		order = append(order, v)
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
		read(gl, initSlices[gl])
	}
	for i := 0; i < len(fns); i++ {
		read(fns[i], body(fns[i]))
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

// body is fn's instructions, in block order.
func body(fn *ssa.Function) []ssa.Instruction {
	var instrs []ssa.Instruction
	for _, b := range fn.Blocks {
		instrs = append(instrs, b.Instrs...)
	}
	return instrs
}

// rootOf is the variable or allocation whose memory v reaches into: through
// field and element addresses, slicing, and loads of the reference a
// variable holds.
func rootOf(v ssa.Value) ssa.Value {
	for {
		switch x := v.(type) {
		case *ssa.FieldAddr:
			v = x.X
		case *ssa.IndexAddr:
			v = x.X
		case *ssa.Slice:
			v = x.X
		case *ssa.UnOp:
			if x.Op != token.MUL {
				return v
			}
			v = x.X
		default:
			return v
		}
	}
}

// writesInto are, per variable or allocation, the instructions of instrs
// that can write into its memory: a store or map update through it, and a
// call handed a reference into it, which can fill it.
func writesInto(instrs []ssa.Instruction) map[ssa.Value][]ssa.Instruction {
	w := make(map[ssa.Value][]ssa.Instruction)
	for _, instr := range instrs {
		switch instr := instr.(type) {
		case *ssa.Store:
			w[rootOf(instr.Addr)] = append(w[rootOf(instr.Addr)], instr)
		case *ssa.MapUpdate:
			w[rootOf(instr.Map)] = append(w[rootOf(instr.Map)], instr)
		case ssa.CallInstruction:
			for _, a := range instr.Common().Args {
				if refersInto(a.Type()) {
					w[rootOf(a)] = append(w[rootOf(a)], instr)
				}
			}
		}
	}
	return w
}

// refersInto reports whether a value of t refers to memory it does not
// hold: a pointer, map, slice or channel, through which a callee writes.
func refersInto(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Slice, *types.Chan:
		return true
	}
	return false
}

// writers are, per module function, the package variables it writes or a
// function it statically calls writes, through as many calls as it takes.
func writers(fns []*ssa.Function) map[*ssa.Function]map[*ssa.Global]bool {
	ws := make(map[*ssa.Function]map[*ssa.Global]bool)
	callees := make(map[*ssa.Function][]*ssa.Function)
	for _, fn := range fns {
		instrs := body(fn)
		ws[fn] = make(map[*ssa.Global]bool)
		for v := range writesInto(instrs) {
			if gl, ok := v.(*ssa.Global); ok {
				ws[fn][gl] = true
			}
		}
		for _, instr := range instrs {
			if c, ok := instr.(ssa.CallInstruction); ok {
				if callee := c.Common().StaticCallee(); callee != nil {
					callees[fn] = append(callees[fn], callee)
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range fns {
			for _, callee := range callees[fn] {
				for gl := range ws[callee] {
					if !ws[fn][gl] {
						ws[fn][gl], changed = true, true
					}
				}
			}
		}
	}
	return ws
}

// declares reports whether the module declares the package variable gl.
func (g *graph) declares(gl *ssa.Global) bool { return inModule(g.module, gl.Pkg.Pkg.Path()) }

// isPackageInit reports whether fn is a package's initializer.
func isPackageInit(fn *ssa.Function) bool {
	return fn.Pkg != nil && fn == fn.Pkg.Func("init")
}

// readInitializers is the one reader of what the module's package
// initializers store. For each package variable it is the slice of an
// initializer that computes the variable: the writes into it, every value
// they depend on, followed back through operands, and every write into an
// allocation in the slice. A write is a store or map update through the
// variable, a call handed a reference into it, or a call of a function that
// writes it. Only the module's variables have slices.
// The slice over-reaches rather than falls short: what it calls or takes as
// a value is reachable from every load of the variable, as the code that
// made the value. Another package's initializer, which an initializer calls,
// is that package's, and is read as its own.
func (g *graph) readInitializers(prog *ssa.Program, ws map[*ssa.Function]map[*ssa.Global]bool) map[*ssa.Global][]ssa.Instruction {
	var inits []*ssa.Function
	for _, p := range prog.AllPackages() {
		if inModule(g.module, p.Pkg.Path()) {
			inits = append(inits, p.Func("init"))
		}
	}
	sort.SliceStable(inits, func(i, j int) bool { return inits[i].Pkg.Pkg.Path() < inits[j].Pkg.Pkg.Path() })
	out := make(map[*ssa.Global][]ssa.Instruction)
	for _, init := range inits {
		instrs := body(init)
		w := writesInto(instrs)
		roots := make(map[*ssa.Global][]ssa.Instruction)
		for v, writes := range w {
			if gl, ok := v.(*ssa.Global); ok && g.declares(gl) {
				roots[gl] = append(roots[gl], writes...)
			}
		}
		for _, instr := range instrs {
			if st, ok := instr.(*ssa.Store); ok {
				if gl, ok := st.Addr.(*ssa.Global); ok {
					g.held[gl] = append(g.held[gl], st.Val)
				}
			}
			c, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			if callee := c.Common().StaticCallee(); callee != nil && !isPackageInit(callee) {
				for gl := range ws[callee] {
					if g.declares(gl) {
						roots[gl] = append(roots[gl], instr)
					}
				}
			}
		}
		for gl, rs := range roots {
			in := make(map[ssa.Instruction]bool)
			var add func(ssa.Instruction)
			add = func(instr ssa.Instruction) {
				if in[instr] {
					return
				}
				in[instr] = true
				for _, op := range instr.Operands(nil) {
					if v, ok := (*op).(ssa.Instruction); ok {
						add(v)
					}
				}
				switch a := instr.(type) {
				case *ssa.Alloc, *ssa.MakeMap, *ssa.MakeSlice, *ssa.MakeChan:
					for _, wr := range w[a.(ssa.Value)] {
						add(wr)
					}
				}
			}
			for _, r := range rs {
				add(r)
			}
			// In the initializer's order, so the node reads the same on every
			// run.
			for _, instr := range instrs {
				if in[instr] {
					out[gl] = append(out[gl], instr)
				}
			}
		}
	}
	return out
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
// function, an external spawner reached as a value, or the main function of
// a module main package.
func (g *graph) markerOf(fn *ssa.Function) (reachTouch, bool) {
	name := fullName(fn)
	if t, ok := g.markers.touches[name]; ok {
		return reachTouch{t, name}, true
	}
	if _, ok := g.markers.spawners[name]; ok {
		return reachTouch{touchProc, spawnTail([]string{name}, unresolvedProgram{})[0]}, true
	}
	if f, ok := fn.Object().(*types.Func); ok && f.Name() == "main" && f.Pkg() != nil &&
		f.Pkg().Name() == "main" && f.Parent() == f.Pkg().Scope() && inModule(g.module, f.Pkg().Path()) {
		return reachTouch{mainTouch, name}, true
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
		// value could then be called by: a module type's, walked, and any
		// type's markers.
		for _, m := range methods(instr.Parent().Prog, instr.X.Type()) {
			g.addEdge(n, m, true)
		}
	case *ssa.Store:
		if g.isCmdPath(instr.Addr) {
			n.cmdPaths = append(n.cmdPaths, instr.Val)
		}
	}
	for _, op := range instr.Operands(nil) {
		switch v := (*op).(type) {
		case *ssa.Function:
			// Every other function this one takes as a value.
			if _, isClosure := instr.(*ssa.MakeClosure); !isClosure && v != callee {
				g.addEdge(n, v, true)
			}
		case *ssa.Global:
			// A use of a package variable reaches its initializer's slice;
			// storing into the variable whole does not read it.
			if st, ok := instr.(*ssa.Store); ok && op == &st.Addr {
				continue
			}
			if _, ok := g.nodes[v]; ok {
				n.edges = append(n.edges, follow{to: v})
			}
		}
	}
}

// methods are the methods in t's method set: those a value of t converted
// to an interface can be called by.
func methods(prog *ssa.Program, t types.Type) []*ssa.Function {
	mset := prog.MethodSets.MethodSet(t)
	var fns []*ssa.Function
	for i := range mset.Len() {
		if f := prog.MethodValue(mset.At(i)); f != nil {
			fns = append(fns, f)
		}
	}
	return fns
}

// isCmdPath reports whether addr is the address of the field cmdPath names.
func (g *graph) isCmdPath(addr ssa.Value) bool {
	fa, ok := addr.(*ssa.FieldAddr)
	if !ok {
		return false
	}
	st, ok := fa.X.Type().Underlying().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := st.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	field := named.Underlying().(*types.Struct).Field(fa.Field)
	return (decl{named.Obj().Pkg().Path(), named.Obj().Name() + "." + field.Name()}) == cmdPath
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
				programs, slots := resolve(f.value, map[ssa.Value]bool{})
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
		fs = append(fs, flow{v, false, []string{cmdPath.String()}})
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

// resolve is what programs v names, and the slots of its function it names
// them through. seen stops a cycle of phis.
func (g *graph) resolve(v ssa.Value, seen map[ssa.Value]bool) ([]program, []slot) {
	if seen[v] {
		return nil, nil
	}
	seen[v] = true
	switch v := v.(type) {
	case *ssa.Const:
		if v.Value != nil && v.Value.Kind() == constant.String {
			name := constant.StringVal(v.Value)
			if name == gitName || strings.HasSuffix(name, "/"+gitName) {
				return []program{gitProgram{name}}, nil
			}
			return []program{namedProgram{name}}, nil
		}
	case *ssa.Parameter:
		return nil, []slot{{viaParam, slices.Index(v.Parent().Params, v)}}
	case *ssa.FreeVar:
		return nil, []slot{{viaFreeVar, slices.Index(v.Parent().FreeVars, v)}}
	case *ssa.UnOp:
		if v.Op == token.MUL {
			return g.resolveAddr(v.X, seen)
		}
	case *ssa.ChangeType:
		return g.resolve(v.X, seen)
	case *ssa.Convert:
		return g.resolve(v.X, seen)
	case *ssa.Phi:
		var ps []program
		var ss []slot
		for _, e := range v.Edges {
			p, s := g.resolve(e, seen)
			ps, ss = append(ps, p...), append(ss, s...)
		}
		return ps, ss
	case *ssa.Extract:
		if call, ok := v.Tuple.(*ssa.Call); ok && v.Index == 0 {
			if callee := call.Call.StaticCallee(); callee != nil {
				switch fullName(callee) {
				case selfExecutable.String():
					return []program{selfProgram{}}, nil
				case lookPath.String():
					return g.resolve(call.Call.Args[0], seen)
				}
			}
		}
	}
	return []program{unresolvedProgram{}}, nil
}

// resolveAddr is what programs the variable at addr holds.
func (g *graph) resolveAddr(addr ssa.Value, seen map[ssa.Value]bool) ([]program, []slot) {
	switch a := addr.(type) {
	case *ssa.Alloc:
		// A local whose address never leaves its function, but to closures
		// that only read it, holds what the function stores in it; one whose
		// address escapes can be filled anywhere.
		var ps []program
		var ss []slot
		for _, r := range *a.Referrers() {
			switch r := r.(type) {
			case *ssa.Store:
				if r.Addr != a {
					return []program{unresolvedProgram{}}, nil
				}
				p, s := g.resolve(r.Val, seen)
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
			return nil, []slot{{viaCaptured, slices.Index(a.Parent().FreeVars, a)}}
		}
	case *ssa.Global:
		// A package variable holds what its initializer stored in it whole.
		var ps []program
		for _, v := range g.held[a] {
			p, _ := g.resolve(v, seen)
			ps = append(ps, p...)
		}
		if len(ps) > 0 {
			return ps, nil
		}
	case *ssa.IndexAddr:
		// selfArgs[0] is this binary.
		if load, ok := a.X.(*ssa.UnOp); ok && load.Op == token.MUL {
			if gl, ok := load.X.(*ssa.Global); ok && gl.Object() != nil && gl.Object().Pkg() != nil &&
				(decl{gl.Object().Pkg().Path(), gl.Object().Name()}) == selfArgs {
				if c, ok := a.Index.(*ssa.Const); ok && c.Int64() == 0 {
					return []program{selfProgram{}}, nil
				}
			}
		}
	}
	return []program{unresolvedProgram{}}, nil
}
