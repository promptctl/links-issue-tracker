// Package gotestjson reads one `go test -json` stream into the facts it
// proves: each package that ended, with its top-level tests and the time each
// took. It is the one reader and the one per-test clock of every tool that
// reads such a stream, so two tools never report different durations for one
// test. [LAW:one-source-of-truth]
//
// It reports facts, not verdicts. A package that failed to build, a test with
// no verdict, a verdict that carried no elapsed time: each is a field of the
// result, and what a fact means is the consuming tool's decision.
// [LAW:single-enforcer] What it cannot read is an error: a package-level
// action go test does not emit, or a replay write that failed.
package gotestjson

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// event is the subset of test2json's output this reader consumes (see
// $GOROOT/src/cmd/internal/test2json). A nil Elapsed means the event carries
// no duration, not a zero-second one.
type event struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     *float64
	Output      string
	FailedBuild string
}

// Stream is everything one stream proved.
type Stream struct {
	// Packages are the packages whose terminal event the stream carried, in
	// stream order.
	Packages []Package
	// Cut are the tests of the packages that never reported their end, in
	// package then test order: the tests still running when the stream ended,
	// and the top-level tests that reached a verdict before it did, which no
	// package result lists.
	Cut []TestID
}

// TestID names one test of one package.
type TestID struct{ Package, Test string }

// Package is the stamped fact a package's terminal event proves: this import
// path finished, took this long, passed or failed, and ran these top-level
// tests. [LAW:parse-dont-validate] everything downstream works from these,
// never from raw stream lines.
type Package struct {
	ImportPath string
	Elapsed    time.Duration
	// NoElapsed: the terminal event carried no elapsed time, so Elapsed is no
	// figure at all, not a zero-second one.
	NoElapsed bool
	Failed    bool
	// FailedBuild is the build go test reported failing for this package,
	// empty when its test binary built.
	FailedBuild string
	Tests       []Test
}

// MainFailed reports that the package's test binary failed with no test to
// blame: its TestMain failed, or the binary died outside every test.
func (p Package) MainFailed() bool {
	if !p.Failed || p.FailedBuild != "" {
		return false
	}
	return !slices.ContainsFunc(p.Tests, func(t Test) bool { return t.Verdict == Fail || t.Verdict == Unfinished })
}

// Verdict is how one top-level test ended.
type Verdict int

const (
	Pass Verdict = iota
	Fail
	Skip
	// Unfinished: the package ended with no verdict for the test — a
	// -timeout panic, or a binary that died mid-test — so the package's end
	// stopped its clock instead.
	Unfinished
)

// verdicts are go test's verdict actions, for a test and for a package.
var verdicts = map[string]Verdict{"pass": Pass, "fail": Fail, "skip": Skip}

// Test is one top-level test of a package that ended. Subtests are never
// results: their time is already inside their top-level ancestor's, because
// Elapsed is the ancestor's clock (see runningTest).
type Test struct {
	Name     string
	Elapsed  time.Duration
	Parallel bool // it called t.Parallel (emitted a pause event)
	Verdict  Verdict
	// NoElapsed: its verdict carried no elapsed time, so Elapsed is the
	// clock's reading without go test's own figure.
	NoElapsed bool
}

// testKey identifies one running test.
type testKey struct{ pkg, test string }

// runningTest is the one record of a test between its first event and its
// verdict: its buffered output and its clock. [LAW:one-source-of-truth]
// output replay, the parallel flag, and the unfinished listing all read this
// record.
//
// The clock runs from the test's start to its end, less the wait at
// t.Parallel: time before the pause plus time since the resume. It is every
// listed test's Elapsed. [LAW:one-source-of-truth] A finished test's end is
// the later of its own end and its subtests' — go test's Elapsed alone would
// list a table of t.Parallel subtests at 0.00s, because testing.tRunner sums
// a test's duration before it releases its parallel subtests. An unfinished
// test's end is its package's.
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

// replay is the writer output is replayed to. It keeps the first write that
// failed, which Read returns: a replay that lost output is no replay.
// [LAW:no-silent-failure]
type replay struct {
	w   io.Writer
	err error
}

func (r *replay) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.w.Write(p)
	r.err = err
	return n, err
}

