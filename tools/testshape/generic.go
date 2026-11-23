package main

// What an outside generic does with its type arguments, read from its source.
//
// Code outside the module is never walked: the classification charges it.
// But an outside generic's instance is handed values of the types it is
// instantiated with, and a value of a type parameter T leaves the generic's
// body in two ways only: converted to an interface, as a type that mentions
// T (T, []T, a struct holding T, a func taking T); or handed to another
// generic instantiated over a type that mentions T. Code that is not generic
// cannot name T. A call of a constraint method is an interface call on T.
// So what the instance can do with a module type is decided by reading the
// generic's body, and the bodies of the generics it instantiates over T, in
// generic form: no value is followed.
//
// [LAW:one-source-of-truth] the source the build compiles is the one
// reading: nothing about a generic is recorded beside it, so no toolchain,
// version or invocation can make a reading stale.

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// genericSource reads the bodies of outside generics from the source files
// the build compiles, each package type-checked over the build's own
// packages: the types its bodies name are the module program's, but for
// the package's own declarations, which bind translates.
type genericSource struct {
	fset     *token.FileSet
	sizes    types.Sizes
	listed   map[string]*listedPackage // every outside package the build lists, by import path
	universe map[string]*types.Package // the module program's outside packages, by import path
	prog     *ssa.Program              // the read packages, over the universe's packages without bodies
	ctxt     *types.Context
	created  map[*types.Package]bool
	packages map[string]sourcePackage
	bodies   map[funcName]genericBody
}

// sourcePackage is an outside package read from source, or why it is not.
type sourcePackage struct {
	pkg     *types.Package
	problem string
}

func newGenericSource(ld *loader) *genericSource {
	listed := map[string]*listedPackage{}
	for id, p := range ld.byID {
		if ld.exported[id] != nil {
			listed[id] = p
		}
	}
	return &genericSource{
		fset:     ld.fset,
		sizes:    ld.sizes,
		listed:   listed,
		universe: ld.exported,
		prog:     ssa.NewProgram(ld.fset, 0),
		ctxt:     types.NewContext(),
		created:  map[*types.Package]bool{},
		packages: map[string]sourcePackage{},
		bodies:   map[funcName]genericBody{},
	}
}

// genericBody is what a generic's body does with its type parameters: a
// readBody, or an unreadBody saying why it could not be read.
// [LAW:types-are-the-program]
type genericBody interface{ genericBody() }

// readBody is a generic body's every way a value of a type parameter can
// leave it, in terms of its type parameters.
type readBody struct {
	params    []*types.TypeParam
	converts  []conversion     // of a type mentioning a type parameter
	calls     []constraintCall // of a method of a type parameter
	instances []genericCall    // of a generic, over a type mentioning a type parameter
}

// unreadBody is a body the reading cannot decide: why names the construct,
// or why there is no body to read.
type unreadBody struct{ why string }

func (readBody) genericBody()   {}
func (unreadBody) genericBody() {}

type constraintCall struct {
	param  *types.TypeParam
	method *types.Func
}

type genericCall struct {
	origin funcName
	args   []types.Type
}

// pkg reads the package at path from source, once.
func (g *genericSource) pkg(path string) sourcePackage {
	if sp, ok := g.packages[path]; ok {
		return sp
	}
	sp := g.read(path)
	g.packages[path] = sp
	return sp
}

func (g *genericSource) read(path string) sourcePackage {
	p := g.listed[path]
	if p == nil {
		return sourcePackage{problem: "the build lists no source for " + path}
	}
	pkg, files, info, problems := checkSource(g.fset, g.sizes, p, func(imp string) (*types.Package, error) {
		if imp == "unsafe" {
			return types.Unsafe, nil
		}
		id := imp
		if mapped, ok := p.ImportMap[imp]; ok {
			id = mapped
		}
		if pkg := g.universe[id]; pkg != nil {
			return pkg, nil
		}
		return nil, fmt.Errorf("%s imports %s, which the build did not load", path, id)
	})
	if len(problems) > 0 {
		return sourcePackage{problem: "its source does not type-check: " + problems[0]}
	}
	for _, imp := range pkg.Imports() {
		g.create(imp)
	}
	// Not importable: the universe's package of the same path is the one
	// every other package imports.
	g.prog.CreatePackage(pkg, files, info, false).Build()
	return sourcePackage{pkg: pkg}
}

