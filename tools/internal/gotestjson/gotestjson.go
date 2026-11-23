// Package gotestjson reads a `go test -json` stream into what each package and
// each of its top-level tests did, and how long each took. It is the one reader
// of that stream in this repository's tools, so two tools fed one stream can
// never report different durations for one test. [LAW:one-source-of-truth]
//
// The reader has one rule for a malformed stream: a line that is JSON but not
// an event, an action go test does not emit, an event naming no package that
// is not a build event, a verdict without its Elapsed figure, or a stream with
// no package in it, is an error, never a guess.
// What differs between consumers (one prints an unfinished test, another
// refuses it) is not decided here: the reader returns those facts as typed
// values and each tool acts on them.
package gotestjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// Verdict is the verdict go test reported for a package.
type Verdict int

const (
	Pass Verdict = iota
	Fail
	Skip
)

// Ending is how a top-level test ended: with go test's verdict, or with none.
type Ending int

const (
	Passed Ending = iota
	Failed
	Skipped
	// Unfinished: the test's package ended with no verdict for it — a
	// -timeout panic, or a binary that died mid-test — so the package's end
	// stopped its clock.
	Unfinished
)

func (e Ending) String() string {
	return [...]string{"pass", "fail", "skip", "unfinished"}[e]
}

// Test is one top-level test of a package that ended. Subtests are never
// tests here: their time is already inside their top-level ancestor's.
type Test struct {
	Name     string
	Elapsed  time.Duration
	Parallel bool // it called t.Parallel (emitted a pause event)
	Ending   Ending
}

// Build is whether a package's test binary built: Built, or BuildFailed.
type Build interface{ build() }

// Built: the test binary built, and its run is the package's verdict.
type Built struct{}

// BuildFailed: the test binary did not build; Package names the package
// whose build failed.
type BuildFailed struct{ Package string }

func (Built) build()       {}
func (BuildFailed) build() {}

// Package is the stamped fact a package's terminal event proves: this import
// path finished, took this long, ended with this verdict, and ran these
// top-level tests, in the order their verdicts arrived and then, for tests
// with none, in name order. [LAW:parse-dont-validate] consumers work from
// these, never from raw stream lines.
type Package struct {
	ImportPath string
	Elapsed    time.Duration
	Verdict    Verdict
	Build      Build
	Tests      []Test
}

// Cut is a package the stream ended inside: it never reached its terminal
// event. Open lists its tests that had no verdict, in name order.
type Cut struct {
	ImportPath string
	Open       []string
}

// Run is everything one stream reported, in stream order.
type Run struct {
	Packages []Package
	Cut      []Cut // sorted by import path
}

// event is the subset of test2json's output this reader consumes (see
// $GOROOT/src/cmd/internal/test2json). A nil Elapsed means the event carries
// no duration, not a zero-second one.
type event struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
	// FailedBuild is set on a package's fail event when its test binary did
	// not build, naming the package whose build failed.
	FailedBuild string
}

// actions is go test's whole action set: test2json's, plus cmd/go's build
// events. [LAW:parse-dont-validate] any other action is a malformed stream.
var actions = map[string]bool{
	"start": true, "run": true, "pause": true, "cont": true, "output": true,
	"bench": true, "attr": true, "artifacts": true,
	"pass": true, "fail": true, "skip": true,
	"build-output": true, "build-fail": true,
}

var verdicts = map[string]Verdict{"pass": Pass, "fail": Fail, "skip": Skip}

var endings = map[string]Ending{"pass": Passed, "fail": Failed, "skip": Skipped}

// packageless are the actions that name their package by ImportPath, not
// Package: cmd/go's build events.
var packageless = map[string]bool{"build-output": true, "build-fail": true}

// testKey identifies one running test.
type testKey struct{ pkg, test string }

// runningTest is the one record of a test between its first event and its
// verdict: its buffered output and its clock. [LAW:one-source-of-truth]
// output replay, the parallel flag, and the unfinished listing all read it.
//
// The clock runs from the test's start to its end, less the wait at
// t.Parallel: time before the pause plus time since the resume. A finished
// test's end is the later of its own end and its subtests' — go test's
// Elapsed alone would list a table of t.Parallel subtests at 0.00s, because
// testing.tRunner sums a test's duration before it releases its parallel
// subtests. An unfinished test's end is its package's.
type runningTest struct {
	output   []string
	parallel bool
	paused   bool
	ran      time.Duration // accumulated up to the last pause
	since    time.Time     // when the clock last started; read only while !paused
	subEnd   time.Time     // the latest end of its finished subtests; set on top-level tests only
}

