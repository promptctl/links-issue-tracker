package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/ssa"
)

// module is one module of the suite.
type module struct {
	Dir  string
	Path string
}

// suiteModules finds every module of the suite under root: every go.mod in a
// directory the go command's ./... would walk — none under testdata, nor
// under a directory whose name begins with "." or "_".
func suiteModules(root string) ([]module, error) {
	var mods []module
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
		}
		if d.IsDir() || d.Name() != "go.mod" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mp := modfile.ModulePath(data)
		if mp == "" {
			return fmt.Errorf("%s declares no module path", path)
		}
		mods = append(mods, module{Dir: filepath.Dir(path), Path: mp})
		return nil
	})
	return mods, err
}

// contains reports whether the package at path — a package, its external
// test package, or its test binary's main — belongs to m.
func (m module) contains(path string) bool {
	path = strings.TrimSuffix(strings.TrimSuffix(path, ".test"), "_test")
	return path == m.Path || strings.HasPrefix(path, m.Path+"/")
}

// binary is one test binary: the package it tests and what the build put in
// it.
type binary struct {
	ImportPath string
	// Packages: every module package the binary contains, test variants
	// included, in import-path order. Their initialization runs before every
	// test.
	Packages []*ssa.Package
	// TestMain is the package's TestMain, nil when it has none.
	TestMain *ssa.Function
	// Tests are the top-level tests the binary declares, in name order.
	Tests []declaredTest
}

// declaredTest is one name go test runs as a top-level test: a test, a fuzz
// target's seed corpus, or an example with an output comment. A name both the
// package and its external test package declare is one entry of the stream,
// so it is one test whose reach is both functions'.
type declaredTest struct {
	Name  string
	Funcs []*ssa.Function
}

// loaded is one module loaded and built to SSA.
type loaded struct {
	Module   module
	Prog     *ssa.Program
	Binaries map[string]*binary // by the import path of the package under test
	// ModuleOf is the module of each outside package the build links, by
	// import path: "std" for the standard library.
	ModuleOf map[string]string
}

// listedPackage is the subset of `go list -json` this loader reads.
type listedPackage struct {
	ImportPath      string // with " [p.test]" for a package recompiled for p's test binary
	Name            string
	Dir             string
	Export          string
	CompiledGoFiles []string
	Imports         []string // IDs, after ImportMap
	ImportMap       map[string]string
	Module          *struct{ Path, GoVersion string }
	Error           *struct{ Err string }
}

// path is the package's import path, without the test binary it was
// recompiled for.
func (p *listedPackage) path() string {
	path, _, _ := strings.Cut(p.ImportPath, " ")
	return path
}

func (p *listedPackage) isTestMain() bool {
	return p.Name == "main" && strings.HasSuffix(p.ImportPath, ".test")
}

