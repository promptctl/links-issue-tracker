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
	converted   []types.Type
	dynamic     []*types.Signature
	invokes     []invoke
	spawnFields []spawnField
}

type callSite struct {
	caller *ssa.Function
	common *ssa.CallCommon
}

type invoke struct {
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
					s.invokes = append(s.invokes, invoke{common.Value.Type().Underlying().(*types.Interface), common.Method})
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
				s.converted = append(s.converted, v.X.Type())
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

// structure is every method reachable from a type's structure — its own,
// and those of every named type among its fields, elements and pointees,
// outside types included — and every interface type in that structure.
type structure struct {
	methods []*ssa.Function
	ifaces  []*types.Interface
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
			if iface, ok := u.Underlying().(*types.Interface); ok {
				s.ifaces = append(s.ifaces, iface)
				return
			}
			s.methods = append(s.methods, a.methodsOf(u)...)
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
// every concrete type converted to an interface in the reach of the
// initialization of the binary's packages and of its TestMain, each with its
// origin. These are the possible targets of a test's dynamic and interface
// calls.
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
	origin  string // the initializer or TestMain that made it exist
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
	var out []poolEntry[types.Type]
	for _, e := range p.types {
		if types.Implements(e.value, iface) {
			out = append(out, e)
		}
	}
	if p.final {
		p.implementers.Set(iface, out)
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
	origin  map[*ssa.Function]string // collect: the seed each function was reached from
	queue   []*ssa.Function
	touches map[Touch]witness

	sitesInto map[*ssa.Function][]callSite
	spawns    []spawnSite

	// Dispatches reached while collecting, applied again to every value
	// that joins the pool after them.
	dynSigs       typeutil.Map // normalized signature -> *ssa.Function (first caller)
	invokes       []pendingInvoke
	structIfaces  []pendingInvoke
	dispatchingBy *ssa.Function
	converted     typeutil.Map
	// dispatched: the walk has reached a dispatching function. Once the
	// pool is final, a second dispatch reaches nothing the first did not.
	dispatched bool
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
		a:         a,
		pool:      p,
		collect:   collect,
		visited:   map[*ssa.Function]step{},
		origin:    map[*ssa.Function]string{},
		touches:   map[Touch]witness{},
		sitesInto: map[*ssa.Function][]callSite{},
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
			if cl, ok := w.a.class.Lookup(o.pkg, o.key); ok && cl.Spawn != nil && !cl.Spawn.Field {
				arg := cl.Spawn.Arg
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
	for _, t := range s.converted {
		w.convert(t, fn, via{})
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
	if !o.classified {
		name := o.pkg + "." + o.key
		if _, seen := w.a.unclassified[name]; !seen {
			w.a.unclassified[name] = w.a.name(from)
		}
		return
	}
	for t := range o.class.Touches.Each {
		w.touch(t, fn, func() string { return fmt.Sprintf("%s.%s (%s)", o.pkg, o.key, o.class.Touches) })
	}
	if o.class.Dispatching {
		w.dispatchAll(fn)
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

// convert: a type converted to an interface in the walk's reach has every
// method reachable, together with the methods of every type reachable from
// its structure, since reflection (fmt, encoding/json) calls them. An
// interface in that structure holds values converted elsewhere: each
// binary-level type implementing it is treated as converted here.
func (w *walker) convert(t types.Type, from *ssa.Function, how via) {
	if w.collect && w.pool.typed.At(t) == nil {
		w.pool.typed.Set(t, true)
		e := poolEntry[types.Type]{t, w.origin[from], w.a.name(from)}
		w.pool.types = append(w.pool.types, e)
		for _, p := range w.invokes {
			if types.Implements(t, p.inv.iface) {
				w.reachMethod(e, p.inv.method, p.from)
			}
		}
		for _, p := range w.structIfaces {
			if types.Implements(t, p.inv.iface) {
				w.convert(t, p.from, e.describe("reflection reaches type", nil))
			}
		}
		if w.dispatchingBy != nil {
			for _, m := range w.a.methodsOf(t) {
				w.reach(m, w.dispatchingBy, e.describe("dispatch reaches type", nil), false)
			}
		}
	}
	if w.converted.At(t) != nil {
		return
	}
	w.converted.Set(t, true)
	if how.what == "" {
		how = via{what: "converts %s to an interface", typ: t}
	}
	s := w.a.structureOf(t)
	for _, m := range s.methods {
		w.reach(m, from, how, false)
	}
	for _, iface := range s.ifaces {
		if w.collect {
			w.structIfaces = append(w.structIfaces, pendingInvoke{invoke{iface: iface}, from})
		}
		for _, e := range w.pool.implementing(iface) {
			w.convert(e.value, from, e.describe("reflection reaches type", nil))
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

func (w *walker) invoke(inv invoke, from *ssa.Function) {
	if w.collect {
		w.invokes = append(w.invokes, pendingInvoke{inv, from})
	}
	for _, e := range w.pool.implementing(inv.iface) {
		w.reachMethod(e, inv.method, from)
	}
}

func (w *walker) reachMethod(e poolEntry[types.Type], method *types.Func, from *ssa.Function) {
	sel := w.a.prog.MethodSets.MethodSet(e.value).Lookup(method.Pkg(), method.Name())
	if sel == nil {
		return
	}
	if fn := w.a.prog.MethodValue(sel); fn != nil {
		w.reach(fn, from, e.describe("interface call reaches %s", e.value), false)
	}
}

// dispatchAll: a dispatching outside function reaches every binary-level
// function value and every method of every binary-level type.
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
