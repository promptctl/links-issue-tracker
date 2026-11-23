package main

import (
	"strings"
	"testing"
	"time"
)

func TestTheTableHasARowPerPackageAndATotal(t *testing.T) {
	disk := reached{FS: witness{}}
	cli := reached{CLI: witness{}, FS: witness{}}
	pkgs := []measuredPackage{
		{ImportPath: "example.com/m/a", Tests: []measuredTest{
			{Name: "TestPure", Reached: reached{}, Ran: true, Elapsed: 100 * time.Millisecond},
			{Name: "TestPure2", Reached: reached{}, Ran: true, Elapsed: 200 * time.Millisecond},
			{Name: "TestDisk", Reached: disk, Ran: true, Elapsed: 1500 * time.Millisecond},
		}},
		{ImportPath: "example.com/m/cmd", Setup: reached{Clock: witness{}}, Tests: []measuredTest{
			{Name: "TestRun", Reached: cli, Ran: true, Elapsed: 4 * time.Second},
			{Name: "TestSkipped", Reached: disk},
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

func TestTheListingGivesEachTestItsLayerAndAWitnessPerTouch(t *testing.T) {
	pkgs := []measuredPackage{{
		ImportPath: "example.com/m/a",
		Setup:      reached{Clock: witness{chain: []string{"init of a", "time.Now"}, what: "time.Now (clock)"}},
		Tests: []measuredTest{
			{Name: "TestPure", Reached: reached{}, Ran: true},
			{Name: "TestDisk", Reached: reached{
				FS:  witness{chain: []string{"a.TestDisk", "a.load", "os.ReadFile"}, what: "os.ReadFile (fs)"},
				Git: witness{chain: []string{"a.TestDisk", "a.git"}, what: `os/exec.Command spawns "git", which is git`},
			}},
		},
	}}
	var out strings.Builder
	writeListing(&out, pkgs)
	want := "\nexample.com/m/a\n" +
		"    setup clock: init of a → time.Now ⇒ time.Now (clock)\n" +
		"  unit        TestPure\n" +
		"  integration TestDisk  [not run]\n" +
		`    git: a.TestDisk → a.git ⇒ os/exec.Command spawns "git", which is git` + "\n" +
		"    fs: a.TestDisk → a.load → os.ReadFile ⇒ os.ReadFile (fs)\n"
	if out.String() != want {
		t.Errorf("listing:\n%s\nwant:\n%s", out.String(), want)
	}
}
