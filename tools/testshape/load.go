package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"sort"
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
	// types are every package of the program, the module's and its
	// dependencies', as go/types declares them.
	types []*types.Package
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
	module := ""
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Main {
			module = p.Module.Path
			break
		}
	}
	if module == "" {
		return nil, fmt.Errorf("no package in %s belongs to a main module; run testshape from the module's root", dir)
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
		l.types = append(l.types, p.Types)
		// The test binary's own packages: the package under test recompiled
		// with its _test.go files, and its external _test package.
		if inModule(module, p.PkgPath) && p.ForTest != "" && (p.PkgPath == p.ForTest || p.PkgPath == p.ForTest+"_test") {
			l.tests[p.ForTest] = append(l.tests[p.ForTest], sp)
		}
	})
	l.prog.Build()
	return l, nil
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

// cutReceiver splits a full name as go/types prints it — "pkg.F",
// "(pkg.T).M" or "(*pkg.T).M" — into its receiver's opening and the rest,
// which starts with the package path.
func cutReceiver(name string) (open, rest string) {
	for _, open := range []string{"(*", "("} {
		if rest, ok := strings.CutPrefix(name, open); ok {
			return open, rest
		}
	}
	return "", name
}

// qualify is a module marker's full name: its package path under module.
func qualify(module, name string) string {
	open, rest := cutReceiver(name)
	return open + module + "/" + rest
}

// packageOf is the import path of the package that declares the function a
// full name names.
func packageOf(name string) string {
	_, rest := cutReceiver(name)
	if typ, _, ok := strings.Cut(rest, ")"); ok {
		rest = typ
	}
	return rest[:strings.LastIndex(rest, ".")]
}

// fullNames is every full name the marker data names: the markers, the
// spawners, and the functions a spawned program is resolved through.
func fullNames(module string, moduleMarkers []marker) []string {
	names := []string{selfExecutable, lookPath}
	for _, m := range moduleMarkers {
		names = append(names, qualify(module, m.name))
	}
	for _, m := range externalMarkers {
		names = append(names, m.name)
	}
	for _, s := range spawners {
		names = append(names, s.name)
	}
	return names
}

// unresolved returns, sorted, each of names that no function, method or
// interface method of pkgs has as its full name. A marker that names nothing
// would count no touch, and its tests would look like unit tests.
// [LAW:no-silent-failure]
func unresolved(names []string, pkgs []*types.Package) []string {
	declared := make(map[string]bool)
	for _, p := range pkgs {
		scope := p.Scope()
		for _, name := range scope.Names() {
			switch obj := scope.Lookup(name).(type) {
			case *types.Func:
				declared[obj.FullName()] = true
			case *types.TypeName:
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				for m := range named.Methods() {
					declared[m.FullName()] = true
				}
				if iface, ok := named.Underlying().(*types.Interface); ok {
					for m := range iface.Methods() {
						declared[m.FullName()] = true
					}
				}
			}
		}
	}
	var missing []string
	for _, name := range names {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}
