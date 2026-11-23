package main

import (
	"fmt"
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// analyzer decides reach over one module's SSA program. It holds what every
// walk shares and no walk changes: each function's summary and owner, each
// type's structure and methods.
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
	// unresolved: called-back interfaces the program does not hold, with
	// the first function calling one back.
	unresolved map[string]string
	callbacks  map[string]*types.Interface
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
		unresolved:   map[string]string{},
		callbacks:    map[string]*types.Interface{},
	}
}

// walker is one walk: a binary's setup, over an empty layer, or one test,
// over its binary's layer. It reads both layers as one set and adds only to
// its own.
type walker struct {
	a      *analyzer
	layers [2]*layer // the binary's, then the walk's own
	own    *layer
	queue  []*ssa.Function
}

func newWalker(a *analyzer, binary *layer) *walker {
	own := newLayer()
	return &walker{a: a, layers: [2]*layer{binary, own}, own: own}
}

func (w *walker) seen(fn *ssa.Function) bool {
	_, below := w.layers[0].visited[fn]
	_, mine := w.own.visited[fn]
	return below || mine
}

func (w *walker) stepOf(fn *ssa.Function) step {
	if st, ok := w.own.visited[fn]; ok {
		return st
	}
	return w.layers[0].visited[fn]
}

func (w *walker) originOf(fn *ssa.Function) string {
	if o, ok := w.own.origin[fn]; ok {
		return o
	}
	return w.layers[0].origin[fn]
}

func (w *walker) sites(fn *ssa.Function) []callSite {
	return append(append([]callSite(nil), w.layers[0].sitesInto[fn]...), w.own.sitesInto[fn]...)
}

func (w *walker) entryOrigin(from *ssa.Function) entryOrigin {
	return entryOrigin{origin: w.originOf(from), creator: w.a.name(from)}
}

// reached is every touch the walk's reach holds, its binary's setup's
// included: `go test -run` runs the setup for every test it runs.
func (w *walker) reached() reached {
	r := reached{}
	for _, l := range w.layers {
		for t, wit := range l.touches {
			r[t] = wit
		}
	}
	return r
}

// root starts the walk at fn, made to run by origin.
func (w *walker) root(fn *ssa.Function, origin string) {
	if w.seen(fn) {
		return
	}
	w.own.visited[fn] = step{via: staticCall{}}
	w.own.origin[fn] = origin
	w.enter(fn, nil)
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
		w.a.bodiless[w.a.name(fn)] = w.caller(fn, w.stepOf(fn).from)
		return
	}
	s := w.a.summary(fn)
	for _, site := range s.static {
		callee := site.common.StaticCallee()
		switch k := w.a.owner(callee).knows.(type) {
		case moduleCode:
			w.own.sitesInto[callee] = append(w.own.sitesInto[callee], site)
		case SpawnArg:
			arg := k.Arg
			if callee.Signature.Recv() != nil {
				arg++
			}
			w.own.spawns = append(w.own.spawns, spawnSite{in: fn, spawner: w.a.owner(callee).name.String(), program: programValue{site.common.Args[arg]}})
		}
		w.reach(callee, fn, staticCall{})
	}
	for _, f := range s.taken {
		w.take(f, fn)
	}
	for _, c := range s.converted {
		w.convert(c.typ, c.to, fn, converting{c.typ})
	}
	for _, d := range s.dynamic {
		w.dynamic(d, fn)
	}
	for _, inv := range s.invokes {
		w.invoke(inv, fn)
	}
	for _, sf := range s.spawnFields {
		w.own.spawns = append(w.own.spawns, spawnSite{in: fn, spawner: sf.spawner, program: programValue{sf.program}})
	}
}

// reach makes fn reachable from from. Module code joins the walk; code
// outside the module is never walked, only charged.
func (w *walker) reach(fn, from *ssa.Function, how via) {
	o := w.a.owner(fn)
	if _, spawner := o.knows.(SpawnArg); spawner {
		if _, static := how.(staticCall); !static {
			w.own.spawns = append(w.own.spawns, spawnSite{in: from, spawner: o.name.String(), program: programUnknown{"the spawner is reached as a value"}})
		}
	}
	if w.seen(fn) {
		return
	}
	w.own.visited[fn] = step{from: from, via: how}
	w.own.origin[fn] = w.originOf(from)
	w.enter(fn, from)
}

// enter walks fn if it is module code, and charges it otherwise.
func (w *walker) enter(fn, from *ssa.Function) {
	o := w.a.owner(fn)
	if _, walked := o.knows.(moduleCode); walked {
		w.queue = append(w.queue, fn)
		return
	}
	w.charge(o.name, o.knows, fn, from)
}

