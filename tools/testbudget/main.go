// testbudget enforces the per-package wall-clock budgets in budgets.go against
// a `go test -json` stream on stdin. It exists so the suite's runtime is an
// observable that fails the build out loud instead of a number nobody watches:
// the testperf epic (links-testperf-xxsx) paid down seventeen minutes of
// accumulated test time that arrived one unwatched second at a time, and whose
// only signal was a per-package timeout panic indicting an innocent diff.
// [LAW:no-silent-failure] the budget IS the alarm — a package over budget fails
// CI naming the package and the overage, never a warning.
//
// It is invoked by ci.yml's Test step as the consumer of the gating run itself:
//
//	go test -short -json ./... | go run ./tools/testbudget
//
// so enforcement adds no CI time and measures the exact conditions it gates
// (the concurrent -short run on CI hardware). The tool replays go test's
// human-readable contract while it reads — package result lines as they
// arrive, a test's accumulated output only when that test fails — then prints
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
	"sort"
	"time"
)

// event is the subset of test2json's output this tool consumes (see
// $GOROOT/src/cmd/internal/test2json). A nil Elapsed means the event carries
// no duration, not a zero-second one.
type event struct {
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
}

// packageResult is the stamped fact a package's terminal event proves: this
// import path finished, took this long, and passed or failed.
// [LAW:parse-dont-validate] everything downstream (the table, enforcement)
// works from these, never from raw stream lines.
type packageResult struct {
	ImportPath string
	Elapsed    time.Duration
	Failed     bool
}

type violation struct {
	ImportPath string
	Elapsed    time.Duration
	Budget     time.Duration
}

// consume reads one `go test -json` stream from r, replaying human-readable
// output to w, and returns each package's terminal result in stream order.
// Lines that are not JSON events (a panic trace, stray build noise) are echoed
// to w verbatim — surfaced, never swallowed. [LAW:no-silent-failure]
func consume(r io.Reader, w io.Writer) ([]packageResult, error) {
	// Output for a still-running test, held until its verdict: failed tests
	// replay it, passing tests discard it — go test's non-verbose contract.
	type testKey struct{ pkg, test string }
	buffered := make(map[testKey][]string)

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
		switch {
		case ev.Action == "output" && ev.Test != "":
			k := testKey{ev.Package, ev.Test}
			buffered[k] = append(buffered[k], ev.Output)
		case ev.Action == "output" || ev.Action == "build-output":
			io.WriteString(w, ev.Output)
		case ev.Test != "" && ev.Action == "fail":
			k := testKey{ev.Package, ev.Test}
			for _, s := range buffered[k] {
				io.WriteString(w, s)
			}
			delete(buffered, k)
		case ev.Test != "" && (ev.Action == "pass" || ev.Action == "skip"):
			delete(buffered, testKey{ev.Package, ev.Test})
		case ev.Test == "" && (ev.Action == "pass" || ev.Action == "fail" || ev.Action == "skip"):
			var elapsed time.Duration
			if ev.Elapsed != nil {
				elapsed = time.Duration(*ev.Elapsed * float64(time.Second))
			}
			results = append(results, packageResult{
				ImportPath: ev.Package,
				Elapsed:    elapsed,
				Failed:     ev.Action == "fail",
			})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading go test -json stream: %w", err)
	}
	return results, nil
}

// enforce checks every result against its budget — a listed number or the
// default, so no package is ever exempt and a new slow package cannot creep in
// unnamed. [LAW:dataflow-not-control-flow] the check always runs; only the
// budget value varies.
func enforce(results []packageResult, budgets map[string]time.Duration, def time.Duration) []violation {
	var viols []violation
	for _, r := range results {
		budget, listed := budgets[r.ImportPath]
		if !listed {
			budget = def
		}
		if r.Elapsed > budget {
			viols = append(viols, violation{r.ImportPath, r.Elapsed, budget})
		}
	}
	return viols
}

// printTable renders every package's elapsed against its budget, slowest
// first. It prints on success too: each CI log is the calibration record the
// next re-baseline of budgets.go reads its numbers from.
func printTable(w io.Writer, results []packageResult, budgets map[string]time.Duration, def time.Duration) {
	sorted := append([]packageResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Elapsed > sorted[j].Elapsed })
	fmt.Fprintf(w, "\ntestbudget: per-package wall clock vs budget (budgets: tools/testbudget/budgets.go)\n")
	for _, r := range sorted {
		budget, listed := budgets[r.ImportPath]
		if !listed {
			budget = def
		}
		fmt.Fprintf(w, "%8.1fs / %4ds  %s\n", r.Elapsed.Seconds(), int(budget.Seconds()), r.ImportPath)
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
		fmt.Fprintf(os.Stderr, "%s%s\n", prefix, violationLine(v))
	}
	if len(viols) > 0 {
		os.Exit(1)
	}
}
