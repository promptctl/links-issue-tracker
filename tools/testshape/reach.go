package main

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	iofs "io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// analyzer reads a module's source and answers, for a top-level test, which
// touches the code it reaches has. It parses only: no type checking, no build,
// no subprocess, so it runs on any checkout without the toolchain's cgo setup.
//
// The graph is by identifier. A test reaches every function and package-level
// variable it names, called or passed as a value, and their bodies and
// initializers in turn, across every package of the module. A method call
// `x.M()` reaches every method named M declared in the calling package and in
// the module packages its file imports, since without types the receiver is
// unknown. Both choices over-reach rather than under-reach: a test counted
// integration that is not costs a look, a test counted unit that is not
// defeats the measurement. What the walk cannot see is a call through an
// interface value or a function value it never names.
type analyzer struct {
	fsys    iofs.FS
	modPath string
	fset    *token.FileSet
	bctx    build.Context
	pkgs    map[pkgKey]*pkg
}

type pkgKey struct {
	path  string
	tests bool // the package's _test.go files are included
}

// pkg is one directory's parsed Go files: a package with or without its tests.
// Both the package and its external _test package live in one pkg, because
// both reach the same helpers.
type pkg struct {
	key     pkgKey
	name    string
	funcs   map[string][]*fn // package-level functions and variables by name
	methods map[string][]*fn // methods by method name
	consts  map[string]string
	// fields are the names of every struct field the package declares, its
	// function-local and anonymous structs included.
	fields map[string]bool
}

// fn is one node of the graph: a function, a method, or a package-level
// variable, whose body is its initializer.
type fn struct {
	name   string // qualified for display: internal/cli.runCLIInDir
	pkg    *pkg
	file   *file
	body   ast.Node
	ftype  *ast.FuncType // nil for a variable
	recv   *ast.FieldList
	params []string // positional parameter names, receiver excluded

	scanned bool
	edges   []edge
	// spawnParams are the indices of parameters this function hands, directly
	// or through a callee, to the argument naming a child process's program.
	spawnParams map[int]bool
	// assigns are the package-level variables this function sets: a test's
	// seam, replacing the variable's initializer with a stand-in.
	assigns  []*fn
	scanning bool
	// locals are the names the function declares inside itself, which hide
	// the package's names of the same spelling. Nil until first needed.
	locals map[string]bool
}

func (f *fn) isVar() bool {
	_, ok := f.body.(*ast.ValueSpec)
	return ok
}

type file struct {
	imports map[string]string // local name -> import path
	test    bool              // a _test.go file
}

// edge is one step out of a fn: to another fn, or to a touch.
type edge struct {
	to    *fn
	touch touch
	label string // the marker reached, for a touch edge
}

func newAnalyzer(fsys iofs.FS, modPath string) *analyzer {
	bctx := build.Default
	bctx.JoinPath = path.Join
	bctx.OpenFile = func(p string) (io.ReadCloser, error) { return fsys.Open(p) }
	return &analyzer{fsys: fsys, modPath: modPath, fset: token.NewFileSet(), bctx: bctx, pkgs: map[pkgKey]*pkg{}}
}

// inModule reports whether importPath is a package of the measured module,
// and its directory relative to the module root.
func (a *analyzer) inModule(importPath string) (string, bool) {
	if importPath == a.modPath {
		return ".", true
	}
	rel, ok := strings.CutPrefix(importPath, a.modPath+"/")
	return rel, ok
}

// markerKey is the key funcMarkers lists a function under.
func (a *analyzer) markerKey(importPath, name string) string {
	if rel, ok := a.inModule(importPath); ok {
		return "./" + rel + "." + name
	}
	return importPath + "." + name
}

