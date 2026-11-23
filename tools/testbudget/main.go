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
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/promptctl/links-issue-tracker/tools/internal/gotestjson"
)

// slowestListed is how many of an over-budget package's top-level tests its
// violation names.
const slowestListed = 10

// packageResult and testResult are the shared reader's facts: testbudget
// times tests by the same clock as every other tool that reads the stream.
// [LAW:one-source-of-truth]
type (
	packageResult = gotestjson.Package
	testResult    = gotestjson.Test
)

type violation struct {
	ImportPath string
	Elapsed    time.Duration
	Budget     time.Duration
	Ran        int          // top-level tests the package ran
	Slowest    []testResult // its slowestListed slowest, slowest first
}

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
func consume(r io.Reader, w io.Writer) ([]packageResult, error) {
	s, err := gotestjson.Read(r, w)
	return s.Packages, err
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
// reproduction on a machine of different speed. [LAW:nothing-unseen] Each
// test's elapsed prints at go test's own two-decimal precision, as its
// `--- PASS` line does; the package's figures print at the table's one.
func writeViolation(w io.Writer, annotation string, v violation) {
	fmt.Fprintf(w, "%s%s\n", annotation, violationLine(v))
	fmt.Fprintf(w, "  slowest top-level tests, %d of %d:\n", len(v.Slowest), v.Ran)
	for _, t := range v.Slowest {
		mode := "serial"
		if t.Parallel {
			mode = "parallel"
		}
		mark := ""
		if t.Verdict == gotestjson.Unfinished {
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