// create puts a universe package, and what it imports, in the program,
// without bodies: a read body calls into them, never walks them.
func (g *genericSource) create(pkg *types.Package) {
	if g.created[pkg] {
		return
	}
	g.created[pkg] = true
	for _, imp := range pkg.Imports() {
		g.create(imp)
	}
	g.prog.CreatePackage(pkg, nil, nil, true)
}

// body reads the body of the generic function or method named origin,
// once.
func (g *genericSource) body(origin funcName) genericBody {
	if b, ok := g.bodies[origin]; ok {
		return b
	}
	b := g.readBody(origin)
	g.bodies[origin] = b
	return b
}

func (g *genericSource) readBody(origin funcName) genericBody {
	sp := g.pkg(origin.pkg)
	if sp.pkg == nil {
		return unreadBody{sp.problem}
	}
	obj := lookupFunc(sp.pkg, origin.key)
	if obj == nil {
		return unreadBody{"its source declares no " + origin.key}
	}
	fn := g.prog.FuncValue(obj)
	r := bodyReader{seen: map[*ssa.Function]bool{}}
	for tp := range fn.TypeParams().TypeParams() {
		r.body.params = append(r.body.params, tp)
	}
	r.scan(fn)
	if r.problem != "" {
		return unreadBody{r.problem}
	}
	return r.body
}

// lookupFunc is the function or method key declares in pkg: "Name" or
// "Type.Method".
func lookupFunc(pkg *types.Package, key string) *types.Func {
	typ, method, isMethod := strings.Cut(key, ".")
	if !isMethod {
		fn, _ := pkg.Scope().Lookup(key).(*types.Func)
		return fn
	}
	tn, _ := pkg.Scope().Lookup(typ).(*types.TypeName)
	if tn == nil {
		return nil
	}
	named, _ := tn.Type().(*types.Named)
	if named == nil {
		return nil
	}
	for m := range named.Methods() {
		if m.Name() == method {
			return m
		}
	}
	return nil
}

// bodyReader reads one generic's body: the function, the functions nested
// in it, and the thunks and wrappers it makes, all of which see its type
// parameters.
type bodyReader struct {
	body    readBody
	seen    map[*ssa.Function]bool
	problem string // the first construct the reading does not model
}

func (r *bodyReader) unread(format string, args ...any) {
	if r.problem == "" {
		r.problem = fmt.Sprintf(format, args...)
	}
}

func (r *bodyReader) scan(fn *ssa.Function) {
	if r.seen[fn] {
		return
	}
	r.seen[fn] = true
	if fn.Blocks == nil {
		r.unread("%s has no Go body", fn)
		return
	}
	for _, anon := range fn.AnonFuncs {
		r.scan(anon)
	}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			r.instr(instr)
		}
	}
}

func (r *bodyReader) instr(instr ssa.Instruction) {
	if c, ok := instr.(ssa.CallInstruction); ok {
		if common := c.Common(); common.IsInvoke() {
			if tp, ok := common.Value.Type().(*types.TypeParam); ok {
				r.body.calls = append(r.body.calls, constraintCall{tp, common.Method})
			}
		}
	}
	for _, op := range instr.Operands(nil) {
		if f, ok := (*op).(*ssa.Function); ok {
			r.function(f)
		}
	}
	if x, to, ok := converted(instr); ok && mentionsTypeParam(x) {
		switch {
		case isUnsafePointer(to):
			r.unread("it converts %s to unsafe.Pointer", x)
		case types.IsInterface(to) && !isTypeParam(to):
			r.record(x)
			r.body.converts = append(r.body.converts, conversion{x, to.Underlying().(*types.Interface)})
		}
	}
}

// function reads a function the body calls or takes as a value: an
// instance of a generic is a call of it; a thunk or wrapper, which sees the
// type parameters, is read as part of the body (go/ssa shares them across
// packages, so they have none). Any other function is not generic, so it
// cannot take a type parameter's value.
func (r *bodyReader) function(f *ssa.Function) {
	args := f.TypeArgs()
	switch {
	case len(args) > 0 && slices.ContainsFunc(args, mentionsTypeParam):
		name, declared := declaredName(f)
		if !declared {
			r.unread("it calls %s, which declares nothing", f)
			return
		}
		for _, t := range args {
			r.record(t)
		}
		r.body.instances = append(r.body.instances, genericCall{name, args})
	case len(args) == 0 && f.Synthetic != "" && f.Pkg == nil:
		r.scan(f)
	}
}

