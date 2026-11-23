// testshape measures the suite's shape: for each package of a `go test -json`
// stream on stdin, how many top-level tests sit at each layer, read from the
// code that can run for each test.
//
//	(set -o pipefail; go test -short -json ./... > shape.json)
//	go run ./tools/testshape < shape.json
//	go run ./tools/testshape -tests < shape.json
//
// It prints one row per package: its layered tests and its skipped ones, the
// count at each layer and the elapsed of those tests summed, the unit share by
// count, how many tests have each touch, and the touches of the package's
// initializer and of its TestMain. A TOTAL row ends the table. -tests also
// lists every test with its layer, elapsed and touches, and one witness chain
// per touch: the calls from the test to the touch. Elapsed is the shared
// reader's (tools/internal/gotestjson), the clock tools/testbudget reports. A
// test's elapsed includes its waits — a parent's for its parallel subtests,
// and theirs for the package's serial tests — so an elapsed sum is not what a
// layer costs; the counts are the comparable figure.
//
// THE RULE. A test's layer is decided by its touches, the ways the code that
// can run for it leaves its process:
//
//   - network: a listener, a dial, an HTTP client or server;
//   - fs: the filesystem, the harness's TempDir and Chdir included;
//   - clock: the real clock (time.Now, a timer, a context deadline);
//   - git: git, run through Env.Git, Dolt's gitcmd, or any spawn of a
//     program named "git" or a path ending in /git;
//   - store: a store on disk, opened by the app, engine or store openers,
//     Dolt's database loader, or database/sql;
//   - proc: any other process, a program no constant names included;
//   - cli: a CLI run.
//
// A test that runs the CLI is end-to-end. A test with any other touch is
// integration. A test with no touch needs nothing outside its process and is
// unit. That is the one definition of unit, laws:testing's. A skipped test
// sits at no layer.
//
// WHAT CAN RUN FOR A TEST is everything reachable from its TestXxx function
// in the module's typed program, built with go/packages, go/types and SSA,
// resolved the way the compiler resolves it, never by matching names:
//
//   - A static call reaches its callee.
//   - A function or closure whose value is taken in the test's reach is
//     reachable, whoever calls it.
//   - A use of a package variable, of any type, reaches the slice of the
//     package initializers that computes it: the writes into the variable,
//     every value they depend on, every write into what the slice allocates
//     (a map's entries, a struct's fields), and every function the slice
//     calls or takes as a value. `var db = openStore()` reaches openStore,
//     a map of funcs reaches each func, and a hook a helper installs reaches
//     the helper, its program resolved at the initializer's call. A store
//     made anywhere else is code that is in the test's reach or not, so a
//     test that overwrites a package variable earns no layer for it: a seam
//     that moves a test down is a value passed in.
//     [LAW:no-shared-mutable-globals]
//   - A call through an interface reaches no implementation. Converting a
//     value to an interface reaches every method of its type: a module
//     type's are walked, and any type's markers are touches, so whoever
//     builds the value carries its touch. A test whose store is reached
//     through storage.Store carries the store at the opener that built it,
//     and an *http.Client stored as an interface carries the network.
//   - Only the module's functions are walked. A call out of the module is a
//     touch if it is a marker and is otherwise not followed, so the testing
//     package's own clock reads are never charged.
//
// The markers, the spawners and how a spawned program is resolved are data,
// in markers.go. A spawn's program is resolved from a constant where it
// enters: through the parameters and captured variables of as many helpers
// as carry it, and through a package variable's initializer. The witness
// names the program on the spawning call; a program no constant names is
// unresolved, another process. A spawn of the test binary itself
// (os.Executable, os.Args[0]) is a CLI run when the package's TestMain can
// reach a CLI run, and another process otherwise. That is decided per
// package, so in a package whose TestMain runs the CLI, a helper process
// that re-runs one test counts as end-to-end too. The touches of a package's
// initializer and of its TestMain print on its row and are charged to no
// test.
//
// A MEASURE THAT MEASURED NOTHING FAILS. testshape exits 1, naming the
// package or test, on a stream that ran no test, a package that failed to
// build, a test binary that failed outside any test, a test with no verdict,
// a package or a test the stream ran twice, a package that does not load, a
// test the loaded program does not declare or declares in both test
// packages, or a marker its package does not declare. A failed test is still
// measured: it is counted at its layer and marked in -tests. Test output and
// go test's own lines are replayed to stderr, so stdout is the report.
//
// -tags passes the measured run's build tags to the load, so the files
// classified are the files that run compiled.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// config is one run's inputs: the module to load, how to load it, which
// module markers count, and what to print.
type config struct {
	dir           string
	buildFlags    []string
	moduleMarkers []markerGroup
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
	fail := func(errs ...error) int {
		for _, err := range errs {
			fmt.Fprintln(stderr, "testshape:", err)
		}
		return 1
	}
	s, err := gotestjson.Read(stdin, stderr)
	if err != nil {
		return fail(err)
	}
	if refused := refusals(s); len(refused) > 0 {
		return fail(refused...)
	}
	ms, err := resolveMarkers(cfg.dir, cfg.moduleMarkers)
	if err != nil {
		return fail(members(err)...)
	}
	l, err := load(cfg.dir, cfg.buildFlags)
	if err != nil {
		return fail(members(err)...)
	}
	g := buildGraph(l, ms)
	shapes, err := classify(g, l, s.Packages)
	if err != nil {
		return fail(members(err)...)
	}
	printTable(stdout, l.module, shapes)
	if cfg.listTests {
		fmt.Fprintln(stdout)
		printTests(stdout, l.module, shapes)
	}
	for _, n := range failedTests(s) {
		fmt.Fprintln(stderr, "testshape:", n)
	}
	return 0
}