func (rt *runningTest) elapsedAt(end time.Time) time.Duration {
	if rt.paused {
		return rt.ran
	}
	return rt.ran + end.Sub(rt.since)
}

// endGiven is when a test that go test timed at elapsed ended, or its
// subtests did, whichever is later. A verdict event's Time is no witness:
// test2json holds each verdict until the stream's next line, so a parallel
// test that ends in a quiet stretch is stamped late. The resume (or start)
// it counts from is an update line, which test2json stamps on arrival.
// Cleanup that runs after a test's parallel subtests is the one span lost.
func (rt *runningTest) endGiven(elapsed time.Duration) time.Time {
	return later(rt.since.Add(elapsed-rt.ran), rt.subEnd)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// topLevel is the top-level ancestor of test, or test itself.
func topLevel(test string) string {
	name, _, _ := strings.Cut(test, "/")
	return name
}

func isTopLevel(test string) bool { return topLevel(test) == test }

// reader is the state of one stream read.
type reader struct {
	replay io.Writer
	// Every test between its first event and its verdict. Its output is held
	// until then: failed tests replay it, passing tests discard it — go
	// test's non-verbose contract.
	running map[testKey]*runningTest
	// Top-level tests that reached a verdict, held until their package ends.
	finished map[string][]Test
	// Packages that emitted an event and have not ended.
	open map[string]bool
	run  Run
}

// Read reads one `go test -json` stream from r and replays go test's
// human-readable output to replay while it reads: package output as it
// arrives, a test's accumulated output only when that test fails or its
// package ends without its verdict. Lines that are not JSON (a panic trace,
// stray build noise) are echoed verbatim — surfaced, never swallowed.
// [LAW:no-silent-failure]
func Read(r io.Reader, replay io.Writer) (Run, error) {
	rd := &reader{
		replay:   replay,
		running:  make(map[testKey]*runningTest),
		finished: make(map[string][]Test),
		open:     make(map[string]bool),
	}
	sc := bufio.NewScanner(r)
	// A single output line can exceed Scanner's 64KB default (a dumped
	// fixture, a long panic argument); give it room instead of erroring.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			fmt.Fprintf(replay, "%s\n", line)
			continue
		}
		var ev event
		err := json.Unmarshal(line, &ev)
		if err == nil {
			err = rd.event(ev)
		}
		if err != nil {
			return Run{}, fmt.Errorf("malformed go test -json stream at %s: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Run{}, fmt.Errorf("reading go test -json stream: %w", err)
	}
	// A stream cut off before a package's terminal event leaves that
	// package's tests running with no package result to list them on; their
	// output is still the diagnostic, so it surfaces. [LAW:no-silent-failure]
	for _, pkg := range slices.Sorted(maps.Keys(rd.open)) {
		var names []string
		for _, u := range rd.drain(pkg, "the stream ended before its package did") {
			names = append(names, u.test)
		}
		rd.run.Cut = append(rd.run.Cut, Cut{ImportPath: pkg, Open: names})
	}
	if len(rd.run.Packages) == 0 && len(rd.run.Cut) == 0 {
		return Run{}, fmt.Errorf("malformed go test -json stream: it reports no package")
	}
	return rd.run, nil
}

func (rd *reader) event(ev event) error {
	if !actions[ev.Action] {
		return fmt.Errorf("action %q is not one go test emits", ev.Action)
	}
	if ev.Action == "build-output" {
		io.WriteString(rd.replay, ev.Output)
	}
	if packageless[ev.Action] {
		// build-fail names its package by ImportPath; the package's own
		// terminal event carries the fact as FailedBuild.
		return nil
	}
	if ev.Package == "" {
		return fmt.Errorf("a %s event names no package", ev.Action)
	}
	if ev.Test == "" {
		return rd.packageEvent(ev)
	}
	rd.open[ev.Package] = true
	k := testKey{ev.Package, ev.Test}
	rt := track(rd.running, k, ev.Time)
	switch ev.Action {
	case "output":
		rt.output = append(rt.output, ev.Output)
	case "pause":
		rt.ran += ev.Time.Sub(rt.since)
		rt.paused, rt.parallel = true, true
	case "cont":
		rt.since, rt.paused = ev.Time, false
	case "pass", "fail", "skip":
		if ev.Elapsed == nil {
			return fmt.Errorf("test %s %s has no Elapsed", ev.Package, ev.Test)
		}
		if ev.Action == "fail" {
			for _, s := range rt.output {
				io.WriteString(rd.replay, s)
			}
		}
		delete(rd.running, k)
		end := rt.endGiven(seconds(*ev.Elapsed))
		if !isTopLevel(ev.Test) {
			// A subtest's verdict precedes its ancestor's, which is still
			// running.
			top := track(rd.running, testKey{ev.Package, topLevel(ev.Test)}, ev.Time)
			top.subEnd = later(top.subEnd, end)
			return nil
		}
		rd.finished[ev.Package] = append(rd.finished[ev.Package], Test{
			Name:     ev.Test,
			Elapsed:  rt.elapsedAt(end),
			Parallel: rt.parallel,
			Ending:   endings[ev.Action],
		})
	}
	return nil
}

func (rd *reader) packageEvent(ev event) error {
	verdict, terminal := verdicts[ev.Action]
	switch {
	case ev.Action == "output":
		io.WriteString(rd.replay, ev.Output)
	case terminal:
		if ev.Elapsed == nil {
			return fmt.Errorf("package %s's %s has no Elapsed", ev.Package, ev.Action)
		}
		tests := rd.finished[ev.Package]
		delete(rd.finished, ev.Package)
		delete(rd.open, ev.Package)
		for _, u := range rd.drain(ev.Package, "no verdict before its package ended") {
			if isTopLevel(u.test) {
				tests = append(tests, Test{
					Name:     u.test,
					Elapsed:  u.rt.elapsedAt(ev.Time),
					Parallel: u.rt.parallel,
					Ending:   Unfinished,
				})
			}
		}
		var build Build = Built{}
		if ev.FailedBuild != "" {
			build = BuildFailed{Package: ev.FailedBuild}
		}
		rd.run.Packages = append(rd.run.Packages, Package{
			ImportPath: ev.Package,
			Elapsed:    seconds(*ev.Elapsed),
			Verdict:    verdict,
			Build:      build,
			Tests:      tests,
		})
	default:
		rd.open[ev.Package] = true
	}
	return nil
}

// track returns k's record, starting it at t on the test's first event. In a
// real stream that event is `run`, which test2json emits from the `=== RUN`
// line, so t is when the test started.
func track(running map[testKey]*runningTest, k testKey, t time.Time) *runningTest {
	rt := running[k]
	if rt == nil {
		rt = &runningTest{since: t}
		running[k] = rt
	}
	return rt
}

// unfinishedTest is a test drained from the running set without a verdict.
type unfinishedTest struct {
	test string
	rt   *runningTest
}

// drain is the one place a test is decided unfinished: it removes pkg's
// still-running tests and prints their buffered output in name order, each
// under a line saying why it has no verdict. That output is exactly what a
// -timeout panic or a dying binary leaves behind, so it prints as the package
// ends, right after the package's FAIL line: go test prints it before that
// line, but which tests will never get a verdict is known only at the
// package's terminal event, which follows the line. [LAW:single-enforcer]
func (rd *reader) drain(pkg, why string) []unfinishedTest {
	var drained []unfinishedTest
	for k, rt := range rd.running {
		if k.pkg == pkg {
			drained = append(drained, unfinishedTest{k.test, rt})
			delete(rd.running, k)
		}
	}
	slices.SortFunc(drained, func(a, b unfinishedTest) int { return strings.Compare(a.test, b.test) })
	for _, u := range drained {
		fmt.Fprintf(rd.replay, "--- unfinished: %s %s (%s)\n", pkg, u.test, why)
		for _, s := range u.rt.output {
			io.WriteString(rd.replay, s)
		}
	}
	return drained
}

func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
