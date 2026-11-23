package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// Layer is where a test sits, decided by what it can reach.
type Layer int

const (
	Unit        Layer = iota // reaches nothing outside the process
	Integration              // reaches a store, git, another process, the network, the filesystem or the clock
	EndToEnd                 // reaches a run of the CLI
	numLayers
)

var layerNames = [numLayers]string{"unit", "integration", "e2e"}

func (l Layer) String() string { return layerNames[l] }

// layerOf is the layer of a test that reaches touches.
func layerOf(t Touches) Layer {
	switch {
	case t.Has(CLI):
		return EndToEnd
	case t != 0:
		return Integration
	default:
		return Unit
	}
}

// reached is what one walk found: each touch with its first witness.
type reached map[Touch]witness

func (r reached) set() Touches {
	var s Touches
	for t := range r {
		s = s.With(t)
	}
	return s
}

// binaryShape is what one test binary's code can reach: its setup (the
// initialization of its packages, and its TestMain) and each declared test,
// whose reach includes the setup's.
type binaryShape struct {
	Setup found
	Tests []testShape // in name order
}

// found is what one walk found: each touch with its first witness, and
// each type argument an outside generic instance the walk reached, beyond
// its binary's setup, withheld: what decided that the touches the type
// argument's methods make are not the walk's.
type found struct {
	Reached  reached
	Withheld []withholding
}

// testShape is one test's reach.
type testShape struct {
	Name string
	found
}

// measure decides the reach of b's setup, and of every test b declares over
// it. Whether the setup runs the CLI is decided once, from the setup's whole
// reach: a spawn of the test binary runs the CLI exactly when it does.
func (a *analyzer) measure(b *binary) binaryShape {
	setup := newWalker(a, emptyBinary())
	// Every linked package's initializer is a root: the module's, walked,
	// and each outside one, charged by its classification, which runs
	// whether or not a module package imports its package directly.
	for _, sp := range b.Packages {
		setup.root(sp.Func("init"), "init of "+strings.TrimPrefix(sp.Pkg.Path(), a.mod.Path+"/"))
	}
	if b.TestMain != nil {
		setup.root(b.TestMain, "TestMain")
	}
	setup.run()
	runsCLI := a.reachesCLI(setup.own)
	setup.settle(runsCLI)
	shape := binaryShape{Setup: setup.found()}
	binary := setup.finish()

	for _, t := range b.Tests {
		w := newWalker(a, binary)
		for _, fn := range t.Funcs {
			w.root(fn, t.Name)
		}
		w.run()
		w.settle(runsCLI)
		shape.Tests = append(shape.Tests, testShape{t.Name, w.found()})
	}
	return shape
}

func (a *analyzer) reachesCLI(l *layer) bool {
	for fn := range l.steps.index {
		if a.walked(fn) && a.class.isCLI(a.owner(fn).name) {
			return true
		}
	}
	return false
}

// measuredTest is one declared top-level test, with what the stream says of
// it.
type measuredTest struct {
	testShape
	Ran     bool          // false: skipped, or not selected by the run
	Elapsed time.Duration // zero unless Ran
}

func (t measuredTest) Layer() Layer { return layerOf(t.Reached.set()) }

// measuredPackage is one package of the stream, joined to its binary.
type measuredPackage struct {
	ImportPath string
	Setup      found
	Tests      []measuredTest
}

// join puts the stream's durations on the binary's declared tests. A test
// the stream ran that the binary does not declare, and a test the stream ran
// twice, are refusals: the stream and the measure disagree about what ran.
func join(pkg gotestjson.Package, shape binaryShape) (measuredPackage, []string) {
	var refusals []string
	declared := map[string]bool{}
	for _, t := range shape.Tests {
		declared[t.Name] = true
	}
	ran := map[string]gotestjson.Test{}
	for _, t := range pkg.Tests {
		if _, twice := ran[t.Name]; twice {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran %s twice", pkg.ImportPath, t.Name))
		}
		ran[t.Name] = t
		if !declared[t.Name] {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran %s, which its test binary does not declare", pkg.ImportPath, t.Name))
		}
	}
	m := measuredPackage{ImportPath: pkg.ImportPath, Setup: shape.Setup}
	for _, ts := range shape.Tests {
		t, ok := ran[ts.Name]
		didRun := ok && (t.Ending == gotestjson.Passed || t.Ending == gotestjson.Failed)
		mt := measuredTest{testShape: ts, Ran: didRun}
		if didRun {
			mt.Elapsed = t.Elapsed
		}
		m.Tests = append(m.Tests, mt)
	}
	return m, refusals
}