// members are the errors err joins, or err itself.
func members(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// refusals are the stream's reasons it measured nothing it can be trusted
// for, each naming its package or test.
func refusals(s gotestjson.Stream) []error {
	var out []error
	ran := len(s.Cut) > 0
	ended := make(map[string]bool)
	for _, p := range s.Packages {
		if ended[p.ImportPath] {
			out = append(out, fmt.Errorf("%s ended more than once; measure a run of each package once", p.ImportPath))
		}
		ended[p.ImportPath] = true
		if p.FailedBuild != "" {
			out = append(out, fmt.Errorf("%s failed to build (%s); its tests were not measured", p.ImportPath, p.FailedBuild))
		} else if p.MainFailed() {
			out = append(out, fmt.Errorf("%s's test binary failed outside any test (in TestMain, or before it ran its tests)", p.ImportPath))
		}
		seen := make(map[string]bool)
		for _, t := range p.Tests {
			ran = true
			if seen[t.Name] {
				out = append(out, fmt.Errorf("%s ran %s more than once; measure a run of each test once", p.ImportPath, t.Name))
			}
			seen[t.Name] = true
			if t.Verdict == gotestjson.Unfinished {
				out = append(out, fmt.Errorf("%s %s has no verdict: its package ended before it did", p.ImportPath, t.Name))
			}
		}
	}
	for _, c := range s.Cut {
		out = append(out, fmt.Errorf("%s %s was cut: the stream ended before its package did", c.Package, c.Test))
	}
	if !ran {
		out = append(out, errors.New("the stream ran no test"))
	}
	return out
}

// failedTests are the tests that failed, which are measured all the same:
// what the stream says that is not the measure.
func failedTests(s gotestjson.Stream) []string {
	var out []string
	for _, p := range s.Packages {
		for _, t := range p.Tests {
			if t.Verdict == gotestjson.Fail {
				out = append(out, fmt.Sprintf("%s %s failed; it is measured as far as it ran", p.ImportPath, t.Name))
			}
		}
	}
	return out
}
