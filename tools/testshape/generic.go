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
// [LAW:one-source-of-truth] exception: export data carries no generic
// bodies, so an outside package whose generic the module instantiates is
// type-checked a second time, from the source files the build compiles, for
// its bodies alone. Every type a reading yields is the export-data copy's,
// which the rest of the program sees: the second copy's declarations are
// translated to the first's by path and name, and a declaration the first
// lacks is the tool's inconsistency, which refuses the run. Nothing about a
// generic is recorded beside its source, so no toolchain, version or
// invocation can make a reading stale.

import (
	"fmt"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// genericSource reads the bodies of outside generics from the source files
// the build compiles, each package type-checked over the build's own
// packages, its imports resolved as the loader resolves them.
type genericSource struct {
	ld       *loader
	prog     *ssa.Program // the read packages, over the build's packages without bodies
	pkgs     *programPackages
	ctxt     *types.Context
	packages map[string]sourcePackage
	bodies   map[funcName]genericBody
}

func newGenericSource(ld *loader) *genericSource {
	prog := ssa.NewProgram(ld.fset, 0)
	return &genericSource{
		ld:       ld,
		prog:     prog,
		pkgs:     newProgramPackages(prog),
		ctxt:     types.NewContext(),
		packages: map[string]sourcePackage{},
		bodies:   map[funcName]genericBody{},
	}
}

// unread is what the reading cannot decide, and why: a package with no
// source that type-checks, a body it does not model, an instance whose body
// is either.
type unread struct{ why string }

// sourcePackage is an outside package read from source, or unread.
// [LAW:types-are-the-program]
type sourcePackage interface{ sourcePackage() }

type sourceRead struct{ pkg *types.Package }

func (sourceRead) sourcePackage() {}
func (unread) sourcePackage()     {}

// genericBody is what a generic's body does with its type parameters: a
// readBody, or unread. instantiate is its reading for an instance with
// type arguments args.
type genericBody interface {
	instantiate(g *genericSource, args []types.Type) (reading, error)
}

// readBody is a generic body's every way a value of a type parameter can
// leave it, in terms of its type parameters.
type readBody struct {
	params []*types.TypeParam
	uses
}

// uses is every way a value of a type argument leaves a body: converted to
// an interface, as a type mentioning one; a constraint method called on
// one; handed to an instance of a generic over a type mentioning one.
type uses struct {
	converts  []conversion
	calls     []constraintCall
	instances []instance
}

func (b readBody) instantiate(g *genericSource, args []types.Type) (reading, error) {
	u, err := g.bind(b, args)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (u unread) instantiate(*genericSource, []types.Type) (reading, error) { return u, nil }

// constraintCall is a call of the method named method, of package pkg, on
// a value of type typ through its constraint.
type constraintCall struct {
	typ    types.Type
	pkg    *types.Package
	method string
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
	p := g.ld.byID[path]
	if p == nil || g.ld.exported[path] == nil {
		return unread{"the build lists no source for " + path}
	}
	pkg, files, info, problems := checkSource(g.ld.fset, g.ld.sizes, p, func(imp string) (*types.Package, error) { return g.ld.imported(p, imp) })
	if len(problems) > 0 {
		return unread{"its source does not type-check: " + strings.Join(problems, "; ")}
	}
	// Not importable: the export-data package of the same path is the one
	// every other package imports.
	g.pkgs.fromSource(pkg, files, info, false).Build()
	return sourceRead{pkg}
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
	var pkg *types.Package
	switch sp := g.pkg(origin.pkg).(type) {
	case unread:
		return sp
	case sourceRead:
		pkg = sp.pkg
	}
	obj := lookupFunc(pkg, origin.key)
	if obj == nil {
		return unread{"its source declares no " + origin.key}
	}
	fn := g.prog.FuncValue(obj)
	r := bodyReader{seen: map[*ssa.Function]bool{}}
	for tp := range fn.TypeParams().TypeParams() {
		r.body.params = append(r.body.params, tp)
	}
	r.scan(fn)
	if r.problem != "" {
		return unread{r.problem}
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
				r.body.calls = append(r.body.calls, constraintCall{tp, common.Method.Pkg(), common.Method.Name()})
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
		r.body.instances = append(r.body.instances, instance{name, args})
	case len(args) == 0 && f.Synthetic != "" && f.Pkg == nil:
		r.scan(f)
	}
}

// record notes a type the reading will bind to type arguments: one that
// names a type its body declares cannot be bound outside the body.
func (r *bodyReader) record(t types.Type) {
	eachType(t, inBody, func(t types.Type) bool {
		if n, ok := t.(*types.Named); ok && declaredInBody(n.Obj()) {
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
	eachType(t, inBody, func(t types.Type) bool {
		found = found || isTypeParam(t)
		return !found
	})
	return found
}

// inBody is the descent into every type a generic body's type is made of
// that can name its type parameters: its arguments, elements, fields,
// parameters and results, an interface's methods and embedded types; and the
// underlying type of a type the body declares, but of no other named type,
// whose declaration is outside the body.
func inBody(t types.Type, k part) bool {
	if k == underlyingPart {
		return declaredInBody(t.(*types.Named).Obj())
	}
	return true
}

// declaredInBody reports whether a function body declares the type named
// obj.
func declaredInBody(obj *types.TypeName) bool {
	return obj.Pkg() != nil && obj.Parent() != obj.Pkg().Scope()
}

// bind is a read body's uses for an instance with type arguments args: each
// type parameter replaced by its argument, and each type the read package
// declares replaced by the export-data copy's, so every type is the module
// program's. An error is the tool's inconsistency: the instance compiled.
func (g *genericSource) bind(b readBody, args []types.Type) (uses, error) {
	if len(args) != len(b.params) {
		return uses{}, fmt.Errorf("it has %d type parameters for %d type arguments", len(b.params), len(args))
	}
	s := substitution{g: g, args: map[*types.TypeParam]types.Type{}}
	for i, tp := range b.params {
		s.args[tp] = args[i]
	}
	var out uses
	for _, c := range b.converts {
		typ, err := s.of(c.typ)
		if err != nil {
			return uses{}, err
		}
		to, err := s.iface(c.to)
		if err != nil {
			return uses{}, err
		}
		out.converts = append(out.converts, conversion{typ, to})
	}
	for _, c := range b.calls {
		typ, err := s.of(c.typ)
		if err != nil {
			return uses{}, err
		}
		pkg, err := s.pkg(c.pkg)
		if err != nil {
			return uses{}, err
		}
		out.calls = append(out.calls, constraintCall{typ, pkg, c.method})
	}
	for _, in := range b.instances {
		targs, err := s.list(in.args)
		if err != nil {
			return uses{}, err
		}
		out.instances = append(out.instances, instance{in.origin, targs})
	}
	return out, nil
}

// substitution replaces type parameters by type arguments, and the read
// package's declarations by the export-data copy's.
type substitution struct {
	g    *genericSource
	args map[*types.TypeParam]types.Type
}

// pkg is the export-data copy of p, which every other package imports.
func (s *substitution) pkg(p *types.Package) (*types.Package, error) {
	if p == nil {
		return nil, nil
	}
	u := s.g.ld.exported[p.Path()]
	if u == nil {
		return nil, fmt.Errorf("its source names %s, which the build's export data does not hold", p.Path())
	}
	return u, nil
}

func (s *substitution) of(t types.Type) (types.Type, error) {
	switch t := types.Unalias(t).(type) {
	case *types.TypeParam:
		if a, ok := s.args[t]; ok {
			return a, nil
		}
		return nil, fmt.Errorf("type parameter %s is not bound", t)
	case *types.Basic:
		return t, nil
	case *types.Named:
		origin, err := s.named(t.Origin())
		if err != nil || t.TypeArgs().Len() == 0 {
			return origin, err
		}
		args, err := s.list(slices.Collect(t.TypeArgs().Types()))
		if err != nil {
			return nil, err
		}
		return types.Instantiate(s.g.ctxt, origin, args, false)
	case *types.Pointer:
		elem, err := s.of(t.Elem())
		return types.NewPointer(elem), err
	case *types.Slice:
		elem, err := s.of(t.Elem())
		return types.NewSlice(elem), err
	case *types.Array:
		elem, err := s.of(t.Elem())
		return types.NewArray(elem, t.Len()), err
	case *types.Chan:
		elem, err := s.of(t.Elem())
		return types.NewChan(t.Dir(), elem), err
	case *types.Map:
		kv, err := s.list([]types.Type{t.Key(), t.Elem()})
		if err != nil {
			return nil, err
		}
		return types.NewMap(kv[0], kv[1]), nil
	case *types.Struct:
		var fields []*types.Var
		var tags []string
		for i := range t.NumFields() {
			f := t.Field(i)
			pkg, err := s.pkg(f.Pkg())
			if err != nil {
				return nil, err
			}
			typ, err := s.of(f.Type())
			if err != nil {
				return nil, err
			}
			fields = append(fields, types.NewField(f.Pos(), pkg, f.Name(), typ, f.Embedded()))
			tags = append(tags, t.Tag(i))
		}
		return types.NewStruct(fields, tags), nil
	case *types.Signature:
		return s.signature(t)
	case *types.Interface:
		return s.iface(t)
	}
	return nil, fmt.Errorf("its body converts %s, a type the reading does not model", t)
}

// named is the export-data copy's declaration of the named type n, when
// the read package declares n.
func (s *substitution) named(n *types.Named) (*types.Named, error) {
	obj := n.Obj()
	pkg, err := s.pkg(obj.Pkg())
	if err != nil || pkg == obj.Pkg() {
		return n, err
	}
	tn, ok := pkg.Scope().Lookup(obj.Name()).(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("its source declares type %s.%s, which the build's export data does not", obj.Pkg().Path(), obj.Name())
	}
	named, ok := tn.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("its source declares %s.%s a defined type, which the build's export data does not", obj.Pkg().Path(), obj.Name())
	}
	return named, nil
}

func (s *substitution) list(ts []types.Type) ([]types.Type, error) {
	out := make([]types.Type, len(ts))
	for i, t := range ts {
		u, err := s.of(t)
		if err != nil {
			return nil, err
		}
		out[i] = u
	}
	return out, nil
}

func (s *substitution) signature(t *types.Signature) (*types.Signature, error) {
	params, err := s.tuple(t.Params())
	if err != nil {
		return nil, err
	}
	results, err := s.tuple(t.Results())
	if err != nil {
		return nil, err
	}
	return types.NewSignatureType(nil, nil, nil, params, results, t.Variadic()), nil
}

func (s *substitution) iface(t *types.Interface) (*types.Interface, error) {
	var methods []*types.Func
	for m := range t.ExplicitMethods() {
		pkg, err := s.pkg(m.Pkg())
		if err != nil {
			return nil, err
		}
		sig, err := s.signature(m.Signature())
		if err != nil {
			return nil, err
		}
		methods = append(methods, types.NewFunc(m.Pos(), pkg, m.Name(), sig))
	}
	embedded, err := s.list(slices.Collect(t.EmbeddedTypes()))
	if err != nil {
		return nil, err
	}
	return types.NewInterfaceType(methods, embedded).Complete(), nil
}

func (s *substitution) tuple(t *types.Tuple) (*types.Tuple, error) {
	var vars []*types.Var
	for v := range t.Variables() {
		pkg, err := s.pkg(v.Pkg())
		if err != nil {
			return nil, err
		}
		typ, err := s.of(v.Type())
		if err != nil {
			return nil, err
		}
		vars = append(vars, types.NewParam(v.Pos(), pkg, v.Name(), typ))
	}
	return types.NewTuple(vars...), nil
}

// instance is a generic's instance: its origin, by the name the
// classification gives it, and its type arguments, in the types of the
// program it is read from.
type instance struct {
	origin funcName
	args   []types.Type
}

// instanceOf is the instance an outside function is, false for a function
// with no type arguments: it is handed no type.
func (a *analyzer) instanceOf(fn *ssa.Function) (instance, bool) {
	args := typeArgsOf(fn)
	return instance{a.owner(fn).name, args}, len(args) > 0
}

// typeArgsOf is fn's type arguments. A method's are its receiver type's: a
// bound method value or a method expression of an instantiated type is a
// wrapper that carries none itself, and Go methods have no type parameters
// of their own.
func typeArgsOf(fn *ssa.Function) []types.Type {
	if obj, ok := fn.Object().(*types.Func); ok {
		if recv := obj.Signature().Recv(); recv != nil {
			if n := namedOf(recv.Type()); n != nil {
				return slices.Collect(n.TypeArgs().Types())
			}
		}
	}
	return fn.TypeArgs()
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

// reading is an instance's own body in the module program's types: its
// uses, or unread. [LAW:types-are-the-program]
type reading interface{ reading() }

func (uses) reading()   {}
func (unread) reading() {}

// genericInstance is what the analyzer knows of one instance: its reading,
// and, once decided, its decision.
type genericInstance struct {
	in       instance
	key      instanceKey
	reading  reading
	decision *decision
}

// generic is in's entry, its reading read once.
func (a *analyzer) generic(in instance) *genericInstance {
	key := a.instanceKey(in)
	if gi, ok := a.instances[key]; ok {
		return gi
	}
	gi := &genericInstance{in: in, key: key, reading: a.readInstance(in)}
	a.instances[key] = gi
	return gi
}

// readInstance binds in's body to its type arguments. A body that does not
// bind is the tool's inconsistency, which refuses the run: the instance
// reads as using nothing only so the walk can finish and say so.
func (a *analyzer) readInstance(in instance) reading {
	r, err := a.generics.body(in.origin).instantiate(a.generics, in.args)
	if err != nil {
		a.inconsistent("reading %s: %v", namer{a.mod}.instance(in), err)
		return uses{}
	}
	return r
}

// decision is what an instance does with its type arguments, through its
// own body and every instance it makes over them, transitively: each use,
// as the type argument reach it is; and each type argument it withholds.
type decision struct {
	uses     []typeArgument
	withheld []withholding
}

// withholding is a type argument, the arg'th of in, that no use converts
// or calls a method of, though converting it would reach module code.
type withholding struct {
	in  instance
	arg int
}

// withheldKey is a withholding's key: identical instances share it.
type withheldKey struct {
	instance instanceKey
	arg      int
}

// decide is what in does with its type arguments, decided once.
func (a *analyzer) decide(in instance) *decision {
	root := a.generic(in)
	if root.decision != nil {
		return root.decision
	}
	d := &decision{}
	type pending struct {
		gi   *genericInstance
		path []instance // from in to gi's instance
	}
	seen := map[instanceKey]bool{root.key: true}
	queue := []pending{{root, []instance{in}}}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		switch r := p.gi.reading.(type) {
		case unread:
			// The conservative reading: every type argument converted, the
			// reach module code converting it gets, which errs away from
			// unit. [LAW:no-silent-failure] the witness says why the body
			// was not read, and the output counts the instance with why.
			for _, t := range p.gi.in.args {
				d.uses = append(d.uses, typeArgument{p.path, unreadConversion{r.why, t}})
			}
		case uses:
			for _, c := range r.converts {
				d.uses = append(d.uses, typeArgument{p.path, c})
			}
			for _, c := range r.calls {
				d.uses = append(d.uses, typeArgument{p.path, c})
			}
			for _, sub := range r.instances {
				gi := a.generic(sub)
				if !seen[gi.key] {
					seen[gi.key] = true
					queue = append(queue, pending{gi, append(slices.Clip(p.path), sub)})
				}
			}
		}
	}
	for i, t := range in.args {
		used := slices.ContainsFunc(d.uses, func(u typeArgument) bool { return holds(u.use.argType(), t) })
		if !used && a.couldReachModuleCode(t) {
			d.withheld = append(d.withheld, withholding{in, i})
		}
	}
	root.decision = d
	return d
}

// argUse is one use of a type argument's value: a conversion, a
// constraintCall, or an unreadConversion.
type argUse interface {
	argType() types.Type // the type of the value used
	text(n namer) string
}

// unreadConversion is the conversion of typ to an interface that an
// unread body is taken to make.
type unreadConversion struct {
	why string
	typ types.Type
}

func (c conversion) argType() types.Type       { return c.typ }
func (c constraintCall) argType() types.Type   { return c.typ }
func (c unreadConversion) argType() types.Type { return c.typ }

// anyInterface is the interface an unread body's type argument is
// converted to.
var anyInterface = types.NewInterfaceType(nil, nil)

// couldReachModuleCode reports whether converting t to an interface would
// reach anything: a method of a module type in its structure, or an
// interface there, which holds values converted elsewhere.
func (a *analyzer) couldReachModuleCode(t types.Type) bool {
	s := a.structureOf(t)
	return len(s.methods) > 0 || len(s.ifaces) > 0
}

// unreadInstances is every instance the analyzer read whose body could not
// be read, with why.
func (a *analyzer) unreadInstances() []unreadInstance {
	var out []unreadInstance
	for _, gi := range a.instances {
		if u, ok := gi.reading.(unread); ok {
			out = append(out, unreadInstance{gi.in, u.why})
		}
	}
	return out
}

// unreadInstance is an instance whose body could not be read, so that every
// type argument counts as converted.
type unreadInstance struct {
	in  instance
	why string
}
