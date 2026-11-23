// testshape measures the test suite's shape: per package, how many top-level
// tests sit at each layer (unit, integration, end-to-end) and how much wall
// time each layer takes. A test's layer is decided by what its code reaches,
// read from source, never by its file name or a comment.
//
// Run it from the module root on a -short run of the suite:
//
//	go test -short -json ./... | go run ./tools/testshape
//
// The stream gives each top-level test's verdict and wall time: go test's
// elapsed for it, or the sum of its direct subtests' when that is larger, as
// it is for subtests that run in parallel. The source gives what it touches:
// testshape parses the package's Go files, its tests included, and walks every
// function the test names, through its helpers and into the module's other
// packages (see analyzer), marking each marker it reaches (markers.go):
//
//   - cli: it runs the lit command, in process or as a binary;
//   - store: it opens a Dolt store on disk;
//   - git: it starts git;
//   - proc: it starts some other child process;
//   - fs: it reads or writes the filesystem, a t.TempDir included;
//   - clock-wait: it waits on the real clock (a sleep, a timer, a deadline);
//   - clock-read: it reads the real clock as a value.
//
// The layer follows from the touches (layerOf): end-to-end if the test runs
// the CLI; integration if it otherwise reaches git, a store on disk, another
// process, or a wait on the real clock; unit if none of those. A unit test
// needs nothing outside its process, but touching the filesystem or reading
// the clock leaves it unit: those counts are columns of their own so they
// stay in view.
//
// It prints one row per package and a total row. -tests also lists every test
// with its layer, time, and, for each touch, the chain of calls from the test
// to the marker that gave it, so a classification can be checked by reading
// the code.
//
// A test the stream ran whose source testshape cannot find is named on stderr
// and fails the run, after the table prints. [LAW:no-silent-failure]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strings"
)

func main() {
	listTests := flag.Bool("tests", false, "list every test with its layer, touches, and time")
	flag.Parse()
	fsys := os.DirFS(".")
	modPath, err := modulePath(fsys)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testshape: %v (run it from the module root)\n", err)
		os.Exit(2)
	}
	timed, err := readStream(os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testshape: %v\n", err)
		os.Exit(2)
	}
	shapes, unread := classify(newAnalyzer(fsys, modPath), timed)
	if *listTests {
		printTests(os.Stdout, modPath, shapes)
	}
	printTable(os.Stdout, modPath, shapes)
	for _, err := range unread {
		fmt.Fprintf(os.Stderr, "testshape: %v\n", err)
	}
	if len(unread) > 0 {
		os.Exit(1)
	}
}

// modulePath reads the module path from go.mod at the root of fsys.
func modulePath(fsys iofs.FS) (string, error) {
	f, err := fsys.Open("go.mod")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readModulePath(f)
}

func readModulePath(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("go.mod has no module line")
}

// testShape is one top-level test: what the stream measured and what its code
// reaches.
type testShape struct {
	timedTest
	reach
	layer layer
}

// classify joins each timed test to its source. A test whose source cannot
// be read is returned as an error, not guessed at.
func classify(a *analyzer, timed []timedTest) ([]testShape, []error) {
	var shapes []testShape
	var unread []error
	for _, t := range timed {
		f, err := a.testFunc(t.Package, t.Name)
		if err != nil {
			unread = append(unread, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		r, err := a.reachOf(f)
		if err != nil {
			unread = append(unread, fmt.Errorf("%s %s: %w", t.Package, t.Name, err))
			continue
		}
		shapes = append(shapes, testShape{t, r, layerOf(r.touches)})
	}
	return shapes, unread
}
