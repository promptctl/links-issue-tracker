package main

import (
	"fmt"
	"go/types"
	"slices"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// analyzer decides reach over one module's SSA program. It holds what every
// walk of the module shares: what is derived once per function or type
// (summary, owner, structure, methods, carriage, called-back interfaces, the
// RTA state's type keys), filled in by the first walk that asks; and the
// refusals the walks record (unclassified, bodiless, unresolved).
type analyzer struct {
	prog     *ssa.Program
	mod      module
	moduleOf map[string]string // each package's module, "std" for the standard library
	class    *Classification

	summaries  map[*ssa.Function]*summary
	structures typeutil.Map // types.Type -> *structure
	methods    typeutil.Map // types.Type -> []*ssa.Function
	carriages  typeutil.Map // *types.Signature -> carriage
	typeKeys   typeutil.Map // types.Type -> int
	owners     map[*ssa.Function]owner
	generics   *genericSource
	instances  map[instanceKey]*genericInstance

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
	// inconsistencies: what the analysis found that its own model rules
	// out, each a bug in the tool, never a fact about the suite.
	inconsistencies map[string]bool
}

// inconsistent records a bug in the tool, which refuses the run.
// [LAW:no-silent-failure]
func (a *analyzer) inconsistent(format string, args ...any) {
	a.inconsistencies[fmt.Sprintf(format, args...)] = true
}

func newAnalyzer(l *loaded, class *Classification) *analyzer {
	return &analyzer{
		prog:            l.Prog,
		mod:             l.Module,
		moduleOf:        l.ModuleOf,
		class:           class,
		summaries:       map[*ssa.Function]*summary{},
		owners:          map[*ssa.Function]owner{},
		generics:        l.Generics,
		instances:       map[instanceKey]*genericInstance{},
		unclassified:    map[string]string{},
		bodiless:        map[string]string{},
		unresolved:      map[string]string{},
		callbacks:       map[string]*types.Interface{},
		inconsistencies: map[string]bool{},
	}
}

// walker is one walk: a binary's setup, over an empty layer, or one test,
// over its binary's finished layer. It reads every collection of the state
// through a set joining the two, and adds only to its own layer.
type walker struct {
	a      *analyzer
	binary *finished
	own    *layer
	queue  []*ssa.Function

	steps      set[*ssa.Function, step]
	indirect   set[*ssa.Function, via]
	touches    set[Touch, touched]
	withheld   set[withheldKey, withholding]
	sites      set[callSite, callSite]
	spawns     set[spawnSite, spawnSite]
	taken      set[*ssa.Function, funcEntry]
	runtime    set[int, typeEntry]
	structured set[int, struct{}]
	called     set[int, pendingCall]
	invoked    set[invoke, pendingInvoke]
	reflected  set[int, pendingReflect]
	callbacks  set[int, pendingCallback]
	dispatched set[struct{}, *ssa.Function]
	reflective set[struct{}, pendingCall]
}

func newWalker(a *analyzer, binary *finished) *walker {
	b, own := binary.layer, newLayer()
	return &walker{
		a: a, binary: binary, own: own,
		steps:      set[*ssa.Function, step]{b.steps, own.steps},
		indirect:   set[*ssa.Function, via]{b.indirect, own.indirect},
		touches:    set[Touch, touched]{b.touches, own.touches},
		withheld:   set[withheldKey, withholding]{b.withheld, own.withheld},
		sites:      set[callSite, callSite]{b.sites, own.sites},
		spawns:     set[spawnSite, spawnSite]{b.spawns, own.spawns},
		taken:      set[*ssa.Function, funcEntry]{b.taken, own.taken},
		runtime:    set[int, typeEntry]{b.runtime, own.runtime},
		structured: set[int, struct{}]{b.structured, own.structured},
		called:     set[int, pendingCall]{b.called, own.called},
		invoked:    set[invoke, pendingInvoke]{b.invoked, own.invoked},
		reflected:  set[int, pendingReflect]{b.reflected, own.reflected},
		callbacks:  set[int, pendingCallback]{b.callbacks, own.callbacks},
		dispatched: set[struct{}, *ssa.Function]{b.dispatched, own.dispatched},
		reflective: set[struct{}, pendingCall]{b.reflective, own.reflective},
	}
}

// finish ends a setup walk: its layer becomes the binary's, finished.
func (w *walker) finish() *finished { return &finished{layer: w.own} }

func (w *walker) stepOf(fn *ssa.Function) step {
	st, _ := w.steps.get(fn)
	return st
}

func (w *walker) entryOrigin(from *ssa.Function) entryOrigin {
	return entryOrigin{origin: w.stepOf(from).origin, creator: w.a.name(from)}
}

// implementing is every runtime type of either layer implementing iface.
func (w *walker) implementing(iface *types.Interface) []typeEntry {
	return w.runtime.matching(w.a.typeKey(iface), func(e typeEntry) bool { return types.Implements(e.typ, iface) })
}

// found is what the walk found: every touch its reach holds, its binary's
// setup's included, since `go test -run` runs the setup for every test it
// runs; and every type argument withheld in its own layer, since what a
// test's setup withheld is its setup's.
func (w *walker) found() found {
	r := reached{}
	for t := range w.touches.all {
		r[t.touch] = t.witness
	}
	return found{Reached: r, Withheld: slices.Clone(w.own.withheld.entries)}
}

// root starts the walk at fn, made to run by origin.
func (w *walker) root(fn *ssa.Function, origin string) {
	if w.steps.add(fn, step{via: staticCall{}, origin: origin}) {
		w.enter(fn, nil)
	}
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
			w.sites.addTo(callee, site, site)
		case SpawnArg:
			arg := k.Arg
			if callee.Signature.Recv() != nil {
				arg++
			}
			sp := spawnSite{in: fn, spawner: w.a.owner(callee).name.String(), program: programValue{site.common.Args[arg]}}
			w.spawns.add(sp, sp)
		}
		w.reach(callee, fn, staticCall{})
	}
	for _, f := range s.taken {
		w.take(f, fn)
	}
	for _, c := range s.converted {
		w.convert(c.typ, c.to, fn, converting{c.typ})
	}
	for _, sig := range s.dynamic {
		w.callValue(sig, fn)
	}
	for _, inv := range s.invokes {
		w.invoke(inv, fn)
	}
	for _, sf := range s.spawnFields {
		sp := spawnSite{in: fn, spawner: sf.spawner, program: programValue{sf.program}}
		w.spawns.add(sp, sp)
	}
}

