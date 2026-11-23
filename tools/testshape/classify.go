package main

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// layer is where a test sits.
type layer int

const (
	layerUnit layer = iota
	layerIntegration
	layerE2E
	numLayers
)

var layerNames = [numLayers]string{"unit", "integration", "e2e"}

func (l layer) String() string { return layerNames[l] }

// witness is the chain that reaches one touch: the vertices from the test to
// the one that makes the touch, then the calls from there to it.
type witness struct {
	path []fmt.Stringer
	tail []string
}

// render is the chain, with the module path left off every name.
func (w witness) render(module string) string {
	names := make([]string, 0, len(w.path)+len(w.tail))
	for _, v := range w.path {
		names = append(names, v.String())
	}
	return short(module, strings.Join(append(names, w.tail...), " → "))
}

// touches is which touches a test has: each touched one holds its witness,
// an untouched one nil.
type touches [numTouches]*witness

// layerOf is the layer rule, the one home of it: a test that runs the CLI is
// end-to-end; one with any other touch is integration; one with none, which
// needs nothing outside its process, is unit. [LAW:single-enforcer]
func layerOf(ts touches) layer {
	if ts[touchCLI] != nil {
		return layerE2E
	}
	for _, w := range ts {
		if w != nil {
			return layerIntegration
		}
	}
	return layerUnit
}

// testShape is one top-level test: the stream's facts and its touches. A
// skipped test has none, and sits at no layer.
type testShape struct {
	gotestjson.Test
	touches touches
}

// packageShape is one package's tests, its initializer's touches, and its
// TestMain's when it has one.
type packageShape struct {
	importPath string
	tests      []testShape
	init       touches
	testMain   *touches
}

// classify gives every top-level test the stream ran its touches, read off
// the graph. A test the stream ran that the loaded program does not declare,
// or declares twice, fails the classification: the load is not the code that
// ran. [LAW:no-silent-failure]
func classify(g *graph, l *loaded, pkgs []gotestjson.Package) ([]packageShape, error) {
	var shapes []packageShape
	var errs []error
	for _, p := range pkgs {
		if len(p.Tests) == 0 {
			continue
		}
		variants := l.tests[p.ImportPath]
		if len(variants) == 0 {
			errs = append(errs, fmt.Errorf("%s ran tests, and the loaded module has no test package for it", p.ImportPath))
			continue
		}
		ps := packageShape{importPath: p.ImportPath}
		// A spawn of the test binary is a CLI run when its TestMain can run
		// the CLI, and another process otherwise.
		self := touchProc
		tm, err := lookup(p.ImportPath, variants, "TestMain")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if tm != nil {
			r := g.walk([]ssa.Member{tm})
			for _, h := range g.hits(r, touchProc) {
				if h.touch == touchCLI {
					self = touchCLI
				}
			}
			ts := touchesOf(r, g.hits(r, self))
			ps.testMain = &ts
		}
		var inits []ssa.Member
		for _, v := range variants {
			inits = append(inits, v.Func("init"))
		}
		r := g.walk(inits)
		ps.init = touchesOf(r, g.hits(r, self))
		for _, t := range p.Tests {
			fn, err := lookup(p.ImportPath, variants, t.Name)
			switch {
			case err != nil:
				errs = append(errs, err)
			case fn == nil:
				errs = append(errs, fmt.Errorf("%s ran %s, and the loaded module declares no such function", p.ImportPath, t.Name))
			case t.Verdict == gotestjson.Skip:
				ps.tests = append(ps.tests, testShape{Test: t})
			default:
				r := g.walk([]ssa.Member{fn})
				ps.tests = append(ps.tests, testShape{Test: t, touches: touchesOf(r, g.hits(r, self))})
			}
		}
		shapes = append(shapes, ps)
	}
	return shapes, errors.Join(errs...)
}

// lookup is the package-level function named name in the test binary's
// packages; nil when neither declares it, and an error when both do.
func lookup(pkg string, variants []*ssa.Package, name string) (*ssa.Function, error) {
	var found *ssa.Function
	for _, v := range variants {
		if fn := v.Func(name); fn != nil {
			if found != nil {
				return nil, fmt.Errorf("%s declares %s in both its package and its external test package", pkg, name)
			}
			found = fn
		}
	}
	return found, nil
}

// touchesOf keeps each touch's first hit, and builds the witness of that one
// alone.
func touchesOf(r *reach, hs []hit) touches {
	var ts touches
	for _, h := range hs {
		if ts[h.touch] == nil {
			ts[h.touch] = witnessOf(r, h)
		}
	}
	return ts
}
