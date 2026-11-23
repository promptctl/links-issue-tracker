package main

import (
	"fmt"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// analyzer decides reach over one module's SSA program. It holds what every
// walk shares and no walk changes: each function's summary and each type's
// structure. [LAW:one-source-of-truth] the rules live in the walker; these
// caches only remember what the walker already derived.
type analyzer struct {
	prog  *ssa.Program
	mod   module
	class *Classification

	summaries  map[*ssa.Function]*summary
	structures typeutil.Map // types.Type -> *structure
	methods    typeutil.Map // types.Type -> []*ssa.Function
	owners     map[*ssa.Function]owner

	// unclassified: every outside function module code reaches that the
	// classification does not cover, with the first function seen reaching
	// it.
	unclassified map[string]string
	// bodiless: module functions with no Go body to walk.
	bodiless map[string]string
}

func newAnalyzer(prog *ssa.Program, mod module, class *Classification) *analyzer {
	return &analyzer{
		prog:         prog,
		mod:          mod,
		class:        class,
		summaries:    map[*ssa.Function]*summary{},
		owners:       map[*ssa.Function]owner{},
		unclassified: map[string]string{},
		bodiless:     map[string]string{},
	}
}

// owner is where a function's code lives: the package and the key the
// classification names it by ("Name" or "Type.Method"), and whether the
// module defines it. A closure belongs to its enclosing function; an
// instance of a generic function to its origin; a method, or a wrapper that
// calls one, to its receiver's type. A wrapper around an interface method is
// module glue: its body is an interface call, which the walk dispatches.
type owner struct {
	pkg, key string
	inModule bool
	// class is the classification of an outside function; classified is
	// false when the classification does not cover it.
	class      Class
	classified bool
}

// owner is derived once per function: every walk asks it of every function
// it reaches.
func (a *analyzer) owner(fn *ssa.Function) owner {
	if o, ok := a.owners[fn]; ok {
		return o
	}
	o := a.deriveOwner(fn)
	if !o.inModule {
		o.class, o.classified = a.class.Lookup(o.pkg, o.key)
	}
	a.owners[fn] = o
	return o
}

func (a *analyzer) deriveOwner(fn *ssa.Function) owner {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if o := fn.Origin(); o != nil {
		fn = o
	}
	if recv := fn.Signature.Recv(); recv != nil && fn.Synthetic != "" {
		// A promotion wrapper: the method belongs to the outer type.
		return a.methodOwner(recv.Type(), fn.Name())
	}
	if obj, ok := fn.Object().(*types.Func); ok && obj != nil {
		if recv := obj.Signature().Recv(); recv != nil {
			if types.IsInterface(recv.Type()) {
				return owner{inModule: true}
			}
			return a.methodOwner(recv.Type(), obj.Name())
		}
		if obj.Pkg() != nil {
			return a.ownerIn(obj.Pkg().Path(), obj.Name())
		}
	}
	return a.ownerIn(fn.Pkg.Pkg.Path(), fn.Name())
}

func (a *analyzer) methodOwner(recv types.Type, method string) owner {
	named := namedOf(recv)
	if named == nil || named.Obj().Pkg() == nil {
		return owner{inModule: true}
	}
	return a.ownerIn(named.Obj().Pkg().Path(), named.Obj().Name()+"."+method)
}

func (a *analyzer) ownerIn(pkg, key string) owner {
	return owner{pkg: pkg, key: key, inModule: a.mod.contains(pkg)}
}

func namedOf(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
}

// summary is what one function's body does, in the terms the rules use. It
// is the same for every walk, so it is derived once per function.
type summary struct {
	static      []callSite
	taken       []*ssa.Function
	converted   []conversion
	dynamic     []*types.Signature
	invokes     []invoke
	spawnFields []spawnField
}

type callSite struct {
	caller *ssa.Function
	common *ssa.CallCommon
}

// conversion is a value of a concrete type made into an interface value.
type conversion struct {
	typ types.Type // the concrete type
	to  types.Type // the interface type
}

// invoke is a call of an interface method.
type invoke struct {
	recv   types.Type // the interface type the call is made through
	iface  *types.Interface
	method *types.Func
}

// spawnField is a program stored into a spawner's field (exec.Cmd's Path):
// the one way a spawn's program arrives other than as an argument. The store
// is read only to find the spawn and its program, never to decide reach.
type spawnField struct {
	spawner string
	program ssa.Value
}

func (a *analyzer) summary(fn *ssa.Function) *summary {
	if s, ok := a.summaries[fn]; ok {
		return s
	}
	s := &summary{}
	a.summaries[fn] = s
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			// [LAW:dataflow-not-control-flow] a value with no use creates
			// nothing: `var _ I = (*T)(nil)` converts and is never read.
			if v, ok := instr.(ssa.Value); ok && v.Referrers() != nil && len(*v.Referrers()) == 0 {
				if _, isCall := instr.(*ssa.Call); !isCall {
					continue
				}
			}
			var callee *ssa.Value
			if c, ok := instr.(ssa.CallInstruction); ok {
				common := c.Common()
				switch {
				case common.IsInvoke():
					recv := common.Value.Type()
					s.invokes = append(s.invokes, invoke{recv, recv.Underlying().(*types.Interface), common.Method})
				case common.StaticCallee() != nil:
					s.static = append(s.static, callSite{fn, common})
					callee = &common.Value
				default:
					if _, builtin := common.Value.(*ssa.Builtin); !builtin {
						s.dynamic = append(s.dynamic, common.Signature())
					}
				}
			}
			for _, op := range instr.Operands(nil) {
				if f, ok := (*op).(*ssa.Function); ok && op != callee {
					s.taken = append(s.taken, f)
				}
			}
			switch v := instr.(type) {
			case *ssa.MakeInterface:
				s.converted = append(s.converted, conversion{v.X.Type(), v.Type()})
			case *ssa.Store:
				if sf, ok := a.spawnFieldOf(v); ok {
					s.spawnFields = append(s.spawnFields, sf)
				}
			}
		}
	}
	return s
}