// load parses the package at importPath, which must be in the module. A
// directory with no Go files the build would compile is an empty package.
func (a *analyzer) load(key pkgKey) (*pkg, error) {
	if p, ok := a.pkgs[key]; ok {
		return p, nil
	}
	dir, _ := a.inModule(key.path)
	p := &pkg{key: key, funcs: map[string][]*fn{}, methods: map[string][]*fn{}, consts: map[string]string{}, fields: map[string]bool{}}
	a.pkgs[key] = p
	entries, err := iofs.ReadDir(a.fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading package %s: %w", key.path, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || (!key.tests && strings.HasSuffix(name, "_test.go")) {
			continue
		}
		match, err := a.bctx.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("reading build constraints of %s: %w", path.Join(dir, name), err)
		}
		if !match {
			continue
		}
		if err := a.parse(p, path.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (a *analyzer) parse(p *pkg, filename string) error {
	src, err := iofs.ReadFile(a.fsys, filename)
	if err != nil {
		return err
	}
	af, err := parser.ParseFile(a.fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", filename, err)
	}
	if p.name == "" || !strings.HasSuffix(af.Name.Name, "_test") {
		p.name = af.Name.Name
	}
	f := &file{imports: map[string]string{}, test: strings.HasSuffix(filename, "_test.go")}
	for _, imp := range af.Imports {
		ipath, _ := strconv.Unquote(imp.Path.Value)
		local, err := a.importName(imp, ipath)
		if err != nil {
			return err
		}
		f.imports[local] = ipath
	}
	rel, _ := a.inModule(p.key.path)
	qualify := func(name string) string {
		if rel == "." {
			return name
		}
		return rel + "." + name
	}
	ast.Inspect(af, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok {
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					p.fields[name.Name] = true
				}
			}
		}
		return true
	})
	for _, d := range af.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			node := &fn{pkg: p, file: f, body: d.Body, ftype: d.Type, recv: d.Recv, params: paramNames(d.Type)}
			if d.Recv == nil {
				node.name = qualify(d.Name.Name)
				p.funcs[d.Name.Name] = append(p.funcs[d.Name.Name], node)
				continue
			}
			node.name = qualify(recvName(d.Recv) + "." + d.Name.Name)
			p.methods[d.Name.Name] = append(p.methods[d.Name.Name], node)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if d.Tok == token.CONST {
						if i < len(vs.Values) {
							if s, ok := stringLit(vs.Values[i]); ok {
								p.consts[id.Name] = s
							}
						}
						continue
					}
					if len(vs.Values) == 0 {
						continue
					}
					p.funcs[id.Name] = append(p.funcs[id.Name], &fn{name: qualify(id.Name), pkg: p, file: f, body: vs})
				}
			}
		}
	}
	return nil
}

// importName is the name a file refers to an import by. A module package's
// name is read from its source; any other's is guessed from its path, which
// only the markers' packages need, and those follow the convention.
func (a *analyzer) importName(imp *ast.ImportSpec, ipath string) (string, error) {
	if imp.Name != nil {
		return imp.Name.Name, nil
	}
	if _, ok := a.inModule(ipath); ok {
		p, err := a.load(pkgKey{ipath, false})
		if err != nil {
			return "", err
		}
		if p.name != "" {
			return p.name, nil
		}
	}
	elems := strings.Split(ipath, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = elems[len(elems)-2]
	}
	last = strings.TrimPrefix(last, "go-")
	if i := strings.IndexAny(last, ".-"); i >= 0 {
		last = last[:i]
	}
	return last, nil
}

func paramNames(ft *ast.FuncType) []string {
	var names []string
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "_")
		}
		for _, n := range field.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

func recvName(recv *ast.FieldList) string {
	t := recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return "?"
		}
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// target is what an expression naming a callee resolves to: module nodes,
// and the marker it names, if any.
type target struct {
	fns       []*fn
	marker    *marker
	spawn     *spawnMarker
	markerKey string
}

// resolveIdent resolves a bare identifier in f's package. A name f declares
// itself is its own, not the package's.
func (a *analyzer) resolveIdent(f *fn, name string) target {
	if localNames(f)[name] {
		return target{}
	}
	key := a.markerKey(f.pkg.key.path, name)
	if m, ok := funcMarkers[key]; ok {
		return target{marker: &m, markerKey: key}
	}
	return target{fns: f.pkg.funcs[name]}
}

