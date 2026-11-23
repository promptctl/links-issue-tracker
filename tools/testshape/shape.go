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
// initialization of its packages, and its TestMain) and each declared test.
type binaryShape struct {
	Setup reached
	Tests map[string]reached
	Order []string // declared tests, in name order
}

// measure decides the reach of every test b declares.
func (a *analyzer) measure(b *binary) binaryShape {
	p := newPool()
	setup := newWalker(a, p, true)
	for _, sp := range b.Packages {
		if init := sp.Func("init"); init != nil {
			setup.root(init, "init of "+strings.TrimPrefix(sp.Pkg.Path(), a.mod.Path+"/"))
		}
	}
	if b.TestMain != nil {
		setup.root(b.TestMain, "TestMain")
	}
	setup.run()
	p.final = true

	testMainRunsCLI := false
	if b.TestMain != nil {
		tm := newWalker(a, p, false)
		tm.root(b.TestMain, "TestMain")
		tm.run()
		testMainRunsCLI = a.reachesCLI(tm)
	}
	setup.settleSpawns(testMainRunsCLI)

	shape := binaryShape{Setup: setup.touches, Tests: map[string]reached{}}
	for _, test := range b.Tests {
		w := newWalker(a, p, false)
		w.root(test, test.Name())
		w.run()
		w.settleSpawns(testMainRunsCLI)
		shape.Tests[test.Name()] = w.touches
		shape.Order = append(shape.Order, test.Name())
	}
	return shape
}

func (a *analyzer) reachesCLI(w *walker) bool {
	for fn := range w.visited {
		if o := a.owner(fn); o.inModule && a.class.isCLI(o.pkg, o.key) {
			return true
		}
	}
	return false
}

// measuredTest is one declared top-level test, with what the stream says of
// it.
type measuredTest struct {
	Name    string
	Reached reached
	Ran     bool          // false: skipped, or not selected by the run
	Elapsed time.Duration // zero unless Ran
}

func (t measuredTest) Layer() Layer { return layerOf(t.Reached.set()) }

// measuredPackage is one package of the stream, joined to its binary.
type measuredPackage struct {
	ImportPath string
	Setup      reached
	Tests      []measuredTest
}

// join puts the stream's durations on the binary's declared tests. A test
// the stream ran that the binary does not declare, and a test the stream ran
// twice, are refusals: the stream and the measure disagree about what ran.
func join(pkg gotestjson.Package, shape binaryShape) (measuredPackage, []string) {
	var refusals []string
	ran := map[string]gotestjson.Test{}
	for _, t := range pkg.Tests {
		if _, twice := ran[t.Name]; twice {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran %s twice", pkg.ImportPath, t.Name))
		}
		ran[t.Name] = t
		if _, declared := shape.Tests[t.Name]; !declared {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran %s, which its test binary does not declare", pkg.ImportPath, t.Name))
		}
	}
	m := measuredPackage{ImportPath: pkg.ImportPath, Setup: shape.Setup}
	for _, name := range shape.Order {
		t, ok := ran[name]
		didRun := ok && (t.Verdict == gotestjson.Pass || t.Verdict == gotestjson.Fail)
		mt := measuredTest{Name: name, Reached: shape.Tests[name], Ran: didRun}
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
	if len(run.Packages) == 0 && len(run.Cut) == 0 {
		refusals = append(refusals, "the stream is empty")
	}
	seen := map[string]bool{}
	for _, p := range run.Packages {
		if seen[p.ImportPath] {
			refusals = append(refusals, fmt.Sprintf("%s: the stream ran the package twice", p.ImportPath))
		}
		seen[p.ImportPath] = true
		if p.FailedBuild != "" {
			refusals = append(refusals, fmt.Sprintf("%s: its test binary failed to build (%s)", p.ImportPath, p.FailedBuild))
			continue
		}
		var failed, unfinished []string
		for _, t := range p.Tests {
			switch t.Verdict {
			case gotestjson.Fail:
				failed = append(failed, t.Name)
			case gotestjson.Unfinished:
				unfinished = append(unfinished, t.Name)
			}
		}
		if len(unfinished) > 0 {
			refusals = append(refusals, fmt.Sprintf("%s: the package ended with %s still running", p.ImportPath, strings.Join(unfinished, ", ")))
		}
		if p.Verdict == gotestjson.Fail && len(failed) == 0 && len(unfinished) == 0 {
			refusals = append(refusals, fmt.Sprintf("%s: its test binary died outside any test", p.ImportPath))
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

// moduleRefusals names every module of the suite the stream has no package
// of, and every stream package that belongs to no module of the suite.
func moduleRefusals(mods []module, run gotestjson.Run) []string {
	var refusals []string
	covered := make([]bool, len(mods))
	for _, p := range run.Packages {
		i := moduleIndex(mods, p.ImportPath)
		if i < 0 {
			refusals = append(refusals, fmt.Sprintf("%s: the package belongs to no module of the suite", p.ImportPath))
			continue
		}
		covered[i] = true
	}
	for i, m := range mods {
		if !covered[i] {
			refusals = append(refusals, fmt.Sprintf("module %s (%s): its tests are absent from the stream", m.Path, m.Dir))
		}
	}
	return refusals
}

// moduleIndex is the module that owns path: the one with the longest
// matching module path, so a nested module wins over its parent.
func moduleIndex(mods []module, path string) int {
	best := -1
	for i, m := range mods {
		if m.contains(path) && (best < 0 || len(m.Path) > len(mods[best].Path)) {
			best = i
		}
	}
	return best
}

// analysisRefusals names every outside function module code reaches without
// a classification, with its first caller, and every module function with
// no body to walk.
func analysisRefusals(a *analyzer) []string {
	var refusals []string
	for _, name := range sortedKeys(a.unclassified) {
		refusals = append(refusals, fmt.Sprintf("unclassified outside function %s, first called by %s", name, a.unclassified[name]))
	}
	for _, name := range sortedKeys(a.bodiless) {
		refusals = append(refusals, fmt.Sprintf("module function %s has no Go body to walk, reached from %s", name, a.bodiless[name]))
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