// record notes a type the reading will bind to type arguments: one that
// names a type its body declares cannot be bound outside the body.
func (r *bodyReader) record(t types.Type) {
	eachTypeDeep(t, func(t types.Type) bool {
		if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Parent() != n.Obj().Pkg().Scope() {
			r.unread("it declares type %s in its body", n.Obj().Name())
		}
		return true
	})
}

// converted is the operand type and result type of a conversion.
func converted(instr ssa.Instruction) (types.Type, types.Type, bool) {
	switch v := instr.(type) {
	case *ssa.MakeInterface:
		return v.X.Type(), v.Type(), true
	case *ssa.ChangeType:
		// In a generic body, a type parameter's value converted to an
		// interface is a ChangeType, since its underlying type is its
		// constraint.
		return v.X.Type(), v.Type(), true
	case *ssa.Convert:
		return v.X.Type(), v.Type(), true
	case *ssa.MultiConvert:
		return v.X.Type(), v.Type(), true
	}
	return nil, nil, false
}

func isTypeParam(t types.Type) bool {
	_, ok := t.(*types.TypeParam)
	return ok
}

func isUnsafePointer(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.UnsafePointer
}

func mentionsTypeParam(t types.Type) bool {
	found := false
	eachTypeDeep(t, func(t types.Type) bool {
		found = found || isTypeParam(t)
		return !found
	})
	return found
}

// eachTypeDeep calls visit on t and every type t is made of, each once: a
// type's arguments, elements, fields, parameters and results, an
// interface's methods and embedded types, and the underlying type of a type
// a function body declares, which can name the function's type parameters.
// visit says whether to go on.
func eachTypeDeep(t types.Type, visit func(types.Type) bool) {
	seen := map[types.Type]bool{}
	var walk func(types.Type) bool
	walk = func(t types.Type) bool {
		t = types.Unalias(t)
		if seen[t] {
			return true
		}
		seen[t] = true
		if !visit(t) {
			return false
		}
		var parts []types.Type
		switch u := t.(type) {
		case *types.Named:
			for a := range u.TypeArgs().Types() {
				parts = append(parts, a)
			}
			if o := u.Obj(); o.Pkg() != nil && o.Parent() != o.Pkg().Scope() {
				parts = append(parts, u.Underlying())
			}
		case *types.Pointer:
			parts = append(parts, u.Elem())
		case *types.Slice:
			parts = append(parts, u.Elem())
		case *types.Array:
			parts = append(parts, u.Elem())
		case *types.Chan:
			parts = append(parts, u.Elem())
		case *types.Map:
			parts = append(parts, u.Key(), u.Elem())
		case *types.Struct:
			for f := range u.Fields() {
				parts = append(parts, f.Type())
			}
		case *types.Tuple:
			for v := range u.Variables() {
				parts = append(parts, v.Type())
			}
		case *types.Signature:
			parts = append(parts, u.Params(), u.Results())
		case *types.Interface:
			for m := range u.ExplicitMethods() {
				parts = append(parts, m.Type())
			}
			for e := range u.EmbeddedTypes() {
				parts = append(parts, e)
			}
		case *types.Union:
			for i := range u.Len() {
				parts = append(parts, u.Term(i).Type())
			}
		}
		for _, p := range parts {
			if !walk(p) {
				return false
			}
		}
		return true
	}
	walk(t)
}

// bind is a read body's ways out, for an instance with type arguments
// args: each type parameter replaced by its argument, and each type the
// read package declares replaced by the universe's, so every type is the
// module program's.
func (g *genericSource) bind(b readBody, args []types.Type) (bound, error) {
	if len(args) != len(b.params) {
		return bound{}, fmt.Errorf("it has %d type parameters for %d type arguments", len(b.params), len(args))
	}
	s := substitution{g: g, args: map[*types.TypeParam]types.Type{}}
	for i, tp := range b.params {
		s.args[tp] = args[i]
	}
	var out bound
	for _, c := range b.converts {
		// A type that failed to substitute is invalid, not an interface:
		// s.err says why, and the reading is discarded.
		to, _ := s.of(c.to).Underlying().(*types.Interface)
		out.converts = append(out.converts, conversion{s.of(c.typ), to})
	}
	for _, c := range b.calls {
		out.calls = append(out.calls, boundCall{s.of(c.param), s.pkg(c.method.Pkg()), c.method.Name()})
	}
	for _, c := range b.instances {
		var targs []types.Type
		for _, a := range c.args {
			targs = append(targs, s.of(a))
		}
		out.instances = append(out.instances, instance{c.origin, targs})
	}
	return out, s.err
}

