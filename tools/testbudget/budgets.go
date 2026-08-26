package main

import "time"

// The per-package wall-clock budgets ci.yml enforces on every PR — the one
// home of the numbers. [LAW:one-source-of-truth]
//
// HOW THE NUMBERS ARE SET. Each budget is roughly 1.5x the package's worst
// measured environment, rounded up — far enough over the measurement to absorb
// machine variance, close enough that sustained creep or a single expensive
// test fails the build long before go test's 10-minute per-package default
// turns it into a timeout panic on an innocent diff. Two environments matter
// because the check reads whatever `go test -short -json ./...` run it is
// piped: CI's concurrent run (the enforcing one — its table is in every CI
// log) and the dev machine's (the documented local invocation) — and at
// calibration the dev machine was the SLOWER of the two on every heavy
// package, so most numbers below are dev-bound. Calibration record: CI table
// from PR #422's run and the dev-machine full-suite measurement, both
// 2026-08-26, both quoted per package below as CI/dev.
//
// RAISING A BUDGET IS NOT A FIX. The testperf epic (links-testperf-xxsx) got
// its seventeen minutes back from cheaper fixtures, real parallelism, and
// moving benchmark-shaped work to the nightly lane — never by asserting less,
// and never by absorbing slowness into a bigger number. A budget moves only
// when the package's honest floor moves: a new test whose cost IS the behavior
// it pins (cite it here), or a suite-wide condition change (re-baseline every
// number from the first green run under the new conditions). The suite-wide
// case has been reached once and DECLINED, which is the precedent to reason
// from: links-testing-tt0c.3 brought the race detector into CI, and -race is a
// runtime regime, not a flag — on the inner loop it took tools/licenses from
// 29s to 149s and internal/cli from 117s to 242s. Re-baselining to those
// figures would have been the alarm switched off, blind to a 3x regression in
// anything below the new floor. It runs in its own `race` job instead, so
// every number here still measures one territory: un-instrumented wall clock.
// A condition that inflates the whole table is a reason to ask whether the
// condition belongs in this lane at all. If you hit a
// budget while adding a test, the epic's per-package notes below say where the
// headroom went; make the test cheaper the way the epic did.
var budgets = map[string]time.Duration{
	// 58.5s/95s (141s dev isolated, 2026-08-25). Honest floor after
	// xxsx.2/.3's parallel tests + migrated-template fixtures; remaining
	// poles are two ~5s contention tests. Approaching this budget means
	// fixture tax is back.
	"github.com/promptctl/links-issue-tracker/internal/store": 210 * time.Second,

	// 108s/220s (174s dev isolated), of which ~170s is the serial e2e set —
	// the floor moves only via links-testperf-xxsx.2.1 (Run taking cwd/env
	// as parameters, so the e2e tests can parallelize). Tighten this budget
	// when 2.1 lands.
	"github.com/promptctl/links-issue-tracker/internal/cli": 340 * time.Second,

	// 17.3s/31.5s, and the ~18.5s isolated number is a floor, not slack:
	// the package's wall equals its one giant,
	// TestBurstOfMutationsNeverHitsEngineReadOnlyCollision, whose runtime is
	// the production mirror-contention it exists to reproduce (xxsx.5).
	"github.com/promptctl/links-issue-tracker/cmd/lit": 50 * time.Second,

	// 18.2–30.2s across two same-day CI runs (cold corpus load makes it the
	// suite's noisiest package) / 7.2s dev — the one CI-bound entry, and
	// above every unlisted package in its worst environment, which is what
	// graduates it from the default: five genuine buildEntries runs (~2s
	// each) plus sub-second tests; the content tests share one memoized
	// inventory (xxsx.6). The graph audit is env-gated and does not run
	// here.
	"github.com/promptctl/links-issue-tracker/tools/licenses": 50 * time.Second,
}

// Every package not listed above — today all at or under ~12s in the slower
// environment — gets this budget, so a new or newly slow package is caught
// without anyone remembering to enroll it. A package that legitimately
// outgrows the default graduates to an explicit entry with its rationale, not
// a bigger default.
const defaultBudget = 30 * time.Second