// resolveQualified resolves pkgName.Name in f's file, or reports that pkgName
// is not an import.
func (a *analyzer) resolveQualified(f *fn, pkgName, name string) (target, bool, error) {
	ipath, ok := f.file.imports[pkgName]
	if !ok || localNames(f)[pkgName] {
		return target{}, false, nil
	}
	key := a.markerKey(ipath, name)
	if m, ok := funcMarkers[key]; ok {
		return target{marker: &m, markerKey: key}, true, nil
	}
	if s, ok := spawnMarkers[key]; ok {
		return target{spawn: &s, markerKey: key}, true, nil
	}
	if _, ok := a.inModule(ipath); !ok {
		return target{}, true, nil
	}
	p, err := a.pkgFor(f, ipath)
	if err != nil {
		return target{}, true, err
	}
	return target{fns: p.funcs[name]}, true, nil
}

// pkgFor is the package a file in f's package means by ipath: its own, tests
// included, when an external _test file imports the package under test.
func (a *analyzer) pkgFor(f *fn, ipath string) (*pkg, error) {
	if ipath == f.pkg.key.path {
		return f.pkg, nil
	}
	return a.load(pkgKey{ipath, false})
}

// resolveMethod resolves x.name where x is a value. Uncalled, x.name is far
// more often a field than a method value, so a name some struct of the
// package or its module imports declares as a field reaches no method unless
// it is called.
func (a *analyzer) resolveMethod(f *fn, name string, called bool) (target, error) {
	var t target
	if m, ok := methodMarkers[name]; ok && called {
		t.marker, t.markerKey = &m, "."+name
	}
	pkgs := []*pkg{f.pkg}
	var paths []string
	for _, ipath := range f.file.imports {
		if _, ok := a.inModule(ipath); ok && ipath != f.pkg.key.path {
			paths = append(paths, ipath)
		}
	}
	sort.Strings(paths)
	for _, ipath := range paths {
		p, err := a.load(pkgKey{ipath, false})
		if err != nil {
			return target{}, err
		}
		pkgs = append(pkgs, p)
	}
	if !called {
		for _, p := range pkgs {
			if p.fields[name] {
				return target{}, nil
			}
		}
	}
	for _, p := range pkgs {
		t.fns = append(t.fns, p.methods[name]...)
	}
	return t, nil
}

// calleeOf resolves the function expression of a call.
func (a *analyzer) calleeOf(f *fn, fun ast.Expr) (target, error) {
	switch x := fun.(type) {
	case *ast.Ident:
		return a.resolveIdent(f, x.Name), nil
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			t, isImport, err := a.resolveQualified(f, id.Name, x.Sel.Name)
			if isImport || err != nil {
				return t, err
			}
		}
		return a.resolveMethod(f, x.Sel.Name, true)
	}
	return target{}, nil
}

// scan computes f's edges and spawn parameters, once.
func (a *analyzer) scan(f *fn) error {
	if f.scanned || f.scanning {
		return nil
	}
	f.scanning = true
	defer func() { f.scanning, f.scanned = false, true }()
	f.spawnParams = map[int]bool{}
	var err error
	addTarget := func(t target) {
		if t.marker != nil {
			f.edges = append(f.edges, edge{touch: t.marker.touch, label: t.markerKey})
		}
		for _, to := range t.fns {
			f.edges = append(f.edges, edge{to: to})
		}
	}
	testingVars := a.testingVars(f)
	called := map[*ast.SelectorExpr]bool{}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if err != nil {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			var t target
			if t, err = a.calleeOf(f, x.Fun); err != nil {
				return false
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				called[sel] = true
			}
			err = a.spawnSites(f, t, x)
			return err == nil
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				var t target
				var isImport bool
				if t, isImport, err = a.resolveQualified(f, id.Name, x.Sel.Name); err != nil {
					return false
				}
				if isImport {
					addTarget(t)
					return false
				}
				if testingVars[id.Name] {
					// t.Run, t.Fatalf: the testing package's methods, which
					// only methodMarkers can name.
					if m, ok := methodMarkers[x.Sel.Name]; ok && called[x] {
						addTarget(target{marker: &m, markerKey: "." + x.Sel.Name})
					}
					return false
				}
			}
			var t target
			if t, err = a.resolveMethod(f, x.Sel.Name, called[x]); err != nil {
				return false
			}
			addTarget(t)
			ast.Inspect(x.X, visit)
			return false
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				var t target
				if t, err = a.assigned(f, lhs, x.Tok); err != nil {
					return false
				}
				if len(t.fns) == 0 {
					ast.Inspect(lhs, visit)
				}
				f.assigns = append(f.assigns, t.fns...)
			}
			for _, rhs := range x.Rhs {
				ast.Inspect(rhs, visit)
			}
			return false
		case *ast.KeyValueExpr:
			if _, ok := x.Key.(*ast.Ident); !ok {
				ast.Inspect(x.Key, visit)
			}
			ast.Inspect(x.Value, visit)
			return false
		case *ast.Ident:
			addTarget(a.resolveIdent(f, x.Name))
		}
		return true
	}
	ast.Inspect(f.body, visit)
	return err
}