// charge records what the classification says a call of the outside
// function or interface method name, reached at at from from, can do.
func (w *walker) charge(name funcName, k knowledge, at, from *ssa.Function) {
	switch k := k.(type) {
	case unclassified:
		if _, seen := w.a.unclassified[name.String()]; !seen {
			w.a.unclassified[name.String()] = w.caller(at, from)
		}
	case Effect:
		for t := range k.Touches.Each {
			w.touch(t, at, func() string { return fmt.Sprintf("%s (%s)", name, k.Touches) })
		}
		if k.Dispatching {
			w.dispatch(at)
		}
		for _, m := range k.Calls {
			w.callBack(m, at)
		}
	case SpawnArg:
		// Its touch is its program's, decided as the walk's spawns settle.
	}
}

// caller names what reached at: from, or, at a root, the root's origin.
func (w *walker) caller(at, from *ssa.Function) string {
	if from == nil {
		return w.originOf(at)
	}
	return w.a.name(from)
}

// touch records t, reached at fn, with its witness if it is t's first;
// what names the touch, and is spelled only for a first.
func (w *walker) touch(t Touch, at *ssa.Function, what func() string) {
	for _, l := range w.layers {
		if _, ok := l.touches[t]; ok {
			return
		}
	}
	w.own.touches[t] = witness{chain: w.chain(at), what: what()}
}

// take: a function taken as a value joins the address-taken functions,
// reached by every dynamic call of its signature and every dispatch, in
// either layer. Taking it runs nothing: an initializer that fills a table
// of functions makes none of them reachable.
func (w *walker) take(f, from *ssa.Function) {
	for _, l := range w.layers {
		if l.isAddrTaken[f] {
			return
		}
	}
	e := funcEntry{f, w.entryOrigin(from)}
	w.own.addTaken(e)
	key := normalize(f.Signature)
	for _, l := range w.layers {
		if call, ok := l.dynCalls.At(key).(pendingDynamic); ok {
			w.reach(f, call.from, throughFunc{call.how, e.where})
		}
		if l.dispatcher != nil {
			w.reach(f, l.dispatcher, throughFunc{"dispatch", e.where})
		}
	}
}

// dynamic: a call of a function value reaches every address-taken function
// of its signature.
func (w *walker) dynamic(d dynamicCall, from *ssa.Function) {
	key := normalize(d.sig)
	for _, l := range w.layers {
		if l.dynCalls.At(key) != nil {
			return
		}
	}
	w.own.dynCalls.Set(key, pendingDynamic{from, d.how})
	for _, l := range w.layers {
		for _, e := range l.withSignature(d.sig) {
			w.reach(e.fn, from, throughFunc{d.how, e.where})
		}
	}
}

// convert: a value of type t made into an interface value of type to.
//
// A module type has every method reachable, together with the methods of
// every module type reachable from its structure, since reflection (fmt,
// encoding/json) calls them. An outside type, converted or found in that
// structure, contributes only what module code can call on it: the module
// interface methods of to, and those module code invokes (see invoke). What
// an outside function does with a value it is handed is that function's own
// classification. An interface in the structure holds values converted
// elsewhere: each runtime type implementing it is converted to it here.
func (w *walker) convert(t types.Type, to *types.Interface, from *ssa.Function, how via) {
	if w.a.outsideType(t) {
		for i := range to.NumMethods() {
			if m := to.Method(i); w.a.moduleMethod(m) {
				w.reachMethod(t, m, from, how)
			}
		}
	}
	for _, l := range w.layers {
		if l.structured.At(t) != nil {
			return
		}
	}
	w.own.structured.Set(t, true)
	s := w.a.structureOf(t)
	for _, m := range s.methods {
		w.reach(m, from, how)
	}
	if w.a.moduleType(t) || w.a.outsideType(t) {
		w.addRuntimeType(t, from)
	}
	for _, o := range s.outside {
		w.addRuntimeType(o, from)
	}
	for _, iface := range s.ifaces {
		w.reflect(iface, from)
	}
}

// addRuntimeType records a named type whose values may sit in an interface
// value, and applies to it every call pending in either layer.
func (w *walker) addRuntimeType(t types.Type, from *ssa.Function) {
	for _, l := range w.layers {
		if l.isRuntime.At(t) != nil {
			return
		}
	}
	e := typeEntry{t, w.entryOrigin(from)}
	w.own.addRuntime(e)
	for _, l := range w.layers {
		for _, p := range l.invokes {
			if types.Implements(t, p.inv.iface) {
				w.reachMethod(t, p.inv.method, p.from, throughType{"interface call", t, e.where})
			}
		}
		for _, r := range l.reflected {
			if types.Implements(t, r.iface) {
				w.convert(t, r.iface, r.from, throughType{"reflection", t, e.where})
			}
		}
		if l.dispatcher != nil && w.a.moduleType(t) {
			for _, m := range w.a.methodsOf(t) {
				w.reach(m, l.dispatcher, throughType{"dispatch", t, e.where})
			}
		}
		if w.a.outsideType(t) {
			for _, c := range l.callbacks {
				if types.Implements(t, c.iface) {
					w.reachMethods(t, c.iface, c.from, throughType{c.name + " callback", t, e.where})
				}
			}
		}
	}
}

