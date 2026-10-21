// testbudget enforces the per-package wall-clock budgets in budgets.go against
// a `go test -json` stream on stdin. It exists so the suite's runtime is an
// observable that fails the build out loud instead of a number nobody watches.
// [LAW:no-silent-failure] the budget IS the alarm — a package over budget fails
// CI naming the package, the overage, and the package's slowest top-level
// tests, never a warning.
//
// It is invoked by ci.yml's Test step as the consumer of the gating run itself:
//
//	go test -short -json ./... | go run ./tools/testbudget
//
// so enforcement adds no CI time and measures the exact conditions it gates
// (the concurrent -short run on CI hardware). The tool replays go test's
// human-readable contract while it reads — package result lines as they
// arrive, a test's accumulated output only when that test fails or its package
// ends without its verdict — then prints
// every package's elapsed-vs-budget table so each CI log doubles as the
// calibration record for the next re-baseline.
//
// Test success is not this tool's job: the step runs under bash's pipefail, so
// `go test`'s own exit code remains the one enforcer of the suite being green.
// [LAW:single-enforcer] testbudget exits nonzero only for a blown budget or a
// stream it could not read.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// slowestListed is how many of an over-budget package's top-level tests its
// violation names.
const slowestListed = 10

// event is the subset of test2json's output this tool consumes (see
// $GOROOT/src/cmd/internal/test2json). A nil Elapsed means the event carries
// no duration, not a zero-second one.
type event struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
}

// packageResult is the stamped fact a package's terminal event proves: this
// import path finished, took this long, passed or failed, and ran these
// top-level tests. [LAW:parse-dont-validate] everything downstream (the
// table, enforcement, the slowest-test listing) works from these, never from
// raw stream lines.
type packageResult struct {
	ImportPath string
	Elapsed    time.Duration
	Failed     bool
	Tests      []testResult
}

// testResult is one top-level test of a package that ended. Subtests are
// never results: their time is already inside their top-level ancestor's.
type testResult struct {
	Name     string
	Elapsed  time.Duration
	Parallel bool // it called t.Parallel (emitted a pause event)
	// Unfinished: the package ended with no verdict for this test — a
	// -timeout panic, or a binary that died mid-test. Elapsed is then its
	// clock when the package ended, not go test's figure.
	Unfinished bool
}

type violation struct {
	ImportPath string
	Elapsed    time.Duration
	Budget     time.Duration
	Ran        int          // top-level tests the package ran
	Slowest    []testResult // its slowestListed slowest, slowest first
}

// testKey identifies one running test.
type testKey struct{ pkg, test string }

// runningTest is the one record of a test between its first event and its
// verdict: its buffered output and its clock. [LAW:one-source-of-truth] output
// replay, the parallel flag, and the unfinished listing all read this record.
//
// The clock measures what testing.(*T).Parallel measures: time before the
// pause plus time since the resume, never the wait in between — the same
// figure go test reports as a finished test's Elapsed.
type runningTest struct {
	output   []string
	parallel bool
	paused   bool
	ran      time.Duration // accumulated up to the last pause
	since    time.Time     // when the clock last started; read only while !paused
}

func (rt *runningTest) elapsedAt(end time.Time) time.Duration {
	if rt.paused {
		return rt.ran
	}
	return rt.ran + end.Sub(rt.since)
}

func isTopLevel(test string) bool { return !strings.Contains(test, "/") }

// budgetFor resolves one package's budget — its listed entry or the default,
// so no package is ever exempt. The one home of the resolution rule, shared
// by enforcement and the printed table so the two can never disagree about a
// package's budget. [LAW:single-enforcer]
func budgetFor(path string, budgets map[string]time.Duration, def time.Duration) time.Duration {
	if b, ok := budgets[path]; ok {
		return b
	}
	return def
}