// bound is a read body's ways out for one instance, in the module
// program's types.
type bound struct {
	converts  []conversion
	calls     []boundCall
	instances []instance
}

// boundCall is a constraint method's call on a type argument.
type boundCall struct {
	typ    types.Type
	pkg    *types.Package
	method string
}

// substitution replaces type parameters by type arguments, and the read
// package's declarations by the universe's; err is the first type it could
// not make.
type substitution struct {
	g    *genericSource
	args map[*types.TypeParam]types.Type
	err  error
}

func (s *substitution) fail(format string, args ...any) types.Type {
	if s.err == nil {
		s.err = fmt.Errorf(format, args...)
	}
	return types.Typ[types.Invalid]
}

// pkg is the universe's package of p's path: a read package's names are the
// build's.
func (s *substitution) pkg(p *types.Package) *types.Package {
	if p == nil {
		return nil
	}
	if u := s.g.universe[p.Path()]; u != nil {
		return u
	}
	return p
}

func (s *substitution) of(t types.Type) types.Type {
	switch t := types.Unalias(t).(type) {
	case *types.TypeParam:
		if a, ok := s.args[t]; ok {
			return a
		}
		return s.fail("type parameter %s is not bound", t)
	case *types.Basic:
		return t
	case *types.Named:
		origin := t.Origin()
		if obj := origin.Obj(); obj.Pkg() != nil && s.pkg(obj.Pkg()) != obj.Pkg() {
			tn, _ := s.pkg(obj.Pkg()).Scope().Lookup(obj.Name()).(*types.TypeName)
			if tn == nil {
				return s.fail("the build's %s has no type %s", obj.Pkg().Path(), obj.Name())
			}
			origin = tn.Type().(*types.Named)
		}
		if t.TypeArgs().Len() == 0 {
			return origin
		}
		var args []types.Type
		for a := range t.TypeArgs().Types() {
			args = append(args, s.of(a))
		}
		inst, err := types.Instantiate(s.g.ctxt, origin, args, false)
		if err != nil {
			return s.fail("%v", err)
		}
		return inst
	case *types.Pointer:
		return types.NewPointer(s.of(t.Elem()))
	case *types.Slice:
		return types.NewSlice(s.of(t.Elem()))
	case *types.Array:
		return types.NewArray(s.of(t.Elem()), t.Len())
	case *types.Chan:
		return types.NewChan(t.Dir(), s.of(t.Elem()))
	case *types.Map:
		return types.NewMap(s.of(t.Key()), s.of(t.Elem()))
	case *types.Struct:
		var fields []*types.Var
		var tags []string
		for i := range t.NumFields() {
			f := t.Field(i)
			fields = append(fields, types.NewField(f.Pos(), s.pkg(f.Pkg()), f.Name(), s.of(f.Type()), f.Embedded()))
			tags = append(tags, t.Tag(i))
		}
		return types.NewStruct(fields, tags)
	case *types.Signature:
		return types.NewSignatureType(nil, nil, nil, s.tuple(t.Params()), s.tuple(t.Results()), t.Variadic())
	case *types.Interface:
		var methods []*types.Func
		for m := range t.ExplicitMethods() {
			methods = append(methods, types.NewFunc(m.Pos(), s.pkg(m.Pkg()), m.Name(), s.of(m.Signature()).(*types.Signature)))
		}
		var embedded []types.Type
		for e := range t.EmbeddedTypes() {
			embedded = append(embedded, s.of(e))
		}
		return types.NewInterfaceType(methods, embedded).Complete()
	}
	return s.fail("its body converts %s, which the reading does not model", t)
}

func (s *substitution) tuple(t *types.Tuple) *types.Tuple {
	var vars []*types.Var
	for v := range t.Variables() {
		vars = append(vars, types.NewParam(v.Pos(), s.pkg(v.Pkg()), v.Name(), s.of(v.Type())))
	}
	return types.NewTuple(vars...)
}

// instance is an outside generic's origin, by the name the classification
// gives it, with type arguments in the module program's types.
type instance struct {
	origin funcName
	args   []types.Type
}

// instanceOf is the instance an outside function is, false for a function
// with no type arguments: it is handed no type.
func (a *analyzer) instanceOf(fn *ssa.Function) (instance, bool) {
	args := fn.TypeArgs()
	return instance{a.owner(fn).name, args}, len(args) > 0
}

// decision is what an outside generic's instance does with its type
// arguments, through its own body and the instances it makes over them,
// each with the instantiation that decided it.
type decision struct {
	converts []decided[conversion]
	calls    []decided[boundCall]
	// withheld names the instance when it converts none of its type
	// arguments and calls no method of one, though one could reach module
	// code.
	withheld []string
}

