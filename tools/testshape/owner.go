package main

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// knowledge is what the measure knows of a function's code: module code,
// which it walks; an outside function's Call, which it charges; or
// unclassified, which refuses the run. [LAW:types-are-the-program] an
// unclassified function has no Effect to read as touch-free.
type knowledge interface{ knowledge() }

type moduleCode struct{}

type unclassified struct{}

func (moduleCode) knowledge()   {}
func (unclassified) knowledge() {}
func (Effect) knowledge()       {}
func (SpawnArg) knowledge()     {}

// owner is where a function's code lives, by the name the classification
// gives it, and what the measure knows of that code. A closure belongs to
// its enclosing function; an instance of a generic function to its origin; a
// method, or a wrapper that calls one, to its receiver's type; a package's
// initializer to its package, as "init". A wrapper around an interface
// method is module glue: its body is an interface call, which the walk
// follows.
type owner struct {
	name  funcName
	knows knowledge
}

// walked reports whether fn is module code, which a walk follows into.
func (a *analyzer) walked(fn *ssa.Function) bool {
	_, ok := a.owner(fn).knows.(moduleCode)
	return ok
}

// owner is derived once per function: every walk asks it of every function
// it reaches.
func (a *analyzer) owner(fn *ssa.Function) owner {
	if o, ok := a.owners[fn]; ok {
		return o
	}
	name, inModule := a.nameOf(fn)
	o := owner{name: name, knows: moduleCode{}}
	switch {
	case inModule:
	case name.key == "init":
		o.knows = a.class.initOf(name.pkg, a.moduleOf[name.pkg])
	default:
		o.knows = a.class.Lookup(name.pkg, name.key)
	}
	a.owners[fn] = o
	return o
}

func (a *analyzer) nameOf(fn *ssa.Function) (funcName, bool) {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if o := fn.Origin(); o != nil {
		fn = o
	}
	if recv := fn.Signature.Recv(); recv != nil && fn.Synthetic != "" {
		// A promotion wrapper: the method belongs to the outer type.
		return a.methodName(recv.Type(), fn.Name())
	}
	if obj, ok := fn.Object().(*types.Func); ok && obj != nil {
		if recv := obj.Signature().Recv(); recv != nil {
			if types.IsInterface(recv.Type()) {
				return funcName{}, true
			}
			return a.methodName(recv.Type(), obj.Name())
		}
		if obj.Pkg() != nil {
			return a.nameIn(obj.Pkg().Path(), obj.Name())
		}
	}
	return a.nameIn(fn.Pkg.Pkg.Path(), fn.Name())
}

func (a *analyzer) methodName(recv types.Type, method string) (funcName, bool) {
	named := namedOf(recv)
	if named == nil || named.Obj().Pkg() == nil {
		return funcName{}, true
	}
	return a.nameIn(named.Obj().Pkg().Path(), named.Obj().Name()+"."+method)
}

func (a *analyzer) nameIn(pkg, key string) (funcName, bool) {
	return funcName{pkg, key}, a.mod.contains(pkg)
}

// interfaceMethodName names an interface method by the interface that
// declares it ("TB.TempDir", "Writer.Write"); the universe's error is
// builtin's ("error.Error").
func interfaceMethodName(m *types.Func) funcName {
	pkg := builtin
	if m.Pkg() != nil {
		pkg = m.Pkg().Path()
	}
	if named := namedOf(m.Signature().Recv().Type()); named != nil {
		return funcName{pkg, named.Obj().Name() + "." + m.Name()}
	}
	return funcName{pkg, m.Name()}
}

func namedOf(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
}