// assigned resolves the left side of an assignment to the package-level
// variables it sets, if it sets any.
func (a *analyzer) assigned(f *fn, lhs ast.Expr, tok token.Token) (target, error) {
	if tok == token.DEFINE {
		return target{}, nil
	}
	var t target
	switch x := lhs.(type) {
	case *ast.Ident:
		t = target{fns: a.resolveIdent(f, x.Name).fns}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return target{}, nil
		}
		var err error
		if t, _, err = a.resolveQualified(f, id.Name, x.Sel.Name); err != nil {
			return target{}, err
		}
	}
	var vars []*fn
	for _, v := range t.fns {
		if v.isVar() {
			vars = append(vars, v)
		}
	}
	return target{fns: vars}, nil
}

// localNames is every name f declares in its signature or body: parameters,
// results, receivers, `:=`, var and const declarations, range variables, and
// those of its function literals. It ignores block scope, so a name declared
// anywhere in f hides the package's name everywhere in f.
func localNames(f *fn) map[string]bool {
	if f.locals != nil {
		return f.locals
	}
	f.locals = map[string]bool{}
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				f.locals[n.Name] = true
			}
		}
	}
	idents := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			if id, ok := e.(*ast.Ident); ok {
				f.locals[id.Name] = true
			}
		}
	}
	fields(f.recv)
	if f.ftype != nil {
		fields(f.ftype.Params)
		fields(f.ftype.Results)
	}
	if _, isVar := f.body.(*ast.ValueSpec); isVar {
		// A variable's initializer is its body; the names it declares are
		// the package's own.
		return f.locals
	}
	ast.Inspect(f.body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			fields(x.Type.Params)
			fields(x.Type.Results)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				idents(x.Lhs...)
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				idents(x.Key, x.Value)
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				f.locals[id.Name] = true
			}
		case *ast.TypeSwitchStmt:
			if as, ok := x.Assign.(*ast.AssignStmt); ok {
				idents(as.Lhs...)
			}
		}
		return true
	})
	return f.locals
}

// testingVars are the names f and its function literals declare as
// parameters of the testing package's types: *testing.T, testing.TB, and the
// like. A method called on one is the testing package's, never the module's.
func (a *analyzer) testingVars(f *fn) map[string]bool {
	vars := map[string]bool{}
	collect := func(ft *ast.FuncType) {
		for _, field := range ft.Params.List {
			t := field.Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			sel, ok := t.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); !ok || f.file.imports[id.Name] != "testing" {
				continue
			}
			for _, n := range field.Names {
				vars[n.Name] = true
			}
		}
	}
	if f.ftype != nil {
		collect(f.ftype)
	}
	ast.Inspect(f.body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			collect(lit.Type)
		}
		return true
	})
	return vars
}

// spawnSites records the child processes the call x starts: through a spawn
// marker, or through a module function that hands an argument on to one.
func (a *analyzer) spawnSites(f *fn, t target, x *ast.CallExpr) error {
	var nameArgs []int
	label := t.markerKey
	if t.spawn != nil {
		nameArgs = append(nameArgs, t.spawn.nameArg)
	}
	for _, callee := range t.fns {
		if err := a.scan(callee); err != nil {
			return err
		}
		for i := range callee.spawnParams {
			nameArgs = append(nameArgs, i)
			label = callee.name
		}
	}
	for _, i := range nameArgs {
		if i >= len(x.Args) {
			continue
		}
		arg := x.Args[i]
		if j, ok := paramIndex(f, arg); ok {
			f.spawnParams[j] = true
			continue
		}
		tc, program := a.spawnTouch(f, arg)
		f.edges = append(f.edges, edge{touch: tc, label: label + "(" + program + ")"})
	}
	return nil
}