// reflect: an interface in a converted value's structure stands for every
// runtime type implementing it, each converted to it.
func (w *walker) reflect(iface *types.Interface, from *ssa.Function) {
	for _, l := range w.layers {
		if l.isReflected[iface] {
			return
		}
	}
	w.own.isReflected[iface] = true
	w.own.reflected = append(w.own.reflected, pendingReflect{iface, from})
	for _, l := range w.layers {
		for _, e := range l.implementing(iface) {
			w.convert(e.typ, iface, from, throughType{"reflection", e.typ, e.where})
		}
	}
}

// invoke: a call of a module interface method reaches that method of every
// runtime type implementing the interface. A call of an outside interface
// method is a call of an outside method, charged by its classification; the
// runtime module types implementing it are reached as well.
func (w *walker) invoke(inv invoke, from *ssa.Function) {
	for _, l := range w.layers {
		if l.invoked[inv] {
			return
		}
	}
	w.own.invoked[inv] = true
	module := w.a.moduleMethod(inv.method)
	if module {
		w.own.invokes = append(w.own.invokes, pendingInvoke{inv, from})
	} else {
		name := interfaceMethodName(inv.method)
		w.charge(name, w.a.class.Lookup(name.pkg, name.key), from, from)
	}
	for _, l := range w.layers {
		for _, e := range l.implementing(inv.iface) {
			if module || w.a.moduleType(e.typ) {
				w.reachMethod(e.typ, inv.method, from, throughType{"interface call", e.typ, e.where})
			}
		}
	}
}

// dispatch: a dispatching outside function reaches every address-taken
// function and every method of every runtime module type. What it does with
// an outside value is its own classification.
//
// The first dispatch in either layer is the only one that can reach
// anything: it reaches the whole set, and what either layer creates later
// meets it as it is created.
func (w *walker) dispatch(at *ssa.Function) {
	for _, l := range w.layers {
		if l.dispatcher != nil {
			return
		}
	}
	w.own.dispatcher = at
	for _, l := range w.layers {
		for _, e := range l.addrTaken {
			w.reach(e.fn, at, throughFunc{"dispatch", e.where})
		}
		for _, e := range l.runtimeTypes {
			if !w.a.moduleType(e.typ) {
				continue
			}
			for _, m := range w.a.methodsOf(e.typ) {
				w.reach(m, at, throughType{"dispatch", e.typ, e.where})
			}
		}
	}
}

// callBack: an outside function that calls back the interface named name on
// the values handed to it reaches its methods on every runtime outside type
// implementing it, each charged by its classification.
func (w *walker) callBack(name string, at *ssa.Function) {
	iface, ok := w.a.callbackInterface(name)
	if !ok {
		w.a.unresolved[name] = w.a.name(at)
		return
	}
	for _, l := range w.layers {
		if l.isCalledBack[iface] {
			return
		}
	}
	w.own.isCalledBack[iface] = true
	w.own.callbacks = append(w.own.callbacks, pendingCallback{iface, name, at})
	for _, l := range w.layers {
		for _, e := range l.runtimeTypes {
			if w.a.outsideType(e.typ) && types.Implements(e.typ, iface) {
				w.reachMethods(e.typ, iface, at, throughType{name + " callback", e.typ, e.where})
			}
		}
	}
}

// callbackInterface resolves a called-back interface in the analyzer's own
// program, whose types are the ones its values have.
func (a *analyzer) callbackInterface(name string) (*types.Interface, bool) {
	if iface, ok := a.callbacks[name]; ok {
		return iface, true
	}
	iface, ok := lookupInterface(name, func(path string) *types.Package {
		if p := a.prog.ImportedPackage(path); p != nil {
			return p.Pkg
		}
		return nil
	})
	if ok {
		a.callbacks[name] = iface
	}
	return iface, ok
}

func (w *walker) reachMethod(t types.Type, m *types.Func, from *ssa.Function, how via) {
	if fn, ok := w.a.method(t, m.Pkg(), m.Name()); ok {
		w.reach(fn, from, how)
	}
}

func (w *walker) reachMethods(t types.Type, iface *types.Interface, from *ssa.Function, how via) {
	for i := range iface.NumMethods() {
		w.reachMethod(t, iface.Method(i), from, how)
	}
}