// loadModule loads every package of m and its tests — the module's packages,
// each variant recompiled for a test binary included, from source, and
// everything outside the module from the export data `go list -export`
// builds, under the environment and flags `go test` uses — and builds the
// module's SSA. Code outside the module has no bodies: it is never walked,
// only classified.
//
// go/packages cannot load this shape: it gives syntax only to the packages
// its patterns match, never to a variant recompiled for a test binary, unless
// it type-checks the whole dependency graph from source.
func loadModule(m module, buildFlags []string) (*loaded, error) {
	args := append([]string{"list", "-e", "-json=ImportPath,Name,Dir,Export,CompiledGoFiles,Imports,ImportMap,Module,Error", "-deps", "-test", "-export", "-compiled"}, buildFlags...)
	cmd := exec.Command("go", append(args, "./...")...)
	cmd.Dir = m.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s: %v\n%s", m.Dir, err, stderr.String())
	}
	sizes, err := buildSizes(m.Dir)
	if err != nil {
		return nil, err
	}
	var listed []*listedPackage
	byID := map[string]*listedPackage{}
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		p := new(listedPackage)
		if err := dec.Decode(p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list in %s: %w", m.Dir, err)
		}
		listed = append(listed, p)
		byID[p.ImportPath] = p
	}

	ld := &loader{
		m:        m,
		fset:     token.NewFileSet(),
		byID:     byID,
		exported: map[string]*types.Package{},
		checked:  map[string]*types.Package{},
		syntax:   map[string][]*ast.File{},
		infos:    map[string]*types.Info{},
		sizes:    sizes,
	}
	// go list -deps prints every package after its dependencies, so each
	// package's imports are loaded before it.
	for _, p := range listed {
		ld.load(p)
	}
	if len(ld.problems) > 0 {
		return nil, fmt.Errorf("module %s does not load:\n  %s", m.Path, strings.Join(ld.problems, "\n  "))
	}

	prog := ssa.NewProgram(ld.fset, ssa.InstantiateGenerics)
	created := map[*types.Package]bool{}
	var create func(*types.Package)
	create = func(pkg *types.Package) {
		if created[pkg] {
			return
		}
		created[pkg] = true
		for _, imp := range pkg.Imports() {
			create(imp)
		}
		prog.CreatePackage(pkg, nil, nil, true)
	}
	for _, p := range listed {
		if pkg := ld.exported[p.ImportPath]; pkg != nil {
			create(pkg)
		}
	}
	ssaByID := map[string]*ssa.Package{}
	for _, p := range listed {
		if pkg := ld.checked[p.ImportPath]; pkg != nil {
			created[pkg] = true
			ssaByID[p.ImportPath] = prog.CreatePackage(pkg, ld.syntax[p.ImportPath], ld.infos[p.ImportPath], true)
		}
	}
	prog.Build()

	l := &loaded{Module: m, Prog: prog, Binaries: map[string]*binary{}, ModuleOf: map[string]string{}}
	for _, p := range listed {
		switch {
		case p.isTestMain():
		case p.Module == nil:
			l.ModuleOf[p.path()] = "std"
		case p.Module.Path != m.Path:
			l.ModuleOf[p.path()] = p.Module.Path
		}
		if p.isTestMain() {
			b := ld.newBinary(prog, p, ssaByID)
			l.Binaries[b.ImportPath] = b
		}
	}
	return l, nil
}

// buildSizes is the type sizes of the architecture the go command in dir
// builds for, which go test's binaries are built for.
func buildSizes(dir string) (types.Sizes, error) {
	cmd := exec.Command("go", "env", "GOARCH")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOARCH in %s: %w", dir, err)
	}
	arch := strings.TrimSpace(string(out))
	sizes := types.SizesFor("gc", arch)
	if sizes == nil {
		return nil, fmt.Errorf("go env GOARCH in %s: gc has no sizes for %q", dir, arch)
	}
	return sizes, nil
}

// loader type-checks one module's packages from source and reads the rest
// from export data, all into one types universe.
type loader struct {
	m        module
	fset     *token.FileSet
	byID     map[string]*listedPackage
	exported map[string]*types.Package // outside packages, by path
	checked  map[string]*types.Package // module packages, by ID
	syntax   map[string][]*ast.File
	infos    map[string]*types.Info
	sizes    types.Sizes
	problems []string
}

func (ld *loader) load(p *listedPackage) {
	switch {
	case p.Error != nil:
		ld.problems = append(ld.problems, fmt.Sprintf("%s: %s", p.ImportPath, p.Error.Err))
	case p.isTestMain() || p.ImportPath == "unsafe":
		// The generated test main is the runner, not the suite's code;
		// unsafe is types.Unsafe.
	case p.Module == nil || p.Module.Path != ld.m.Path:
		if err := ld.readExport(p); err != nil {
			ld.problems = append(ld.problems, err.Error())
		}
	default:
		ld.check(p)
	}
}

// readExport loads an outside package's types from the export data go list
// built, into the shared map, so every package sees one types.Package per
// path.
func (ld *loader) readExport(p *listedPackage) error {
	if p.Export == "" {
		return fmt.Errorf("%s: go list built no export data", p.ImportPath)
	}
	f, err := os.Open(p.Export)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := gcexportdata.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", p.ImportPath, err)
	}
	pkg, err := gcexportdata.Read(r, ld.fset, ld.exported, p.ImportPath)
	if err != nil {
		return fmt.Errorf("%s: %w", p.ImportPath, err)
	}
	ld.exported[p.ImportPath] = pkg
	return nil
}