// spawnTouch is what starting the program arg names touches: a program in
// spawnNames, the test binary itself (os.Args[0]: a CLI run), or another
// process.
func (a *analyzer) spawnTouch(f *fn, arg ast.Expr) (touch, string) {
	program, ok := stringLit(arg)
	if id, isIdent := arg.(*ast.Ident); isIdent {
		program, ok = f.pkg.consts[id.Name]
	}
	if ok {
		if tc, listed := spawnNames[program]; listed {
			return tc, strconv.Quote(program)
		}
		return process, strconv.Quote(program)
	}
	if ix, isIndex := arg.(*ast.IndexExpr); isIndex {
		if sel, isSel := ix.X.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Args" {
			if id, isIdent := sel.X.(*ast.Ident); isIdent && f.file.imports[id.Name] == "os" {
				if lit, isLit := ix.Index.(*ast.BasicLit); isLit && lit.Value == "0" {
					return cli, "os.Args[0]"
				}
			}
		}
	}
	return process, "a program named at run time"
}

// paramIndex reports whether arg is one of f's own parameters, by position.
func paramIndex(f *fn, arg ast.Expr) (int, bool) {
	id, ok := arg.(*ast.Ident)
	if !ok {
		return 0, false
	}
	for i, p := range f.params {
		if p == id.Name && p != "_" {
			return i, true
		}
	}
	return 0, false
}

// reach is what one top-level test touches, with, for each touch, the chain
// of calls from the test to the first marker that gives it: the evidence a
// reader checks the classification against.
type reach struct {
	touches touchSet
	witness [touchCount][]string
}

// reachOf walks the graph breadth-first from the test, so each touch's
// witness is a shortest chain. A package-level variable that the test's own
// code (any function of a _test.go file it reaches) assigns is a seam the test
// replaced, so the walk does not enter its initializer: what it reaches
// instead is whatever the test assigned, which the test's code already names.
func (a *analyzer) reachOf(test *fn) (reach, error) {
	first, reached, err := a.walk(test, nil)
	if err != nil {
		return reach{}, err
	}
	replaced := map[*fn]bool{}
	for _, f := range reached {
		if f.file.test {
			for _, v := range f.assigns {
				replaced[v] = true
			}
		}
	}
	if len(replaced) == 0 {
		return first, nil
	}
	r, _, err := a.walk(test, replaced)
	return r, err
}

// walk is one breadth-first walk from test that does not enter skip; it
// returns the reach and every node it visited.
func (a *analyzer) walk(test *fn, skip map[*fn]bool) (reach, []*fn, error) {
	var r reach
	var reached []*fn
	parent := map[*fn]*fn{test: nil}
	queue := []*fn{test}
	chain := func(f *fn) []string {
		var c []string
		for ; f != nil; f = parent[f] {
			c = append([]string{f.name}, c...)
		}
		return c
	}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		reached = append(reached, f)
		if err := a.scan(f); err != nil {
			return reach{}, nil, err
		}
		for _, e := range f.edges {
			if e.to == nil {
				if !r.touches.has(e.touch) {
					r.touches = r.touches.with(e.touch)
					r.witness[e.touch] = append(chain(f), e.label)
				}
				continue
			}
			if _, seen := parent[e.to]; !seen && !skip[e.to] {
				parent[e.to] = f
				queue = append(queue, e.to)
			}
		}
	}
	return r, reached, nil
}

// testFunc finds the top-level test named name in the package at importPath,
// tests included.
func (a *analyzer) testFunc(importPath, name string) (*fn, error) {
	if _, ok := a.inModule(importPath); !ok {
		return nil, fmt.Errorf("package %s is not in module %s", importPath, a.modPath)
	}
	p, err := a.load(pkgKey{importPath, true})
	if err != nil {
		return nil, err
	}
	for _, f := range p.funcs[name] {
		if _, isFunc := f.body.(*ast.BlockStmt); isFunc {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no func %s in the source of %s", name, importPath)
}
