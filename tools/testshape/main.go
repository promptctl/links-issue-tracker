// testshape measures the suite's shape: for each package of a `go test -json`
// stream on stdin, how many top-level tests sit at each layer and how long
// they took, read from the code that can run for each test.
//
//	(set -o pipefail; go test -short -json ./... > shape.json)
//	go run ./tools/testshape < shape.json
//	go run ./tools/testshape -tests < shape.json
//
// It prints one row per package: the count and wall time of its top-level
// tests at each layer, the unit share by count, how many tests have each
// touch, and its TestMain's touches. A TOTAL row ends the table. -tests also
// lists every test with its layer, time, touches and one witness chain per
// touch: the calls from the test to the touch. Times are the shared reader's
// (tools/internal/gotestjson), the clock tools/testbudget reports.
//
// THE RULE. A test's layer is decided by its touches, the ways the code that
// can run for it leaves its process:
//
//   - network: a listener, a dial, an HTTP client or server;
//   - fs: the filesystem, the harness's TempDir and Chdir included;
//   - clock: the real clock (time.Now, a timer, a context deadline);
//   - git: git, run through Env.Git, Dolt's gitcmd, or any spawn of a
//     program named "git" or a path ending in /git;
//   - store: a store on disk, opened by the app, engine or store openers or
//     database/sql;
//   - proc: any other process;
//   - cli: a CLI run.
//
// A test that runs the CLI is end-to-end. A test with any other touch is
// integration. A test with no touch needs nothing outside its process and is
// unit. That is the one definition of unit, laws:testing's.
//
// WHAT CAN RUN FOR A TEST is everything reachable from its TestXxx function
// in the module's typed program, built with go/packages, go/types and SSA,
// resolved the way the compiler resolves it, never by matching names:
//
//   - A static call reaches its callee.
//   - A function or closure used as a value is reached by the function that
//     uses it.
//   - A read of a package variable of function type reaches every function
//     stored into it, except that the variable's initializer is dropped when
//     a function the test reaches, other than package init, stores into it,
//     or the package's TestMain does: that is a seam the test replaced.
//   - A call through an interface reaches no implementation. Converting a
//     value of a module type to an interface reaches every method of the
//     type, so whoever builds the value carries its touch: a test whose store
//     is reached through storage.Store carries the store at the opener that
//     built it.
//   - Only the module's functions are walked. A call out of the module is a
//     touch if it is a marker and is otherwise not followed, so the testing
//     package's own clock reads are never charged.
//
// The markers, the spawners and how a spawned program is resolved are data,
// in markers.go. A spawn's program is resolved by value, through the
// parameters and free variables of as many helpers as carry it. A spawn of
// the test binary itself (os.Executable, os.Args[0]) is a CLI run when the
// package's TestMain, in the branch that does not run the tests, reaches a
// CLI run; otherwise it is the helper-process idiom, another process. A
// TestMain's own touches print on its package's row and are charged to no
// test.
//
// A MEASURE THAT MEASURED NOTHING FAILS. testshape exits 1, naming the
// package or test, on a stream that ran no test, a package that failed to
// build, a test with no verdict, a verdict with no elapsed time, a package
// that does not load, a test the loaded program does not declare, or a
// marker that names no function of the module. A failed test is still
// measured: it is counted at its layer and marked in -tests. Test output and
// go test's own lines are replayed to stderr, so stdout is the report.
//
// -tags passes the measured run's build tags to the load, so the files
// classified are the files that run compiled.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// config is one run's inputs: the module to load, how to load it, which
// markers count, and what to print.
type config struct {
	dir           string
	buildFlags    []string
	moduleMarkers []marker
	listTests     bool
}

func main() {
	listTests := flag.Bool("tests", false, "also list every test with its layer, touches and witness chains")
	tags := flag.String("tags", "", "the build tags of the measured go test run")
	flag.Parse()
	cfg := config{dir: ".", moduleMarkers: moduleMarkers, listTests: *listTests}
	if *tags != "" {
		cfg.buildFlags = []string{"-tags=" + *tags}
	}
	os.Exit(run(cfg, os.Stdin, os.Stdout, os.Stderr))
}

// run measures the stream on stdin against the module in cfg.dir and reports
// on stdout; it returns the exit status.
func run(cfg config, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(lines ...string) int {
		for _, l := range lines {
			fmt.Fprintln(stderr, "testshape:", l)
		}
		return 1
	}
	s, err := gotestjson.Read(stdin, stderr)
	if err != nil {
		return fail(err.Error())
	}
	if refused := refusals(s); len(refused) > 0 {
		return fail(refused...)
	}
	l, err := load(cfg.dir, cfg.buildFlags)
	if err != nil {
		return fail(strings.Split(err.Error(), "\n")...)
	}
	if missing := undeclared(l.module, cfg.moduleMarkers, l.types); len(missing) > 0 {
		lines := make([]string, len(missing))
		for i, m := range missing {
			lines[i] = "marker " + m + " names no function the module declares (tools/testshape/markers.go)"
		}
		return fail(lines...)
	}
	g := buildGraph(l, newMarkerSet(l.module, cfg.moduleMarkers, externalMarkers, spawners))
	shapes, err := classify(g, l, s.Packages)
	if err != nil {
		return fail(strings.Split(err.Error(), "\n")...)
	}
	printTable(stdout, l.module, shapes)
	if cfg.listTests {
		fmt.Fprintln(stdout)
		printTests(stdout, l.module, shapes)
	}
	for _, n := range notes(s) {
		fmt.Fprintln(stderr, "testshape:", n)
	}
	return 0
}

// refusals are the stream's reasons it measured nothing it can be trusted
// for, each naming its package or test.
func refusals(s gotestjson.Stream) []string {
	var out []string
	ran := false
	for _, p := range s.Packages {
		if p.FailedBuild != "" {
			out = append(out, fmt.Sprintf("%s failed to build (%s); its tests were not measured", p.ImportPath, p.FailedBuild))
		}
		for _, t := range p.Tests {
			ran = true
			switch {
			case t.Unfinished:
				out = append(out, fmt.Sprintf("%s %s has no verdict: its package ended before it did", p.ImportPath, t.Name))
			case t.NoElapsed:
				out = append(out, fmt.Sprintf("%s %s has a verdict with no elapsed time", p.ImportPath, t.Name))
			}
		}
	}
	for _, c := range s.Cut {
		out = append(out, fmt.Sprintf("%s %s has no verdict: the stream ended before its package did", c.Package, c.Test))
	}
	if !ran && len(s.Cut) == 0 {
		out = append(out, "the stream ran no test")
	}
	return out
}

// notes are what a measured stream says that is not the measure: tests that
// failed, which are counted all the same, and binaries that failed outside
// any test.
func notes(s gotestjson.Stream) []string {
	var out []string
	for _, p := range s.Packages {
		if p.MainFailed() {
			out = append(out, fmt.Sprintf("%s's test binary failed outside any test", p.ImportPath))
		}
		for _, t := range p.Tests {
			if t.Failed {
				out = append(out, fmt.Sprintf("%s %s failed; it is measured as far as it ran", p.ImportPath, t.Name))
			}
		}
	}
	return out
}