type decided[T any] struct {
	what T
	why  string
}

// instanceKey is an instance's key: one per origin and type arguments,
// identical types sharing it.
type instanceKey struct {
	origin funcName
	args   string
}

func (a *analyzer) instanceKey(in instance) instanceKey {
	keys := make([]string, len(in.args))
	for i, t := range in.args {
		keys[i] = fmt.Sprint(a.typeKey(t))
	}
	return instanceKey{in.origin, strings.Join(keys, ",")}
}

// instanceName names an instance for a witness, relative to the module.
func (a *analyzer) instanceName(in instance) string {
	q := relativeTo(a.mod)
	args := make([]string, len(in.args))
	for i, t := range in.args {
		args[i] = types.TypeString(t, q)
	}
	return strings.TrimPrefix(in.origin.String(), a.mod.Path+"/") + "[" + strings.Join(args, ", ") + "]"
}

// reading is one instance's own body bound to its type arguments: what it
// converts and calls, each with what decided it, and the instances it
// makes. An unreadable body converts every type argument to an interface,
// which is what module code converting it reaches: the conservative
// default. [LAW:no-silent-failure] the witness says why it was not read.
type reading struct {
	name      string
	converts  []decided[conversion]
	calls     []decided[boundCall]
	instances []instance
}

func (a *analyzer) read(in instance) *reading {
	key := a.instanceKey(in)
	if r, ok := a.readings[key]; ok {
		return r
	}
	r := a.readInstance(in)
	a.readings[key] = r
	return r
}

func (a *analyzer) readInstance(in instance) *reading {
	q := relativeTo(a.mod)
	r := &reading{name: a.instanceName(in)}
	var why string
	switch b := a.generics.body(in.origin).(type) {
	case unreadBody:
		why = b.why
	case readBody:
		out, err := a.generics.bind(b, in.args)
		if err == nil {
			for _, c := range out.converts {
				r.converts = append(r.converts, decided[conversion]{c, "converts " + types.TypeString(c.typ, q) + " to an interface"})
			}
			for _, c := range out.calls {
				r.calls = append(r.calls, decided[boundCall]{c, "calls " + c.method + " of " + types.TypeString(c.typ, q) + " through its constraint"})
			}
			r.instances = out.instances
			return r
		}
		why = err.Error()
	}
	for _, t := range in.args {
		r.converts = append(r.converts, decided[conversion]{conversion{t, anyInterface}, "cannot be read (" + why + "), so " + types.TypeString(t, q) + " is converted to an interface"})
	}
	return r
}

// anyInterface is the interface an unreadable generic's type argument is
// converted to.
var anyInterface = types.NewInterfaceType(nil, nil)

// decide is what in does with its type arguments: its own reading, and the
// reading of every instance it makes, transitively, each why naming the
// instantiations from in to the one that decided it.
func (a *analyzer) decide(in instance) *decision {
	key := a.instanceKey(in)
	if d, ok := a.decisions[key]; ok {
		return d
	}
	d := &decision{}
	type pending struct {
		r      *reading
		prefix string // the instantiations from in to r
	}
	root := a.read(in)
	seen := map[instanceKey]bool{key: true}
	queue := []pending{{root, root.name}}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range p.r.converts {
			d.converts = append(d.converts, decided[conversion]{c.what, p.prefix + " " + c.why})
		}
		for _, c := range p.r.calls {
			d.calls = append(d.calls, decided[boundCall]{c.what, p.prefix + " " + c.why})
		}
		for _, sub := range p.r.instances {
			if k := a.instanceKey(sub); !seen[k] {
				seen[k] = true
				r := a.read(sub)
				queue = append(queue, pending{r, p.prefix + " instantiates " + r.name + ", which"})
			}
		}
	}
	if len(d.converts) == 0 && len(d.calls) == 0 && slices.ContainsFunc(in.args, a.couldReachModuleCode) {
		d.withheld = []string{root.name + " neither converts a type argument nor calls a method of one"}
	}
	a.decisions[key] = d
	return d
}

// couldReachModuleCode reports whether converting t to an interface would
// reach anything: a method of a module type in its structure, or an
// interface there, which holds values converted elsewhere.
func (a *analyzer) couldReachModuleCode(t types.Type) bool {
	s := a.structureOf(t)
	return len(s.methods) > 0 || len(s.ifaces) > 0
}