// spawnFieldOf is the spawn a store makes, if it stores into a field the
// classification names a spawner's program.
func (a *analyzer) spawnFieldOf(st *ssa.Store) (spawnField, bool) {
	fa, ok := st.Addr.(*ssa.FieldAddr)
	if !ok {
		return spawnField{}, false
	}
	named := namedOf(fa.X.Type())
	if named == nil || named.Obj().Pkg() == nil {
		return spawnField{}, false
	}
	strct, ok := named.Underlying().(*types.Struct)
	if !ok {
		return spawnField{}, false
	}
	pkg, typ, field := named.Obj().Pkg().Path(), named.Obj().Name(), strct.Field(fa.Field).Name()
	if !a.class.fieldSpawn(pkg, typ, field) {
		return spawnField{}, false
	}
	return spawnField{spawner: pkg + "." + typ + "." + field, program: st.Val}, true
}

// moduleType reports whether t, or what t points to, is a named type the
// module declares.
func (a *analyzer) moduleType(t types.Type) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && a.mod.contains(n.Obj().Pkg().Path())
}

// outsideType reports whether t, or what t points to, is a named concrete
// type declared outside the module.
func (a *analyzer) outsideType(t types.Type) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && !a.mod.contains(n.Obj().Pkg().Path()) && !types.IsInterface(n)
}

// moduleInterface reports whether the interface type t belongs to the
// module: a named interface the module declares, or an interface literal
// that declares a method of the module's.
func (a *analyzer) moduleInterface(t types.Type) bool {
	if n, ok := types.Unalias(t).(*types.Named); ok {
		return n.Obj().Pkg() != nil && a.mod.contains(n.Obj().Pkg().Path())
	}
	iface := t.Underlying().(*types.Interface)
	for i := range iface.NumExplicitMethods() {
		if p := iface.ExplicitMethod(i).Pkg(); p != nil && a.mod.contains(p.Path()) {
			return true
		}
	}
	return false
}

// structure is what a converted value's structure contributes: every method
// of each module type among the type, its fields, elements and pointees;
// each outside named type among them; and each interface type among them.
type structure struct {
	methods []*ssa.Function
	outside []types.Type
	ifaces  []types.Type
}

