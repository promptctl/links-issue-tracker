package main

import (
	"errors"
	"fmt"

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

// touches is which touches a test has: each touched one holds its witness
// chain, an untouched one nil.
type touches [numTouches][]string

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

// testShape is one top-level test: the stream's facts and its touches.
type testShape struct {
	gotestjson.Test
	touches touches
}

// packageShape is one package's tests, and its TestMain's touches when it
// has one.
type packageShape struct {
	importPath string
	tests      []testShape
	testMain   *touches
}

// classify gives every top-level test the stream ran its touches, read off
// the graph. A test the stream ran that the loaded program does not declare
// fails the classification: the load is not the code that ran.
// [LAW:no-silent-failure]
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
		var replaced map[*ssa.Global]bool
		self := touchProc
		if tm := lookup(variants, "TestMain"); tm != nil {
			r := g.walk([]*ssa.Function{tm}, nil)
			// TestMain's stores replace their variables for every test.
			replaced = g.replacedBy(r)
			if g.childRunsCLI(tm) {
				self = touchCLI
			}
			ts := touchesOf(g.hits(r), self)
			ps.testMain = &ts
		}
		for _, t := range p.Tests {
			fn := lookup(variants, t.Name)
			if fn == nil {
				errs = append(errs, fmt.Errorf("%s ran %s, and the loaded module declares no such function", p.ImportPath, t.Name))
				continue
			}
			ps.tests = append(ps.tests, testShape{Test: t, touches: touchesOf(g.hits(g.walk([]*ssa.Function{fn}, replaced)), self)})
		}
		shapes = append(shapes, ps)
	}
	return shapes, errors.Join(errs...)
}

// lookup is the package-level function named name in the test binary's
// packages.
func lookup(variants []*ssa.Package, name string) *ssa.Function {
	for _, v := range variants {
		if fn := v.Func(name); fn != nil {
			return fn
		}
	}
	return nil
}

// touchesOf keeps each touch's first witness; a spawn of the test binary
// itself is the touch self.
func touchesOf(hs []hit, self touch) touches {
	var ts touches
	for _, h := range hs {
		t := h.touch
		if t == touchSelf {
			t = self
		}
		if ts[t] == nil {
			ts[t] = h.chain
		}
	}
	return ts
}
