package main

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// moduleType reports whether t, or what t points to, is a named type the
// module declares.
func (a *analyzer) moduleType(t types.Type) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && a.inModule(n.Obj().Pkg().Path())
}

// outsideType reports whether t, or what t points to, is a named concrete
// type declared outside the module.
func (a *analyzer) outsideType(t types.Type) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && !a.inModule(n.Obj().Pkg().Path()) && !types.IsInterface(n)
}

// moduleMethod reports whether the module declares the interface method m:
// a call of it is module code's, and an outside type converted to an
// interface whose complete method set holds it may have it called.
func (a *analyzer) moduleMethod(m *types.Func) bool {
	return m.Pkg() != nil && a.inModule(m.Pkg().Path())
}

// structure is what a converted value's structure contributes: every method
// of each module type among the type, its fields, elements and pointees;
// each outside named type among them; and each interface among them.
type structure struct {
	methods []*ssa.Function
	outside []types.Type
	ifaces  []*types.Interface
}

// part is how a type is made of another: the parts a walk of a type can
// descend into.
type part int

const (
	underlyingPart part = iota // a named type's underlying type
	elementPart                // a pointer's, slice's, array's or channel's element, a map's key and element, a struct's field
	argumentPart               // a named type's type argument
	signaturePart              // a signature's parameters and results, a tuple's variables
	methodPart                 // an interface's methods and embedded types, a union's terms
)

// parts is every type t is made of, with how.
func parts(t types.Type) []typePart {
	var ps []typePart
	add := func(k part, ts ...types.Type) {
		for _, p := range ts {
			ps = append(ps, typePart{p, k})
		}
	}
	switch u := t.(type) {
	case *types.Named:
		add(underlyingPart, u.Underlying())
		for a := range u.TypeArgs().Types() {
			add(argumentPart, a)
		}
	case *types.Pointer:
		add(elementPart, u.Elem())
	case *types.Slice:
		add(elementPart, u.Elem())
	case *types.Array:
		add(elementPart, u.Elem())
	case *types.Chan:
		add(elementPart, u.Elem())
	case *types.Map:
		add(elementPart, u.Key(), u.Elem())
	case *types.Struct:
		for f := range u.Fields() {
			add(elementPart, f.Type())
		}
	case *types.Signature:
		add(signaturePart, u.Params(), u.Results())
	case *types.Tuple:
		for v := range u.Variables() {
			add(signaturePart, v.Type())
		}
	case *types.Interface:
		for m := range u.ExplicitMethods() {
			add(methodPart, m.Type())
		}
		for e := range u.EmbeddedTypes() {
			add(methodPart, e)
		}
	case *types.Union:
		for i := range u.Len() {
			add(methodPart, u.Term(i).Type())
		}
	}
	return ps
}

type typePart struct {
	typ types.Type
	how part
}

// eachType calls visit on t and on every type a walk from t reaches, each
// once: into says which parts of a type the walk descends into, and visit
// whether to descend into the type it is given at all. A generic type not
// instantiated has no structure a value can have, and is never visited.
// [LAW:one-source-of-truth] the one walk of a type's parts; each caller
// chooses its descent.
func eachType(t types.Type, into func(types.Type, part) bool, visit func(types.Type) (descend bool)) {
	var seen typeutil.Map
	var walk func(types.Type)
	walk = func(t types.Type) {
		t = types.Unalias(t)
		if seen.At(t) != nil {
			return
		}
		seen.Set(t, true)
		if n, ok := t.(*types.Named); ok && n.TypeParams().Len() > 0 && n.TypeArgs().Len() == 0 {
			return
		}
		if !visit(t) {
			return
		}
		for _, p := range parts(t) {
			if into(t, p.how) {
				walk(p.typ)
			}
		}
	}
	walk(t)
}

// structural is the descent into a value's structure: a named type's
// underlying type, and each element, key and field. A signature's and an
// interface's types are not its structure: a visitor reads them as it
// needs.
func structural(_ types.Type, k part) bool { return k == underlyingPart || k == elementPart }

// holds reports whether a value of type t holds a value of type x: x is t,
// or is in t's structure.
func holds(t, x types.Type) bool {
	found := false
	eachType(t, structural, func(u types.Type) bool {
		found = found || types.Identical(u, x)
		return !found
	})
	return found
}

// structureOf is the structure of t, derived once per type.
func (a *analyzer) structureOf(t types.Type) *structure {
	if s, ok := a.structures.At(t).(*structure); ok {
		return s
	}
	s := &structure{}
	a.structures.Set(t, s)
	eachType(t, structural, func(t types.Type) bool {
		switch u := t.(type) {
		case *types.Named:
			if iface, ok := u.Underlying().(*types.Interface); ok {
				s.ifaces = append(s.ifaces, iface)
				return false
			}
			if a.moduleType(u) {
				s.methods = append(s.methods, a.methodsOf(u)...)
			} else {
				// Its pointer's method set, so a method on either receiver
				// counts: this approximation adds methods, never drops one.
				s.outside = append(s.outside, types.NewPointer(u))
			}
		case *types.Interface:
			s.ifaces = append(s.ifaces, u)
		}
		return true
	})
	return s
}

// carriage is what a call of a signature hands its callee, read off its
// argument types (receiver and parameters) and their structure (fields,
// elements, pointees): each function type there, and each method of each
// interface there, is a signature the callee can call; and an interface
// there can hold a function value, which reflection can call.
type carriage struct {
	sigs  []*types.Signature
	iface bool
}

// carried is the carriage of a call of sig, derived from types alone: a
// function stored in a field and handed over inside a struct is carried as
// surely as one passed directly.
func (a *analyzer) carried(sig *types.Signature) carriage {
	if c, ok := a.carriages.At(sig).(carriage); ok {
		return c
	}
	c := carriage{}
	visit := func(t types.Type) bool {
		switch u := t.(type) {
		case *types.Signature:
			c.sigs = append(c.sigs, u)
		case *types.Interface:
			c.iface = true
			for i := range u.NumMethods() {
				c.sigs = append(c.sigs, u.Method(i).Signature())
			}
		}
		return true
	}
	if recv := sig.Recv(); recv != nil {
		eachType(recv.Type(), structural, visit)
	}
	for i := range sig.Params().Len() {
		eachType(sig.Params().At(i).Type(), structural, visit)
	}
	a.carriages.Set(sig, c)
	return c
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

// methodLookup is what a type's method set holds under one name: a
// concreteMethod, an abstractMethod, or absentMethod.
// [LAW:types-are-the-program]
type methodLookup interface{ methodLookup() }

// concreteMethod is a method of a type that is not an interface: the
// program's function for it.
type concreteMethod struct{ fn *ssa.Function }

// abstractMethod is a method of an interface: a call of it is the
// interface call inv.
type abstractMethod struct{ inv invoke }

// absentMethod is no method of the name.
type absentMethod struct{}

func (concreteMethod) methodLookup() {}
func (abstractMethod) methodLookup() {}
func (absentMethod) methodLookup()   {}

// method is t's method called name.
func (a *analyzer) method(t types.Type, pkg *types.Package, name string) methodLookup {
	sel := a.prog.MethodSets.MethodSet(t).Lookup(pkg, name)
	if sel == nil {
		return absentMethod{}
	}
	if iface, ok := t.Underlying().(*types.Interface); ok {
		return abstractMethod{invoke{iface, sel.Obj().(*types.Func)}}
	}
	// go/ssa has no function for a generic type's method not instantiated,
	// which no value has.
	if fn := a.prog.MethodValue(sel); fn != nil {
		return concreteMethod{fn}
	}
	return absentMethod{}
}