// structureOf is the structure of t, derived once per type.
func (a *analyzer) structureOf(t types.Type) *structure {
	if s, ok := a.structures.At(t).(*structure); ok {
		return s
	}
	s := &structure{}
	a.structures.Set(t, s)
	var seen typeutil.Map
	var visit func(types.Type)
	visit = func(t types.Type) {
		t = types.Unalias(t)
		if seen.At(t) != nil {
			return
		}
		seen.Set(t, true)
		switch u := t.(type) {
		case *types.Named:
			if u.TypeParams().Len() > 0 && u.TypeArgs().Len() == 0 {
				return
			}
			if types.IsInterface(u) {
				s.ifaces = append(s.ifaces, u)
				return
			}
			if a.moduleType(u) {
				s.methods = append(s.methods, a.methodsOf(u)...)
			} else {
				// Its pointer's method set, so a method on either receiver
				// counts: this approximation adds methods, never drops one.
				s.outside = append(s.outside, types.NewPointer(u))
			}
			visit(u.Underlying())
		case *types.Pointer:
			visit(u.Elem())
		case *types.Slice:
			visit(u.Elem())
		case *types.Array:
			visit(u.Elem())
		case *types.Chan:
			visit(u.Elem())
		case *types.Map:
			visit(u.Key())
			visit(u.Elem())
		case *types.Struct:
			for i := range u.NumFields() {
				visit(u.Field(i).Type())
			}
		case *types.Interface:
			s.ifaces = append(s.ifaces, u)
		}
	}
	visit(t)
	return s
}

// methodsOf is every method of a concrete type, value and pointer receivers
// alike, promoted ones included.
func (a *analyzer) methodsOf(t types.Type) []*ssa.Function {
	if fns, ok := a.methods.At(t).([]*ssa.Function); ok {
		return fns
	}
	fns := a.deriveMethods(t)
	a.methods.Set(t, fns)
	return fns
}

// A value method is taken from the value type's method set, so it is the
// declared method rather than the pointer wrapper that calls it, and a
// witness through it names the method the source declares.
func (a *analyzer) deriveMethods(t types.Type) []*ssa.Function {
	if types.IsInterface(t) {
		return []*ssa.Function{}
	}
	base := t
	if p, isPtr := types.Unalias(t).(*types.Pointer); isPtr {
		base = p.Elem()
	}
	values := a.prog.MethodSets.MethodSet(base)
	pointers := a.prog.MethodSets.MethodSet(types.NewPointer(base))
	fns := []*ssa.Function{}
	for i := range pointers.Len() {
		sel := pointers.At(i)
		if v := values.Lookup(sel.Obj().Pkg(), sel.Obj().Name()); v != nil {
			sel = v
		}
		if fn := a.prog.MethodValue(sel); fn != nil {
			fns = append(fns, fn)
		}
	}
	return fns
}

// pool is the binary level of one test binary: every function value and
// every concrete named type that may sit in an interface value in the reach
// of the initialization of the binary's packages and of its TestMain, each
// with its origin. These are the possible targets of a test's dynamic and
// interface calls.
type pool struct {
	funcs []poolEntry[*ssa.Function]
	types []poolEntry[types.Type]
	bySig typeutil.Map // normalized signature -> []int into funcs
	known map[*ssa.Function]bool
	typed typeutil.Map // types.Type -> bool
	// implementers caches, per interface, the pool types implementing it;
	// valid only once the pool is final.
	implementers typeutil.Map
	final        bool
}

type poolEntry[T any] struct {
	value   T
	origin  string // the initializer, TestMain or test that made it exist
	creator string // the function in whose body it was made
}

func newPool() *pool { return &pool{known: map[*ssa.Function]bool{}} }

// describe is how a walk reached something through this entry.
func (e poolEntry[T]) describe(what string, typ types.Type) via {
	return via{what: what, typ: typ, origin: e.origin, creator: e.creator}
}

// via is how a walk reached a function other than by a static call. It is
// spelled out only when a witness is printed: a walk reaches far more
// functions than it reports. [LAW:carrying-cost] building every via's text
// eagerly cost more than the rest of the walk.
type via struct {
	what string     // "takes it as a value"; with typ, a format naming it
	typ  types.Type // the type the reach went through, if any
	// origin and creator: the binary-level entry the reach went through.
	origin, creator string
}

