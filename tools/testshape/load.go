package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// loadMode type-checks every package from source, function bodies included.
// go/packages checks only the bodies of the packages a pattern names, and a
// test binary recompiles some module packages for its own tests (a fixture
// package that imports the package under test); those are named by no
// pattern, and their bodies are the code a test runs.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax |
	packages.NeedTypesInfo | packages.NeedModule | packages.NeedForTest

// loaded is the module as the compiler sees it: its typed SSA program, and,
// per import path, the packages its test binary compiles tests from.
type loaded struct {
	module string // the module's path
	prog   *ssa.Program
	// tests holds, per import path, the package itself as its test binary
	// compiles it (with its _test.go files) and its external _test package,
	// whichever exist.
	tests map[string][]*ssa.Package
}

// load is the one read of the module: go list, the type checker and the SSA
// builder, run once in dir. buildFlags are the measured run's (its -tags), so
// the files classified are the files that run compiled. A package that fails
// to load fails the whole load, naming it: a partial program would classify
// some tests against code it never saw. [LAW:no-silent-failure]
// [LAW:effects-at-boundaries]
func load(dir string, buildFlags []string) (*loaded, error) {
	cfg := &packages.Config{Mode: loadMode, Dir: dir, Tests: true, BuildFlags: buildFlags}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load the module in %s: %w", dir, err)
	}
	if err := loadErrors(pkgs); err != nil {
		return nil, err
	}
	module, err := mainModule(dir, pkgs)
	if err != nil {
		return nil, err
	}

	// Only the module's functions are walked, so only the module's packages
	// get SSA bodies; every other package is its members alone.
	l := &loaded{module: module, prog: ssa.NewProgram(pkgs[0].Fset, ssa.InstantiateGenerics), tests: make(map[string][]*ssa.Package)}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		var files []*ast.File
		var info *types.Info
		if inModule(module, p.PkgPath) {
			files, info = p.Syntax, p.TypesInfo
		}
		sp := l.prog.CreatePackage(p.Types, files, info, true)
		// The test binary's own packages: the package under test recompiled
		// with its _test.go files, and its external _test package.
		if inModule(module, p.PkgPath) && p.ForTest != "" && (p.PkgPath == p.ForTest || p.PkgPath == p.ForTest+"_test") {
			l.tests[p.ForTest] = append(l.tests[p.ForTest], sp)
		}
	})
	l.prog.Build()
	return l, nil
}

// mainModule is the path of the main module the packages loaded in dir
// belong to.
func mainModule(dir string, pkgs []*packages.Package) (string, error) {
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Main {
			return p.Module.Path, nil
		}
	}
	return "", fmt.Errorf("no package in %s belongs to a main module; run testshape from the module's root", dir)
}

// loadErrors is every package's load error, each naming its package.
func loadErrors(pkgs []*packages.Package) error {
	var errs []error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, fmt.Errorf("package %s failed to load: %v", p.ID, e))
		}
	})
	return errors.Join(errs...)
}

// inModule reports whether the package at path belongs to module.
func inModule(module, path string) bool {
	return path == module || strings.HasPrefix(path, module+"/")
}

// short is s with the module path left off every name in it; the module's
// root package keeps its path.
func short(module, s string) string {
	return strings.ReplaceAll(s, module+"/", "")
}

// String is d's full name as go/types prints it: "pkg.F", "(pkg.T).M",
// "(*pkg.T).M", or "pkg.T.F" for a field.
func (d decl) String() string {
	for _, open := range []string{"(*", "("} {
		if rest, ok := strings.CutPrefix(d.name, open); ok {
			return open + d.pkg + "." + rest
		}
	}
	return d.pkg + "." + d.name
}

// declared is every name p declares, each as decl.String writes it: its
// functions, variables and types, its types' methods and interface methods,
// and its structs' fields.
func declared(p *types.Package) map[string]bool {
	names := make(map[string]bool)
	scope := p.Scope()
	for _, n := range scope.Names() {
		names[decl{p.Path(), n}.String()] = true
		tn, ok := scope.Lookup(n).(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		for m := range named.Methods() {
			names[m.FullName()] = true
		}
		switch u := named.Underlying().(type) {
		case *types.Interface:
			for m := range u.Methods() {
				names[m.FullName()] = true
			}
		case *types.Struct:
			for f := range u.Fields() {
				names[decl{p.Path(), n + "." + f.Name()}.String()] = true
			}
		}
	}
	return names
}

// resolveMarkers checks the marker data against the packages it names, each
// loaded in dir by its own path, whatever the module imports, and returns it
// as full names under the module. A name its package does not declare would
// count no touch, and its tests would read as unit, so each one fails the
// run, naming it. [LAW:no-silent-failure] [LAW:parse-dont-validate]
func resolveMarkers(dir string, module []markerGroup) (markerSet, error) {
	var patterns []string
	add := func(pattern string) {
		if !slices.Contains(patterns, pattern) {
			patterns = append(patterns, pattern)
		}
	}
	for _, g := range module {
		add("./" + g.pkg)
	}
	for _, g := range externalMarkers {
		add(g.pkg)
	}
	for _, s := range spawners {
		add(s.fn.pkg)
	}
	for _, d := range resolvedThrough {
		add(d.pkg)
	}
	pkgs, err := packages.Load(&packages.Config{Mode: packages.NeedName | packages.NeedTypes | packages.NeedModule, Dir: dir}, patterns...)
	if err != nil {
		return markerSet{}, fmt.Errorf("load the marker packages in %s: %w", dir, err)
	}
	if err := loadErrors(pkgs); err != nil {
		return markerSet{}, err
	}
	mod, err := mainModule(dir, pkgs)
	if err != nil {
		return markerSet{}, err
	}
	names := make(map[string]map[string]bool)
	for _, p := range pkgs {
		names[p.PkgPath] = declared(p.Types)
	}
	ms := markerSet{module: mod, touches: make(map[string]touch), spawners: make(map[string]int)}
	var errs []error
	check := func(d decl) string {
		if !names[d.pkg][d.String()] {
			errs = append(errs, fmt.Errorf("marker %s names nothing package %s declares (tools/testshape/markers.go)", d, d.pkg))
		}
		return d.String()
	}
	for _, g := range module {
		for _, n := range g.names {
			ms.touches[check(decl{mod + "/" + g.pkg, n})] = g.touch
		}
	}
	for _, g := range externalMarkers {
		for _, n := range g.names {
			ms.touches[check(decl{g.pkg, n})] = g.touch
		}
	}
	for _, s := range spawners {
		ms.spawners[check(s.fn)] = s.program
	}
	for _, d := range resolvedThrough {
		check(d)
	}
	return ms, errors.Join(errs...)
}
