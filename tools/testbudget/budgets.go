package main

import "time"

// The per-package wall-clock budgets ci.yml enforces on every PR — the one
// home of the numbers. [LAW:one-source-of-truth]
//
// HOW THE NUMBERS ARE SET. Each budget is calibrated from the measurement the
// check itself makes: the per-package table testbudget prints in the CI log of
// a green master run (packages running concurrently under `go test -short` on
// CI hardware — slower and noisier than any dev machine, which is why these
// numbers are well above local ones). Budgets sit far enough over the measured
// number to absorb runner variance, close enough that sustained creep or a
// single expensive test fails the build long before go test's 10-minute
// per-package default turns it into a timeout panic on an innocent diff.
// Calibration record: CI run of PR #422, 2026-08-26.
//
// RAISING A BUDGET IS NOT A FIX. The testperf epic (links-testperf-xxsx) got
// its seventeen minutes back from cheaper fixtures, real parallelism, and
// moving benchmark-shaped work to the nightly lane — never by asserting less,
// and never by absorbing slowness into a bigger number. A budget moves only
// when the package's honest floor moves: a new test whose cost IS the behavior
// it pins (cite it here), or a suite-wide condition change like
// links-testing-tt0c.3 putting `-race` on the inner loop (re-baseline every
// number from the first green run under the new conditions). If you hit a
// budget while adding a test, the epic's per-package notes below say where the
// headroom went; make the test cheaper the way the epic did.
var budgets = map[string]time.Duration{
	// Honest floor ~141s local isolated after xxsx.2/.3 (parallel tests +
	// migrated-template fixtures); remaining poles are two ~5s contention
	// tests. Anything approaching this budget means fixture tax is back.
	"github.com/promptctl/links-issue-tracker/internal/store": 300 * time.Second,

	// ~174s local, of which ~170s is the serial e2e set — the floor moves
	// only via links-testperf-xxsx.2.1 (Run taking cwd/env as parameters, so
	// the e2e tests can parallelize). Tighten this budget when 2.1 lands.
	"github.com/promptctl/links-issue-tracker/internal/cli": 400 * time.Second,

	// ~18.5s local and that is a floor, not slack: the package's wall equals
	// its one giant, TestBurstOfMutationsNeverHitsEngineReadOnlyCollision,
	// whose runtime is the production mirror-contention it exists to
	// reproduce (xxsx.5).
	"github.com/promptctl/links-issue-tracker/cmd/lit": 90 * time.Second,

	// ~6s local: five genuine buildEntries runs (~2s each) plus sub-second
	// tests; the content tests share one memoized inventory (xxsx.6). The
	// graph audit is env-gated and does not run here.
	"github.com/promptctl/links-issue-tracker/tools/licenses": 60 * time.Second,
}

// Every package not listed above — today all under ~12s local — gets this
// budget, so a new or newly slow package is caught without anyone remembering
// to enroll it. A package that legitimately outgrows the default graduates to
// an explicit entry with its rationale, not a bigger default.
const defaultBudget = 60 * time.Second