func (v via) text(m module) string {
	t := v.what
	if v.typ != nil {
		t = fmt.Sprintf(v.what, types.TypeString(v.typ, relativeTo(m)))
	}
	if v.origin != "" {
		t += " from " + v.origin + ", made in " + v.creator
	}
	return t
}

func normalize(sig *types.Signature) *types.Signature {
	return types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())
}

func (p *pool) withSignature(sig *types.Signature) []poolEntry[*ssa.Function] {
	idx, _ := p.bySig.At(normalize(sig)).([]int)
	out := make([]poolEntry[*ssa.Function], len(idx))
	for i, j := range idx {
		out[i] = p.funcs[j]
	}
	return out
}

func (p *pool) implementing(iface *types.Interface) []poolEntry[types.Type] {
	if p.final {
		if cached, ok := p.implementers.At(iface).([]poolEntry[types.Type]); ok {
			return cached
		}
	}
	out := implementing(p.types, iface)
	if p.final {
		p.implementers.Set(iface, out)
	}
	return out
}

func implementing(entries []poolEntry[types.Type], iface *types.Interface) []poolEntry[types.Type] {
	var out []poolEntry[types.Type]
	for _, e := range entries {
		if types.Implements(e.value, iface) {
			out = append(out, e)
		}
	}
	return out
}

// step is how a walk first reached a function.
type step struct {
	from *ssa.Function
	via  via // zero for a static call
}

// witness is one chain of functions from a walk's root to a touch.
type witness struct {
	chain []string
	what  string // what made the touch, at the chain's end
}

func (w witness) String() string {
	return strings.Join(w.chain, " → ") + " ⇒ " + w.what
}

// walker is one walk: from one test, or, collecting, from the binary's
// initialization and TestMain.
type walker struct {
	a    *analyzer
	pool *pool
	// collect: this walk builds the pool — every value it takes and type
	// it converts joins the binary level, and a dispatch it reaches also
	// covers values that join later.
	collect bool

	visited map[*ssa.Function]step
	origin  map[*ssa.Function]string // the seed each function was reached from
	queue   []*ssa.Function
	touches map[Touch]witness

	sitesInto map[*ssa.Function][]callSite
	spawns    []spawnSite

	// invokes: the module-interface calls the walk reached, applied again
	// to every outside type that may sit in an interface value after them.
	invokes []pendingInvoke
	// local, outside a collecting walk: the outside named types this walk
	// put in an interface value, which the pool does not hold.
	local      []poolEntry[types.Type]
	localTyped typeutil.Map
	// invoked: the interface calls the walk has made. A second call of the
	// same method through the same interface reaches nothing the first did
	// not: a type that may sit in an interface later meets it in invokes.
	invoked map[invokeKey]bool
	// outsideCalls: the outside interface methods the walk has charged.
	outsideCalls map[string]bool

	// Dispatches reached while collecting, applied again to every value
	// that joins the pool after them.
	dynSigs       typeutil.Map // normalized signature -> *ssa.Function (first caller)
	structIfaces  []pendingInvoke
	dispatchingBy *ssa.Function
	converted     typeutil.Map
	// dispatched: the walk has reached a dispatching function. Once the
	// pool is final, a second dispatch reaches nothing the first did not.
	dispatched bool
}

type invokeKey struct {
	iface  *types.Interface
	method *types.Func
}

type pendingInvoke struct {
	inv  invoke
	from *ssa.Function
}

type spawnSite struct {
	in       *ssa.Function
	spawner  string // the spawner's name
	program  ssa.Value
	resolved bool // false: the spawner was reached as a value, its program unknown
}

func newWalker(a *analyzer, p *pool, collect bool) *walker {
	return &walker{
		a:            a,
		pool:         p,
		collect:      collect,
		visited:      map[*ssa.Function]step{},
		origin:       map[*ssa.Function]string{},
		touches:      map[Touch]witness{},
		sitesInto:    map[*ssa.Function][]callSite{},
		invoked:      map[invokeKey]bool{},
		outsideCalls: map[string]bool{},
	}
}