// streamRefusals is every way the stream itself shows a measure that
// measured nothing. [LAW:no-silent-failure]
func streamRefusals(run gotestjson.Run) []string {
	var refusals []string
	seen := map[string]bool{}
	for _, p := range run.Packages {
		if seen[p.ImportPath] {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran the package twice", p.ImportPath))
		}
		seen[p.ImportPath] = true
		if b, failed := p.Build.(gotestjson.BuildFailed); failed {
			refusals = append(refusals, fmt.Sprintf("%s: its test binary failed to build (%s)", p.ImportPath, b.Package))
			continue
		}
		var failed, unfinished []string
		for _, t := range p.Tests {
			switch t.Ending {
			case gotestjson.Failed:
				failed = append(failed, t.Name)
			case gotestjson.Unfinished:
				unfinished = append(unfinished, t.Name)
			}
		}
		if len(unfinished) > 0 {
			refusals = append(refusals, fmt.Sprintf("%s: the package ended with %s still running", p.ImportPath, strings.Join(unfinished, ", ")))
		}
		if p.Verdict == gotestjson.Fail && len(failed) == 0 && len(unfinished) == 0 {
			refusals = append(refusals, fmt.Sprintf("%s: the package failed outside any test: in its initialization, or in TestMain before or after the tests", p.ImportPath))
		}
	}
	for _, c := range run.Cut {
		what := "no test running"
		if len(c.Open) > 0 {
			what = strings.Join(c.Open, ", ") + " still running"
		}
		refusals = append(refusals, fmt.Sprintf("%s: the stream ended inside the package, with %s", c.ImportPath, what))
	}
	return refusals
}

// strayRefusals names every stream package that belongs to no module of
// the suite, by moduleOf, the module of every package the suite's loads
// list.
func strayRefusals(moduleOf map[string]string, suite map[string]bool, run gotestjson.Run) []string {
	var refusals []string
	for _, p := range run.Packages {
		if !suite[moduleOf[p.ImportPath]] {
			refusals = append(refusals, fmt.Sprintf("%s: the package belongs to no module of the suite", p.ImportPath))
		}
	}
	return refusals
}

// absentRefusals names every package one module's loader built a test binary
// declaring tests for that the stream has no result for, and every package of
// the module the stream ran tests in that has no test binary: either way, the
// stream and the measure disagree about what ran. owned reports whether the
// module owns a stream package.
func absentRefusals(binaries map[string]*binary, inStream map[string]gotestjson.Package, owned func(string) bool) []string {
	var refusals []string
	for _, path := range sortedKeys(binaries) {
		if _, ok := inStream[path]; !ok && len(binaries[path].Tests) > 0 {
			refusals = append(refusals, fmt.Sprintf("%s: its test binary declares tests, and the stream has no result for the package", path))
		}
	}
	for _, path := range sortedKeys(inStream) {
		if owned(path) && binaries[path] == nil && len(inStream[path].Tests) > 0 {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran tests in a package with no test binary", path))
		}
	}
	return refusals
}

// analysisRefusals names every outside function module code reaches without
// a classification, with its first caller; every module function with no
// body to walk; and every called-back interface the program does not hold.
func analysisRefusals(a *analyzer) []string {
	var refusals []string
	for _, name := range sortedKeys(a.unclassified) {
		refusals = append(refusals, fmt.Sprintf("unclassified outside function %s, first called by %s", name, a.unclassified[name]))
	}
	for _, name := range sortedKeys(a.bodiless) {
		refusals = append(refusals, fmt.Sprintf("module function %s has no Go body to walk, reached from %s", name, a.bodiless[name]))
	}
	for _, name := range sortedKeys(a.unresolved) {
		refusals = append(refusals, fmt.Sprintf("called-back interface %s is not in the program, called back by %s", name, a.unresolved[name]))
	}
	for _, what := range sortedKeys(a.inconsistencies) {
		refusals = append(refusals, "testshape is inconsistent, a bug in the tool: "+what)
	}
	return refusals
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