// reach makes fn reachable from from. Module code joins the walk; code
// outside the module is never walked, only charged.
func (w *walker) reach(fn, from *ssa.Function, how via) {
	if _, static := how.(staticCall); !static {
		w.indirectly(fn, from, how)
	}
	if w.steps.add(fn, step{from: from, via: how, origin: w.stepOf(from).origin}) {
		w.enter(fn, from)
	}
}

// indirectly records fn reached other than by a static call, the first
// time either layer sees it so. A spawner reached so runs a program nothing
// names.
func (w *walker) indirectly(fn, from *ssa.Function, how via) {
	if !w.indirect.add(fn, how) {
		return
	}
	o := w.a.owner(fn)
	if _, spawner := o.knows.(SpawnArg); spawner {
		sp := spawnSite{in: from, spawner: o.name.String(), program: programUnknown{"the spawner is reached as a value"}}
		w.spawns.add(sp, sp)
	}
}

// enter walks fn if it is module code, and charges it otherwise: an
// outside generic's instance is also handed the types it is instantiated
// with.
func (w *walker) enter(fn, from *ssa.Function) {
	o := w.a.owner(fn)
	if _, walked := o.knows.(moduleCode); walked {
		w.queue = append(w.queue, fn)
		return
	}
	if in, generic := instanceOf(fn); generic {
		w.typeArguments(in, fn)
	}
	w.charge(o.name, o.knows, fn.Signature, fn, from)
}