// root starts the walk at fn, made to run by origin.
func (w *walker) root(fn *ssa.Function, origin string) {
	if _, ok := w.visited[fn]; ok {
		return
	}
	w.visited[fn] = step{}
	w.origin[fn] = origin
	w.queue = append(w.queue, fn)
}

// run walks until nothing new is reachable.
func (w *walker) run() {
	for len(w.queue) > 0 {
		fn := w.queue[0]
		w.queue = w.queue[1:]
		w.visit(fn)
	}
}

func (w *walker) visit(fn *ssa.Function) {
	if fn.Blocks == nil {
		w.a.bodiless[w.a.name(fn)] = w.a.name(w.visited[fn].from)
		return
	}
	s := w.a.summary(fn)
	for _, site := range s.static {
		callee := site.common.StaticCallee()
		o := w.a.owner(callee)
		if !o.inModule && callee.Synthetic == "package initializer" {
			// A package's initializer calls those of its imports; an
			// outside package's initialization is outside code the module
			// does not call, so it is neither walked nor classified.
			continue
		}
		if !o.inModule {
			if o.classified && o.class.Spawn != nil && !o.class.Spawn.Field {
				arg := o.class.Spawn.Arg
				if callee.Signature.Recv() != nil {
					arg++
				}
				w.spawns = append(w.spawns, spawnSite{in: fn, spawner: o.pkg + "." + o.key, program: site.common.Args[arg], resolved: true})
			}
		} else {
			w.sitesInto[callee] = append(w.sitesInto[callee], site)
		}
		w.reach(callee, fn, via{}, true)
	}
	for _, f := range s.taken {
		w.take(f, fn)
	}
	for _, c := range s.converted {
		w.convert(c.typ, c.to, fn, via{})
	}
	for _, sig := range s.dynamic {
		w.dynamic(sig, fn)
	}
	for _, inv := range s.invokes {
		w.invoke(inv, fn)
	}
	for _, sf := range s.spawnFields {
		w.spawns = append(w.spawns, spawnSite{in: fn, spawner: sf.spawner, program: sf.program, resolved: true})
	}
}

// reach makes fn reachable from from. Module code joins the walk; code
// outside the module is never walked, only classified.
func (w *walker) reach(fn, from *ssa.Function, how via, static bool) {
	o := w.a.owner(fn)
	if !o.inModule && o.classified && o.class.Spawn != nil && !static {
		w.spawns = append(w.spawns, spawnSite{in: from, spawner: o.pkg + "." + o.key})
	}
	if _, ok := w.visited[fn]; ok {
		return
	}
	w.visited[fn] = step{from: from, via: how}
	w.origin[fn] = w.origin[from]
	if o.inModule {
		w.queue = append(w.queue, fn)
		return
	}
	w.classified(o, fn, from)
}

// classified charges what the classification says an outside function or
// interface method can do, called at at, whose caller is from.
func (w *walker) classified(o owner, at, from *ssa.Function) {
	if !o.classified {
		name := o.pkg + "." + o.key
		if _, seen := w.a.unclassified[name]; !seen {
			w.a.unclassified[name] = w.a.name(from)
		}
		return
	}
	for t := range o.class.Touches.Each {
		w.touch(t, at, func() string { return fmt.Sprintf("%s.%s (%s)", o.pkg, o.key, o.class.Touches) })
	}
	if o.class.Dispatching {
		w.dispatchAll(at)
	}
}

// touch records t, reached at fn, with its witness if it is t's first;
// what names the touch, and is spelled only for a first.
func (w *walker) touch(t Touch, at *ssa.Function, what func() string) {
	if _, ok := w.touches[t]; ok {
		return
	}
	w.touches[t] = witness{chain: w.chain(at), what: what()}
}

// chain is the path the walk took from its root to fn.
func (w *walker) chain(fn *ssa.Function) []string {
	var rev []string
	for fn != nil {
		st := w.visited[fn]
		name := w.a.name(fn)
		if st.via.what != "" {
			name += " [" + st.via.text(w.a.mod) + "]"
		}
		rev = append(rev, name)
		fn = st.from
	}
	out := make([]string, len(rev))
	for i, s := range rev {
		out[len(rev)-1-i] = s
	}
	return out
}