// Read reads one `go test -json` stream from r, replaying human-readable
// output to w, and returns what the stream proved. Lines that are not JSON
// events (a panic trace, stray build noise) are echoed to w verbatim —
// surfaced, never swallowed. [LAW:no-silent-failure]
func Read(r io.Reader, w io.Writer) (Stream, error) {
	out := &replay{w: w}
	// Every test between its first event and its verdict. Its output is held
	// until then: failed tests replay it, passing tests discard it — go
	// test's non-verbose contract.
	running := make(map[testKey]*runningTest)
	// Top-level tests that reached a verdict, held until their package ends.
	finished := make(map[string][]Test)

	var s Stream
	sc := bufio.NewScanner(r)
	// A single output line can exceed Scanner's 64KB default (a dumped
	// fixture, a long panic argument); give it room instead of erroring.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Bytes()
		var ev event
		if err := json.Unmarshal(raw, &ev); err != nil {
			fmt.Fprintf(out, "%s\n", raw)
			continue
		}
		if ev.Test == "" {
			switch ev.Action {
			case "start", "build-fail":
				// A package starting, and a build failing, which the
				// package's own terminal event names in FailedBuild.
			case "output", "build-output":
				io.WriteString(out, ev.Output)
			case "pass", "fail", "skip":
				tests := finished[ev.Package]
				delete(finished, ev.Package)
				for _, u := range drain(out, running, ev.Package, "no verdict before its package ended") {
					if isTopLevel(u.test) {
						tests = append(tests, Test{
							Name:     u.test,
							Elapsed:  u.rt.elapsedAt(ev.Time),
							Parallel: u.rt.parallel,
							Verdict:  Unfinished,
						})
					}
				}
				s.Packages = append(s.Packages, Package{
					ImportPath:  ev.Package,
					Elapsed:     seconds(ev.Elapsed),
					NoElapsed:   ev.Elapsed == nil,
					Failed:      ev.Action == "fail",
					FailedBuild: ev.FailedBuild,
					Tests:       tests,
				})
			default:
				return Stream{}, fmt.Errorf("line %d: package %s: %q is no package action go test emits", line, ev.Package, ev.Action)
			}
			continue
		}

		k := testKey{ev.Package, ev.Test}
		rt := track(running, k, ev.Time)
		switch ev.Action {
		case "output":
			rt.output = append(rt.output, ev.Output)
		case "pause":
			rt.ran += ev.Time.Sub(rt.since)
			rt.paused, rt.parallel = true, true
		case "cont":
			rt.since, rt.paused = ev.Time, false
		case "pass", "fail", "skip":
			if ev.Action == "fail" {
				for _, s := range rt.output {
					io.WriteString(out, s)
				}
			}
			delete(running, k)
			end := rt.endGiven(seconds(ev.Elapsed))
			if !isTopLevel(ev.Test) {
				// A subtest's verdict precedes its ancestor's, which is
				// still running.
				top := track(running, testKey{ev.Package, topLevel(ev.Test)}, ev.Time)
				top.subEnd = later(top.subEnd, end)
				continue
			}
			finished[ev.Package] = append(finished[ev.Package], Test{
				Name:      ev.Test,
				Elapsed:   rt.elapsedAt(end),
				Parallel:  rt.parallel,
				Verdict:   verdicts[ev.Action],
				NoElapsed: ev.Elapsed == nil,
			})
		}
	}
	if err := sc.Err(); err != nil {
		return Stream{}, fmt.Errorf("reading go test -json stream: %w", err)
	}
	// A stream cut off before a package's terminal event leaves that
	// package's tests, running or finished, with no package result to list
	// them on; the running ones' output is still the diagnostic, so it
	// surfaces. [LAW:no-silent-failure]
	var unended []string
	for k := range running {
		unended = append(unended, k.pkg)
	}
	for pkg := range finished {
		unended = append(unended, pkg)
	}
	sort.Strings(unended)
	for _, pkg := range slices.Compact(unended) {
		var cut []string
		for _, t := range finished[pkg] {
			cut = append(cut, t.Name)
		}
		for _, u := range drain(out, running, pkg, "the stream ended before its package did") {
			cut = append(cut, u.test)
		}
		sort.Strings(cut)
		for _, test := range cut {
			s.Cut = append(s.Cut, TestID{pkg, test})
		}
	}
	if out.err != nil {
		return Stream{}, fmt.Errorf("replaying go test's output: %w", out.err)
	}
	return s, nil
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
func drain(w io.Writer, running map[testKey]*runningTest, pkg, why string) []unfinishedTest {
	var drained []unfinishedTest
	for k, rt := range running {
		if k.pkg == pkg {
			drained = append(drained, unfinishedTest{k.test, rt})
			delete(running, k)
		}
	}
	sort.Slice(drained, func(i, j int) bool { return drained[i].test < drained[j].test })
	for _, u := range drained {
		fmt.Fprintf(w, "--- unfinished: %s %s (%s)\n", pkg, u.test, why)
		for _, s := range u.rt.output {
			io.WriteString(w, s)
		}
	}
	return drained
}

// seconds converts test2json's Elapsed to a Duration; nil, no duration, is
// the zero its NoElapsed fact qualifies.
func seconds(elapsed *float64) time.Duration {
	if elapsed == nil {
		return 0
	}
	return time.Duration(*elapsed * float64(time.Second))
}
