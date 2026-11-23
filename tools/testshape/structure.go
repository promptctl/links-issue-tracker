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
	return n != nil && n.Obj().Pkg() != nil && a.mod.contains(n.Obj().Pkg().Path())
}

// outsideType reports whether t, or what t points to, is a named concrete
// type declared outside the module.
func (a *analyzer) outsideType(t types.Type) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && !a.mod.contains(n.Obj().Pkg().Path()) && !types.IsInterface(n)
}

// moduleMethod reports whether the module declares the interface method m:
// a call of it is module code's, and an outside type converted to an
// interface whose complete method set holds it may have it called.
func (a *analyzer) moduleMethod(m *types.Func) bool {
	return m.Pkg() != nil && a.mod.contains(m.Pkg().Path())
}

// structure is what a converted value's structure contributes: every method
// of each module type among the type, its fields, elements and pointees;
// each outside named type among them; and each interface among them.
type structure struct {
	methods []*ssa.Function
	outside []types.Type
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

// method is t's method called name, if t has one.
func (a *analyzer) method(t types.Type, pkg *types.Package, name string) (*ssa.Function, bool) {
	sel := a.prog.MethodSets.MethodSet(t).Lookup(pkg, name)
	if sel == nil {
		return nil, false
	}
	fn := a.prog.MethodValue(sel)
	return fn, fn != nil
}