// take: a function value taken in the walk's reach is reachable, and, while
// collecting, joins the binary level.
func (w *walker) take(f, from *ssa.Function) {
	if w.collect && !w.pool.known[f] {
		w.pool.known[f] = true
		e := poolEntry[*ssa.Function]{f, w.origin[from], w.a.name(from)}
		key := normalize(f.Signature)
		idx, _ := w.pool.bySig.At(key).([]int)
		w.pool.bySig.Set(key, append(idx, len(w.pool.funcs)))
		w.pool.funcs = append(w.pool.funcs, e)
		if caller, ok := w.dynSigs.At(key).(*ssa.Function); ok {
			w.reach(f, caller, e.describe("dynamic call reaches value", nil), false)
		}
		if w.dispatchingBy != nil {
			w.reach(f, w.dispatchingBy, e.describe("dispatch reaches value", nil), false)
		}
	}
	w.reach(f, from, via{what: "takes it as a value"}, false)
}

// convert: a value of type t made into an interface value of type to.
//
// A module type has every method reachable, together with the methods of
// every module type reachable from its structure, since reflection (fmt,
// encoding/json) calls them. An outside type, converted or found in that
// structure, contributes only what module code can call on it: the methods
// implementing a module interface it is converted to, and the module
// interface methods module code invokes (see invoke). What an outside
// function does with a value it is handed is that function's own
// classification. An interface in the structure holds values converted
// elsewhere: each binary-level type implementing it is treated as converted
// to it here.
func (w *walker) convert(t, to types.Type, from *ssa.Function, how via) {
	if how.what == "" {
		how = via{what: "converts %s to an interface", typ: t}
	}
	if w.a.outsideType(t) && w.a.moduleInterface(to) {
		iface := to.Underlying().(*types.Interface)
		for i := range iface.NumMethods() {
			w.reachMethod(t, iface.Method(i), from, how)
		}
	}
	if w.converted.At(t) != nil {
		return
	}
	w.converted.Set(t, true)
	s := w.a.structureOf(t)
	for _, m := range s.methods {
		w.reach(m, from, how, false)
	}
	if w.a.moduleType(t) || w.a.outsideType(t) {
		w.mayBeInInterface(t, from)
	}
	for _, o := range s.outside {
		w.mayBeInInterface(o, from)
	}
	for _, it := range s.ifaces {
		iface := it.Underlying().(*types.Interface)
		if w.collect {
			w.structIfaces = append(w.structIfaces, pendingInvoke{invoke{recv: it, iface: iface}, from})
		}
		for _, e := range w.pool.implementing(iface) {
			w.convert(e.value, it, from, e.describe("reflection reaches type", nil))
		}
	}
}

// mayBeInInterface records a named type whose values may sit in an
// interface value: while collecting, it joins the binary level; otherwise,
// an outside type joins this walk's own list. Either way the module
// interface calls already reached are applied to it.
func (w *walker) mayBeInInterface(t types.Type, from *ssa.Function) {
	e := poolEntry[types.Type]{t, w.origin[from], w.a.name(from)}
	switch {
	case w.collect:
		if w.pool.typed.At(t) != nil {
			return
		}
		w.pool.typed.Set(t, true)
		w.pool.types = append(w.pool.types, e)
		for _, p := range w.structIfaces {
			if types.Implements(t, p.inv.iface) {
				w.convert(t, p.inv.recv, p.from, e.describe("reflection reaches type", nil))
			}
		}
		if w.dispatchingBy != nil && w.a.moduleType(t) {
			for _, m := range w.a.methodsOf(t) {
				w.reach(m, w.dispatchingBy, e.describe("dispatch reaches type", nil), false)
			}
		}
	case w.a.outsideType(t):
		if w.localTyped.At(t) != nil {
			return
		}
		w.localTyped.Set(t, true)
		w.local = append(w.local, e)
	default:
		// A module type converted in this walk already has every method
		// reachable.
		return
	}
	for _, p := range w.invokes {
		if types.Implements(t, p.inv.iface) {
			w.reachMethod(t, p.inv.method, p.from, e.describe("interface call reaches %s", t))
		}
	}
}

