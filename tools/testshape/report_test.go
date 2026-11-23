package main

import (
	"go/types"
	"strings"
	"testing"
	"time"
)

func TestTheTableHasARowPerPackageAndATotal(t *testing.T) {
	disk := reached{FS: witness{}}
	cli := reached{CLI: witness{}, FS: witness{}}
	test := func(name string, r reached) testShape { return testShape{Name: name, found: found{Reached: r}} }
	pkgs := []measuredPackage{
		{ImportPath: "example.com/m/a", Tests: []measuredTest{
			{testShape: test("TestPure", reached{}), Ran: true, Elapsed: 100 * time.Millisecond},
			{testShape: test("TestPure2", reached{}), Ran: true, Elapsed: 200 * time.Millisecond},
			{testShape: test("TestDisk", disk), Ran: true, Elapsed: 1500 * time.Millisecond},
		}},
		{ImportPath: "example.com/m/cmd", Setup: found{Reached: reached{Clock: witness{}}}, Tests: []measuredTest{
			{testShape: test("TestRun", cli), Ran: true, Elapsed: 4 * time.Second},
			{testShape: test("TestSkipped", disk)},
		}},
		{ImportPath: "example.com/m/empty"},
	}
	var rows []row
	for _, m := range pkgs {
		rows = append(rows, rowOf(m, "example.com/m"))
	}
	var out strings.Builder
	writeTable(&out, rows)
	want := "" +
		"  package  tests  ran  unit  integ  e2e  unit%  Σunit  Σinteg  Σe2e  store  git  cli  proc  net  fs  clock  setup\n" +
		"        a      3    3     2      1    0    67%   0.3s    1.5s  0.0s      0    0    0     0    0   1      0      -\n" +
		"      cmd      2    1     0      1    1     0%   0.0s    0.0s  4.0s      0    0    1     0    0   2      0  clock\n" +
		"    empty      0    0     0      0    0      -   0.0s    0.0s  0.0s      0    0    0     0    0   0      0      -\n" +
		"    TOTAL      5    4     2      2    1    40%   0.3s    1.5s  4.0s      0    0    1     0    0   3      0  clock\n" +
		"\n" +
		"tests: the top-level tests the package declares; ran: those the stream ran. unit/integ/e2e: declared tests\n" +
		"at each layer. Σ: the elapsed of the tests that ran, summed, waits included,\n" +
		"so a package's sums can exceed its wall time.\n" +
		"touch columns: tests that can reach the touch. setup: touches of the package's initialization and TestMain.\n"
	if out.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestTheListingGivesEachTestItsLayerAWitnessPerTouchAndWhatWasWithheld(t *testing.T) {
	a := types.NewPackage("example.com/m/a", "a")
	paths := types.NewNamed(types.NewTypeName(0, a, "paths", nil), types.NewSlice(types.Typ[types.String]), nil)
	key := types.NewNamed(types.NewTypeName(0, a, "key", nil), types.Typ[types.Int], nil)
	pkgs := []modulePackage{{module: module{Path: "example.com/m"}, measuredPackage: measuredPackage{
		ImportPath: "example.com/m/a",
		Setup: found{
			Reached:  reached{Clock: witness{chain: []string{"init of a", "time.Now"}, what: "time.Now (clock)"}},
			Withheld: []withholding{{instance{funcName{"slices", "Clone"}, []types.Type{paths, types.Typ[types.String]}}, 0, "S"}},
		},
		Tests: []measuredTest{
			{testShape: testShape{Name: "TestPure", found: found{
				Reached: reached{},
				Withheld: []withholding{
					{instance{funcName{"slices", "Sort"}, []types.Type{types.NewSlice(key), key}}, 1, "E"},
					{instance{funcName{"slices", "Max"}, []types.Type{types.NewSlice(key), key}}, 1, "E"},
				},
			}}, Ran: true},
			{testShape: testShape{Name: "TestDisk", found: found{Reached: reached{
				FS:  witness{chain: []string{"a.TestDisk", "a.load", "os.ReadFile"}, what: "os.ReadFile (fs)"},
				Git: witness{chain: []string{"a.TestDisk", "a.git"}, what: `os/exec.Command spawns "git", which is git`},
			}}}},
		},
	}}}
	var out strings.Builder
	writeListing(&out, pkgs)
	want := "\nexample.com/m/a\n" +
		"    setup clock: init of a → time.Now ⇒ time.Now (clock)\n" +
		"    setup withheld: slices.Clone[a.paths, string] neither converts its type parameter S (a.paths) nor calls a method of it\n" +
		"  unit        TestPure\n" +
		"    withheld: slices.Max[[]a.key, a.key] neither converts its type parameter E (a.key) nor calls a method of it\n" +
		"    withheld: slices.Sort[[]a.key, a.key] neither converts its type parameter E (a.key) nor calls a method of it\n" +
		"  integration TestDisk  [not run]\n" +
		`    git: a.TestDisk → a.git ⇒ os/exec.Command spawns "git", which is git` + "\n" +
		"    fs: a.TestDisk → a.load → os.ReadFile ⇒ os.ReadFile (fs)\n"
	if out.String() != want {
		t.Errorf("listing:\n%s\nwant:\n%s", out.String(), want)
	}
}