// consume reads one `go test -json` stream from r, replaying human-readable
// output to w, and returns each package's terminal result in stream order.
// Lines that are not JSON events (a panic trace, stray build noise) are echoed
// to w verbatim — surfaced, never swallowed. [LAW:no-silent-failure]
func consume(r io.Reader, w io.Writer) ([]packageResult, error) {
	// Every test between its first event and its verdict. Its output is held
	// until then: failed tests replay it, passing tests discard it — go
	// test's non-verbose contract.
	running := make(map[testKey]*runningTest)
	// Top-level tests that reached a verdict, held until their package ends.
	finished := make(map[string][]testResult)

	var results []packageResult
	sc := bufio.NewScanner(r)
	// A single output line can exceed Scanner's 64KB default (a dumped
	// fixture, a long panic argument); give it room instead of erroring.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			fmt.Fprintf(w, "%s\n", line)
			continue
		}
		if ev.Test == "" {
			switch ev.Action {
			case "output", "build-output":
				io.WriteString(w, ev.Output)
			case "pass", "fail", "skip":
				tests := finished[ev.Package]
				delete(finished, ev.Package)
				for _, u := range drain(w, running, ev.Package, "no verdict before its package ended") {
					if isTopLevel(u.test) {
						tests = append(tests, testResult{
							Name:       u.test,
							Elapsed:    u.rt.elapsedAt(ev.Time),
							Parallel:   u.rt.parallel,
							Unfinished: true,
						})
					}
				}
				results = append(results, packageResult{
					ImportPath: ev.Package,
					Elapsed:    seconds(ev.Elapsed),
					Failed:     ev.Action == "fail",
					Tests:      tests,
				})
			}
			continue
		}

		k := testKey{ev.Package, ev.Test}
		rt := running[k]
		if rt == nil {
			// In a real stream the first event is `run`, which test2json
			// emits from the `=== RUN` line, so this is when the test started.
			rt = &runningTest{since: ev.Time}
			running[k] = rt
		}
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
					io.WriteString(w, s)
				}
			}
			delete(running, k)
			if isTopLevel(ev.Test) {
				finished[ev.Package] = append(finished[ev.Package], testResult{
					Name:     ev.Test,
					Elapsed:  seconds(ev.Elapsed),
					Parallel: rt.parallel,
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading go test -json stream: %w", err)
	}
	// A stream cut off before a package's terminal event leaves that
	// package's tests running with no package result to list them on; their
	// output is still the diagnostic, so it surfaces. [LAW:no-silent-failure]
	var unreported []string
	for k := range running {
		unreported = append(unreported, k.pkg)
	}
	sort.Strings(unreported)
	for _, pkg := range slices.Compact(unreported) {
		drain(w, running, pkg, "the stream ended before its package did")
	}
	return results, nil
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
// ends, beside the package's FAIL line. [LAW:single-enforcer]
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

// seconds converts test2json's Elapsed, where nil means the event carries no
// duration, to a Duration.
func seconds(elapsed *float64) time.Duration {
	if elapsed == nil {
		return 0
	}
	return time.Duration(*elapsed * float64(time.Second))
}

// enforce checks every result against its budget — a listed number or the
// default, so no package is ever exempt and a new slow package cannot creep in
// unnamed. [LAW:dataflow-not-control-flow] the check always runs; only the
// budget value varies.
func enforce(results []packageResult, budgets map[string]time.Duration, def time.Duration) []violation {
	var viols []violation
	for _, r := range results {
		budget := budgetFor(r.ImportPath, budgets, def)
		if r.Elapsed > budget {
			viols = append(viols, violation{r.ImportPath, r.Elapsed, budget, len(r.Tests), slowest(r.Tests)})
		}
	}
	return viols
}

// slowest returns tests' slowestListed slowest, slowest first; ties go to
// name order so a listing is the same on every run of the same stream.
func slowest(tests []testResult) []testResult {
	s := slices.Clone(tests)
	sort.Slice(s, func(i, j int) bool {
		if s[i].Elapsed != s[j].Elapsed {
			return s[i].Elapsed > s[j].Elapsed
		}
		return s[i].Name < s[j].Name
	})
	return s[:min(len(s), slowestListed)]
}

// printTable renders every package's elapsed against its budget, slowest
// first. It prints on success too: each CI log is the calibration record the
// next re-baseline of budgets.go reads its numbers from.
func printTable(w io.Writer, results []packageResult, budgets map[string]time.Duration, def time.Duration) {
	sorted := append([]packageResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Elapsed > sorted[j].Elapsed })
	fmt.Fprintf(w, "\ntestbudget: per-package wall clock vs budget (budgets: tools/testbudget/budgets.go)\n")
	for _, r := range sorted {
		// A failed package's elapsed is however far it got before dying, not
		// a measurement — mark it so a red run's short number can never be
		// read back as a legitimate calibration point.
		note := ""
		if r.Failed {
			note = "  [FAILED — not a calibration point]"
		}
		budget := budgetFor(r.ImportPath, budgets, def)
		fmt.Fprintf(w, "%8.1fs / %4ds  %s%s\n", r.Elapsed.Seconds(), int(budget.Seconds()), r.ImportPath, note)
	}
}

// violationLine names the package, the overage, and where the budget lives —
// everything the person whose build just went red needs, in one line.
func violationLine(v violation) string {
	over := v.Elapsed - v.Budget
	return fmt.Sprintf(
		"test runtime budget exceeded: %s took %.1fs, budget is %ds (over by %.1fs, +%.0f%%) — see tools/testbudget/budgets.go before raising the number",
		v.ImportPath, v.Elapsed.Seconds(), int(v.Budget.Seconds()),
		over.Seconds(), 100*float64(over)/float64(v.Budget))
}

// writeViolation reports one blown budget: the violation line, carrying the
// GitHub Actions annotation prefix, then the package's slowest top-level tests
// — where the time went, read from the run that failed rather than a
// reproduction on a machine of different speed. [LAW:nothing-unseen] Elapsed
// prints at go test's own two-decimal precision.
func writeViolation(w io.Writer, annotation string, v violation) {
	fmt.Fprintf(w, "%s%s\n", annotation, violationLine(v))
	fmt.Fprintf(w, "  slowest top-level tests, %d of %d:\n", len(v.Slowest), v.Ran)
	for _, t := range v.Slowest {
		mode := "serial"
		if t.Parallel {
			mode = "parallel"
		}
		mark := ""
		if t.Unfinished {
			mark = "  [unfinished]"
		}
		fmt.Fprintf(w, "  %8.2fs  %-8s  %s%s\n", t.Elapsed.Seconds(), mode, t.Name, mark)
	}
}

func main() {
	results, err := consume(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testbudget: %v\n", err)
		os.Exit(2)
	}
	printTable(os.Stdout, results, budgets, defaultBudget)
	viols := enforce(results, budgets, defaultBudget)
	// On GitHub Actions the same line doubles as an inline annotation; the
	// prefix is workflow-command syntax, not part of the message.
	prefix := ""
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		prefix = "::error::"
	}
	for _, v := range viols {
		writeViolation(os.Stderr, prefix, v)
	}
	if len(viols) > 0 {
		os.Exit(1)
	}
}