func (w *walker) dynamic(sig *types.Signature, from *ssa.Function) {
	if w.collect && w.dynSigs.At(normalize(sig)) == nil {
		w.dynSigs.Set(normalize(sig), from)
	}
	for _, e := range w.pool.withSignature(sig) {
		w.reach(e.value, from, e.describe("dynamic call reaches value", nil), false)
	}
}

// invoke: a call through a module interface reaches that method of every
// type that may sit in one. A call through an outside interface is a call
// of an outside method, charged by the classification of the interface
// method; the module types implementing it are reached as well.
func (w *walker) invoke(inv invoke, from *ssa.Function) {
	k := invokeKey{inv.iface, inv.method}
	if w.invoked[k] {
		return
	}
	w.invoked[k] = true
	if w.a.moduleInterface(inv.recv) {
		w.invokes = append(w.invokes, pendingInvoke{inv, from})
		for _, e := range w.pool.implementing(inv.iface) {
			w.reachMethod(e.value, inv.method, from, e.describe("interface call reaches %s", e.value))
		}
		for _, e := range implementing(w.local, inv.iface) {
			w.reachMethod(e.value, inv.method, from, e.describe("interface call reaches %s", e.value))
		}
		return
	}
	w.outsideInvoke(inv, from)
	for _, e := range w.pool.implementing(inv.iface) {
		if w.a.moduleType(e.value) {
			w.reachMethod(e.value, inv.method, from, e.describe("interface call reaches %s", e.value))
		}
	}
}

// outsideInvoke charges a call of an outside interface method, named by the
// interface that declares it ("TB.TempDir", "Writer.Write"). The universe
// declares one interface method, error's Error, which formats a message.
func (w *walker) outsideInvoke(inv invoke, from *ssa.Function) {
	m := inv.method
	if m.Pkg() == nil {
		return
	}
	o := w.a.methodOwner(m.Signature().Recv().Type(), m.Name())
	if o.key == "" {
		o = w.a.ownerIn(m.Pkg().Path(), m.Name())
	}
	name := o.pkg + "." + o.key
	if w.outsideCalls[name] {
		return
	}
	w.outsideCalls[name] = true
	o.class, o.classified = w.a.class.Lookup(o.pkg, o.key)
	w.classified(o, from, from)
}

func (w *walker) reachMethod(t types.Type, method *types.Func, from *ssa.Function, how via) {
	sel := w.a.prog.MethodSets.MethodSet(t).Lookup(method.Pkg(), method.Name())
	if sel == nil {
		return
	}
	if fn := w.a.prog.MethodValue(sel); fn != nil {
		w.reach(fn, from, how, false)
	}
}

// dispatchAll: a dispatching outside function reaches every binary-level
// function value and every method of every binary-level module type. What
// it does with an outside value is its own classification.
//
// The first dispatch in a walk is the only one that can reach anything: it
// reaches the whole pool, and a value that joins the pool later, while
// collecting, is reached from dispatchingBy as it joins.
func (w *walker) dispatchAll(at *ssa.Function) {
	if w.dispatched {
		return
	}
	w.dispatched = true
	if w.collect {
		w.dispatchingBy = at
	}
	for _, e := range w.pool.funcs {
		w.reach(e.value, at, e.describe("dispatch reaches value", nil), false)
	}
	for _, e := range w.pool.types {
		if !w.a.moduleType(e.value) {
			continue
		}
		for _, m := range w.a.methodsOf(e.value) {
			w.reach(m, at, e.describe("dispatch reaches type", nil), false)
		}
	}
}

// name names fn for a witness, relative to its module.
func (a *analyzer) name(fn *ssa.Function) string {
	if fn == nil {
		return "?"
	}
	return strings.ReplaceAll(fn.String(), a.mod.Path+"/", "")
}

func relativeTo(m module) types.Qualifier {
	return func(p *types.Package) string {
		path := p.Path()
		if rel, ok := strings.CutPrefix(path, m.Path+"/"); ok {
			return rel
		}
		return path
	}
}