// charge records what the classification says a call of the outside
// function or interface method name, of signature sig, reached at at from
// from, can do: it is a call the walk does not follow, and whatever else its
// class says.
func (w *walker) charge(name funcName, k knowledge, sig *types.Signature, at, from *ssa.Function) {
	w.unfollowed(sig, at, "call into "+name.String())
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

// typeArguments: what an outside generic's instance, at, does with the
// types it is instantiated with is decided from its source (see
// generic.go). Each type it converts to an interface reaches exactly what
// module code converting that type reaches; each constraint method it
// calls is reached on its type argument, an interface call when the type
// argument is an interface; a type argument it does neither with reaches
// nothing, and the walk records it as withheld.
func (w *walker) typeArguments(in instance, at *ssa.Function) {
	d := w.a.decide(in)
	for _, ta := range d.uses {
		switch u := ta.use.(type) {
		case conversion:
			w.convert(u.typ, u.to, at, ta)
		case constraintCall:
			switch m := w.a.method(u.typ, u.pkg, u.method).(type) {
			case concreteMethod:
				w.reach(m.fn, at, ta)
			case abstractMethod:
				w.invoke(m.inv, at)
			case absentMethod:
				w.a.inconsistent("%s: its type argument %s has no method %s, which its constraint declares", namer{w.a.mod}.instance(in), u.typ, u.method)
			}
		}
	}
	for _, wh := range d.withheld {
		w.withheld.add(withheldKey{w.a.instanceKey(wh.in), wh.arg}, wh)
	}
}

// unfollowed is the one rule at every callee the walk does not follow: an
// outside function or method, and the target of every dynamic call, which
// may be one. Whatever it does, it can call back any address-taken function
// whose signature its argument types (receiver and parameters, through
// their structure) can carry; and, handed an interface, any function
// converted to one, which reflection can call.
func (w *walker) unfollowed(sig *types.Signature, at *ssa.Function, how string) {
	c := w.a.carried(sig)
	for _, s := range c.sigs {
		w.call(s, at, how)
	}
	if c.iface && w.reflective.add(struct{}{}, pendingCall{at, how}) {
		for e := range w.runtime.all {
			if s, ok := e.typ.Underlying().(*types.Signature); ok {
				w.call(s, at, how)
			}
		}
	}
}

// callValue is a call of a function value of signature sig: it reaches every
// address-taken function of it, and its target may be outside code.
func (w *walker) callValue(sig *types.Signature, from *ssa.Function) {
	w.call(sig, from, "dynamic call")
	w.unfollowed(sig, from, "dynamic call")
}

// call is a call of some function of signature sig: it reaches every
// address-taken function of it, in either layer, now or once taken.
func (w *walker) call(sig *types.Signature, from *ssa.Function, how string) {
	key := w.a.sigKey(sig)
	if !w.called.add(key, pendingCall{from, how}) {
		return
	}
	for e := range w.taken.group(key) {
		w.reach(e.fn, from, throughFunc{how, e.where})
	}
}

// caller names what reached at: from, or, at a root, the root's origin.
func (w *walker) caller(at, from *ssa.Function) string {
	if from == nil {
		return w.stepOf(at).origin
	}
	return w.a.name(from)
}

// touch records t, reached at fn, with its witness if it is t's first;
// what names the touch, and is spelled only for a first.
func (w *walker) touch(t Touch, at *ssa.Function, what func() string) {
	if !w.touches.has(t) {
		w.touches.add(t, touched{t, witness{chain: w.chain(at), what: what()}})
	}
}

// take: a function taken as a value joins the address-taken functions,
// reached by every call of its signature and every dispatch, in either
// layer. Taking it runs nothing: an initializer that fills a table of
// functions makes none of them reachable.
func (w *walker) take(f, from *ssa.Function) {
	key := w.a.sigKey(f.Signature)
	e := funcEntry{f, w.entryOrigin(from)}
	if !w.taken.addTo(key, f, e) {
		return
	}
	if p, ok := w.called.get(key); ok {
		w.reach(f, p.from, throughFunc{p.how, e.where})
	}
	if d, ok := w.dispatched.get(struct{}{}); ok {
		w.reach(f, d, throughFunc{"dispatch", e.where})
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
// elsewhere: each runtime type implementing it is converted to it here. A
// function value converted is a runtime type too, which reflection can
// call.
func (w *walker) convert(t types.Type, to *types.Interface, from *ssa.Function, how via) {
	if w.a.outsideType(t) {
		for i := range to.NumMethods() {
			if m := to.Method(i); w.a.moduleMethod(m) {
				w.reachMethod(t, m, from, how)
			}
		}
	}
	if !w.structured.add(w.a.typeKey(t), struct{}{}) {
		return
	}
	s := w.a.structureOf(t)
	for _, m := range s.methods {
		w.reach(m, from, how)
	}
	// A value of an interface type converted is the value it holds, already
	// a runtime type where it was converted to that interface; an outside
	// generic's type argument can be one.
	if _, isFunc := t.Underlying().(*types.Signature); !types.IsInterface(t) && (isFunc || w.a.moduleType(t) || w.a.outsideType(t)) {
		w.addRuntimeType(t, from)
	}
	for _, o := range s.outside {
		w.addRuntimeType(o, from)
	}
	for _, iface := range s.ifaces {
		w.reflect(iface, from)
	}
}

// addRuntimeType records a type whose values may sit in an interface value,
// and applies to it every call pending in either layer.
func (w *walker) addRuntimeType(t types.Type, from *ssa.Function) {
	e := typeEntry{t, w.entryOrigin(from)}
	if !w.runtime.add(w.a.typeKey(t), e) {
		return
	}
	for p := range w.invoked.all {
		if (p.module || w.a.moduleType(t)) && types.Implements(t, p.inv.iface) {
			w.reachMethod(t, p.inv.method, p.from, throughType{"interface call", t, e.where})
		}
	}
	for r := range w.reflected.all {
		if types.Implements(t, r.iface) {
			w.convert(t, r.iface, r.from, throughType{"reflection", t, e.where})
		}
	}
	if d, ok := w.dispatched.get(struct{}{}); ok && w.a.moduleType(t) {
		for _, m := range w.a.methodsOf(t) {
			w.reach(m, d, throughType{"dispatch", t, e.where})
		}
	}
	if w.a.outsideType(t) {
		for c := range w.callbacks.all {
			if types.Implements(t, c.iface) {
				w.reachMethods(t, c.iface, c.from, throughType{c.name + " callback", t, e.where})
			}
		}
	}
	if s, ok := t.Underlying().(*types.Signature); ok {
		if p, ok := w.reflective.get(struct{}{}); ok {
			w.call(s, p.from, p.how)
		}
	}
}

// reflect: an interface in a converted value's structure stands for every
// runtime type implementing it, each converted to it.
func (w *walker) reflect(iface *types.Interface, from *ssa.Function) {
	if !w.reflected.add(w.a.typeKey(iface), pendingReflect{iface, from}) {
		return
	}
	for _, e := range w.implementing(iface) {
		w.convert(e.typ, iface, from, throughType{"reflection", e.typ, e.where})
	}
}

// invoke: a call of a module interface method reaches that method of every
// runtime type implementing the interface. A call of an outside interface
// method is a call of an outside method, charged by its classification; the
// runtime module types implementing it are reached as well.
func (w *walker) invoke(inv invoke, from *ssa.Function) {
	module := w.a.moduleMethod(inv.method)
	if !w.invoked.add(inv, pendingInvoke{inv, from, module}) {
		return
	}
	if !module {
		name := interfaceMethodName(inv.method)
		w.charge(name, w.a.class.Lookup(name.pkg, name.key), inv.method.Signature(), from, from)
	}
	for _, e := range w.implementing(inv.iface) {
		if module || w.a.moduleType(e.typ) {
			w.reachMethod(e.typ, inv.method, from, throughType{"interface call", e.typ, e.where})
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
	if !w.dispatched.add(struct{}{}, at) {
		return
	}
	for e := range w.taken.all {
		w.reach(e.fn, at, throughFunc{"dispatch", e.where})
	}
	for e := range w.runtime.all {
		if !w.a.moduleType(e.typ) {
			continue
		}
		for _, m := range w.a.methodsOf(e.typ) {
			w.reach(m, at, throughType{"dispatch", e.typ, e.where})
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
	if !w.callbacks.add(w.a.typeKey(iface), pendingCallback{iface, name, at}) {
		return
	}
	for e := range w.runtime.all {
		if w.a.outsideType(e.typ) && types.Implements(e.typ, iface) {
			w.reachMethods(e.typ, iface, at, throughType{name + " callback", e.typ, e.where})
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

// reachMethod reaches the method m of t, a runtime type implementing an
// interface that declares m, so a concrete type with the method.
func (w *walker) reachMethod(t types.Type, m *types.Func, from *ssa.Function, how via) {
	switch l := w.a.method(t, m.Pkg(), m.Name()).(type) {
	case concreteMethod:
		w.reach(l.fn, from, how)
	case abstractMethod:
		w.a.inconsistent("runtime type %s is an interface", t)
	case absentMethod:
		w.a.inconsistent("runtime type %s has no method %s, which an interface it implements declares", t, m.Name())
	}
}

func (w *walker) reachMethods(t types.Type, iface *types.Interface, from *ssa.Function, how via) {
	for i := range iface.NumMethods() {
		w.reachMethod(t, iface.Method(i), from, how)
	}
}