func (ld *loader) check(p *listedPackage) {
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(p.Dir, name)
		}
		// Comments too: an example's output comment decides whether go test
		// runs it.
		f, err := parser.ParseFile(ld.fset, name, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			ld.problems = append(ld.problems, err.Error())
			return
		}
		files = append(files, f)
	}
	info := &types.Info{
		Types:        map[ast.Expr]types.TypeAndValue{},
		Defs:         map[*ast.Ident]types.Object{},
		Uses:         map[*ast.Ident]types.Object{},
		Implicits:    map[ast.Node]types.Object{},
		Instances:    map[*ast.Ident]types.Instance{},
		Scopes:       map[ast.Node]*types.Scope{},
		Selections:   map[*ast.SelectorExpr]*types.Selection{},
		FileVersions: map[*ast.File]string{},
	}
	conf := types.Config{
		Importer:  importerFunc(func(path string) (*types.Package, error) { return ld.imported(p, path) }),
		Sizes:     ld.sizes,
		GoVersion: "go" + p.Module.GoVersion,
		Error:     func(err error) { ld.problems = append(ld.problems, err.Error()) },
	}
	pkg, _ := conf.Check(p.path(), ld.fset, files, info)
	ld.checked[p.ImportPath], ld.syntax[p.ImportPath], ld.infos[p.ImportPath] = pkg, files, info
}

// imported resolves an import in p's source to the package the build links
// for it: a test variant where go list mapped one, else the package itself.
func (ld *loader) imported(p *listedPackage, path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	id := path
	if mapped, ok := p.ImportMap[path]; ok {
		id = mapped
	}
	if pkg := ld.checked[id]; pkg != nil {
		return pkg, nil
	}
	if pkg := ld.exported[id]; pkg != nil {
		return pkg, nil
	}
	return nil, fmt.Errorf("%s imports %s, which did not load", p.ImportPath, id)
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// newBinary is the test binary whose generated main is p: the module
// packages it links, its TestMain, and the top-level tests it declares.
func (ld *loader) newBinary(prog *ssa.Program, p *listedPackage, ssaByID map[string]*ssa.Package) *binary {
	b := &binary{ImportPath: strings.TrimSuffix(p.ImportPath, ".test")}
	seen := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if sp := ssaByID[id]; sp != nil {
			b.Packages = append(b.Packages, sp)
		}
		for _, imp := range ld.byID[id].Imports {
			// Code outside the module never imports the module, so the
			// module packages are those the module packages import.
			if ssaByID[imp] != nil {
				visit(imp)
			}
		}
	}
	visit(p.ImportPath)
	slices.SortFunc(b.Packages, func(x, y *ssa.Package) int { return strings.Compare(x.Pkg.Path(), y.Pkg.Path()) })

	byName := map[string][]*ssa.Function{}
	for id, sp := range ssaByID {
		path := sp.Pkg.Path()
		if (path != b.ImportPath && path != b.ImportPath+"_test") || !seen[id] {
			continue
		}
		var testFiles []*ast.File
		for _, f := range ld.syntax[id] {
			if strings.HasSuffix(prog.Fset.Position(f.Pos()).Filename, "_test.go") {
				testFiles = append(testFiles, f)
			}
		}
		for _, f := range testFiles {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv != nil {
					continue
				}
				fn := sp.Func(fd.Name.Name)
				switch name := fd.Name.Name; {
				case name == "TestMain" && takesOne(fn, "M"):
					b.TestMain = fn
				case isTest(name, "Test") && takesOne(fn, "T"), isTest(name, "Fuzz") && takesOne(fn, "F"):
					byName[name] = append(byName[name], fn)
				}
			}
		}
		// go test runs an example only when it has an output comment.
		for _, ex := range doc.Examples(testFiles...) {
			if ex.Output != "" || ex.EmptyOutput {
				name := "Example" + ex.Name
				byName[name] = append(byName[name], sp.Func(name))
			}
		}
	}
	for _, name := range sortedKeys(byName) {
		funcs := byName[name]
		slices.SortFunc(funcs, func(x, y *ssa.Function) int { return strings.Compare(x.Pkg.Pkg.Path(), y.Pkg.Pkg.Path()) })
		b.Tests = append(b.Tests, declaredTest{Name: name, Funcs: funcs})
	}
	return b
}

// isTest is go test's rule for a test function's name: the prefix, then
// nothing or a rune that is not lower case.
func isTest(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// takesOne reports whether fn's signature is func(*testing.<typ>).
func takesOne(fn *ssa.Function, typ string) bool {
	sig := fn.Signature
	if sig.Recv() != nil || sig.Params().Len() != 1 || sig.Results().Len() != 0 {
		return false
	}
	return sig.Params().At(0).Type().String() == "*testing."+typ
}
