// testshape measures the test suite's shape per layer: for every top-level
// test a `go test -json` stream ran (or declared and did not run), which
// layer it sits at, decided by what its code can reach outside the process,
// and how long the tests at each layer took.
//
//	set -o pipefail
//	{ go test -short -json ./... && go -C internal/vendor/dolthub-driver test -short -json ./...; } > shape.json
//	go run ./tools/testshape [-tests] [-tags list] < shape.json
//
// It prints a table, one row per package: the count of top-level tests at
// each layer, their elapsed summed per layer, the unit share, and per touch
// the tests that can reach it. -tests adds a listing of every test's layer,
// its touches, and one witness chain per touch.
//
// THE INVARIANT. The unit share is the figure the layered-test epic's
// done-when is checked against, so a measure that overstates it certifies
// work that was never done. Every approximation this tool makes errs away
// from unit: an imprecision may add a touch a test does not have, or move a
// test between integration and end-to-end; it never removes a touch.
//
// TOUCHES. A store on disk, git, a CLI run, any other process, the network,
// the filesystem, and the real clock. A test that can reach none is unit; one
// that can reach a CLI run is end-to-end; any other is integration.
//
// REACH. What a test can run is decided without following values to where
// they are stored: stores into package variables, pointers, fields, maps and
// initializer contents have unbounded shapes, and an analysis that follows
// some of them silently drops a touch on every other one. Instead reach is
// decided by which function values and concrete types exist, in the manner of
// rapid type analysis:
//
//   - The binary level of a test binary is the reach of the initialization of
//     every package the binary contains, and of its TestMain, which run before
//     every test. The function values they take and the concrete types they
//     convert to an interface are the possible targets of a test's dynamic and
//     interface calls, each carrying its origin: the initializer or TestMain
//     that made it exist. Their own touches print on the package's row.
//   - From each test: a static call reaches its callee. A function value that
//     is taken is reachable. A conversion to an interface makes reachable every
//     method of the converted type and of every type reachable from its
//     structure (fields, elements, pointees), outside types included, since
//     fmt and encoding/json call them by reflection; an interface in that
//     structure stands for every binary-level type implementing it. A dynamic
//     call reaches every binary-level function value of its signature; an
//     interface call reaches that method of every binary-level type that
//     implements the interface. A value with no use creates nothing, so
//     `var _ I = (*T)(nil)` makes no method reachable.
//   - Code outside the module is never walked. Every outside function module
//     code reaches is classified in classification.json: the touches it can
//     make, none, or dispatching — it calls back what was registered with it
//     or handed to it (a command tree's Execute, a server, a database driver),
//     so a call reaches every binary-level function value and every method of
//     every binary-level type. An outside function with no classification
//     refuses the run, naming it and its first caller.
//   - A spawn's program is the one value followed: through parameters (at the
//     static calls in the test's reach) and closure bindings to a constant,
//     os.Executable(), os.Args[0], or exec.LookPath of a constant. It is git
//     when it names git; a CLI run when it is the test binary and the binary's
//     TestMain reaches the CLI; any other program, resolved or not, is another
//     process, and the witness says which.
//
// REFUSALS. A measure that measured nothing fails loudly, exiting 1 and
// naming each cause: an empty stream; a package that failed to build, whose
// binary died outside any test, or that ended with a test still running; a
// stream that ended inside a package; a test or package the stream ran
// twice; a test the stream ran that its binary does not declare; a module of
// the suite (every go.mod outside testdata, as ./... walks) whose tests are
// absent from the stream; a classification that names nothing; an outside
// function with no classification; a module function with no Go body to walk.
// A malformed stream exits 2.
//
// THE STREAM is read through tools/internal/gotestjson, the reader and clock
// tools/testbudget uses, so the two never report different durations for one
// test. Every declared top-level test counts in its layer, since its layer
// comes from its static reach; a test the run skipped or did not select
// contributes no duration and is listed as not run.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

//go:embed classification.json
var classificationData []byte

func main() {
	os.Exit(run(classificationData, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(classData []byte, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("testshape", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listTests := fs.Bool("tests", false, "list each test's layer, touches, and a witness per touch")
	tags := fs.String("tags", "", "the -tags the measured run passed to go test")
	root := fs.String("root", ".", "the repository whose suite the stream measured")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var buildFlags []string
	if *tags != "" {
		buildFlags = []string{"-tags=" + *tags}
	}
	class, err := parseClassification(classData)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return 2
	}
	stream, err := gotestjson.Read(stdin, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return 2
	}
	pkgs, refusals, err := measureSuite(*root, buildFlags, class, stream)
	if err != nil {
		fmt.Fprintf(stderr, "testshape: %v\n", err)
		return 2
	}
	if len(refusals) > 0 {
		for _, r := range refusals {
			fmt.Fprintf(stderr, "testshape: refused: %s\n", r)
		}
		return 1
	}
	var rows []row
	for _, m := range pkgs {
		rows = append(rows, rowOf(m.measuredPackage, m.module.Path))
	}
	writeTable(stdout, rows)
	if *listTests {
		var listed []measuredPackage
		for _, m := range pkgs {
			listed = append(listed, m.measuredPackage)
		}
		writeListing(stdout, listed)
	}
	return 0
}

type modulePackage struct {
	measuredPackage
	module module
}

// measureSuite measures every package of stream against the suite under
// root, returning the measured packages in import-path order and every
// refusal. err is for a failure to measure at all.
func measureSuite(root string, buildFlags []string, class *Classification, stream gotestjson.Run) ([]modulePackage, []string, error) {
	refusals := streamRefusals(stream)
	mods, err := suiteModules(root)
	if err != nil {
		return nil, nil, err
	}
	if err := class.Validate(mods, buildFlags); err != nil {
		refusals = append(refusals, err.Error())
	}
	refusals = append(refusals, moduleRefusals(mods, stream)...)

	var out []modulePackage
	for i, m := range mods {
		var mine []gotestjson.Package
		for _, p := range stream.Packages {
			if moduleIndex(mods, p.ImportPath) == i {
				mine = append(mine, p)
			}
		}
		if len(mine) == 0 {
			continue
		}
		l, err := loadModule(m, buildFlags)
		if err != nil {
			return nil, nil, err
		}
		a := newAnalyzer(l.Prog, m, class)
		for _, p := range mine {
			b := l.Binaries[p.ImportPath]
			if b == nil {
				if len(p.Tests) > 0 {
					refusals = append(refusals, fmt.Sprintf("%s: the stream ran tests in a package with no test binary", p.ImportPath))
				}
				continue
			}
			mp, rs := join(p, a.measure(b))
			refusals = append(refusals, rs...)
			out = append(out, modulePackage{mp, m})
		}
		refusals = append(refusals, analysisRefusals(a)...)
	}
	slices.SortFunc(out, func(a, b modulePackage) int { return strings.Compare(a.ImportPath, b.ImportPath) })
	return out, refusals, nil
}
