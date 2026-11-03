package main

import "time"

// The per-package wall-clock budgets ci.yml enforces on every PR — the one
// home of the numbers. [LAW:one-source-of-truth]
//
// HOW THE NUMBERS ARE SET. Each budget is roughly 1.5x the package's worst
// measured environment, rounded up — far enough over the measurement to absorb
// machine variance, close enough that sustained creep or a single expensive
// test fails the build long before go test's 10-minute per-package default
// turns it into a timeout panic on an innocent diff — every package but
// internal/cli, whose note says why it has no such margin. The check reads whatever
// `go test -short -json ./...` run it is piped, and the enforcing one is CI's
// concurrent run on ubuntu-latest, whose 4-vCPU runners are drawn from a pool
// of different CPUs (AMD EPYC 7763, 9V45, 9V74; Intel Xeon 6973P/6975P) that
// differ by up to ~1.5x on one commit, on top of noisy-neighbor variance within
// one CPU model: commit 70feabb3 took internal/cli 262s on a Xeon 6975P
// and 378s on an EPYC 7763. So the worst environment is the slowest runner the
// pool hands out, not a median. Calibration record: the per-package maximum
// over 17 build-and-test runs on master and PR branches, 2026-11-02/03, quoted
// below as worst (CPU).
//
// RAISING A BUDGET IS NOT A FIX. Time comes back from cheaper fixtures, real
// parallelism, and moving benchmark-shaped work to the nightly lane — never by
// asserting less, and never by absorbing slowness into a bigger number. A
// budget moves only when the package's honest floor moves: a new test whose
// cost IS the behavior it pins (cite it here), or a suite-wide condition change
// (re-baseline every number from the first green run under the new
// conditions). The race detector is the suite-wide case that is declined: -race
// is a runtime regime, not a flag — on the inner loop it takes tools/licenses
// from 29s to 149s and internal/cli from 117s to 242s. Re-baselining to those
// figures would switch the alarm off, blind to a 3x regression in anything
// below the new floor. It runs in its own `race` job instead, so every number
// here still measures one territory: un-instrumented wall clock. A condition
// that inflates the whole table is a reason to ask whether the condition
// belongs in this lane at all. If you hit a budget while adding a test, the
// per-package notes below say where each package's time goes; make the test
// cheaper.
var budgets = map[string]time.Duration{
	// 291s (EPYC 9V74); 151-210s on the other CPUs. CI measured 58.5s at the
	// 2026-08-26 calibration, more than the runner pool's ~1.5x spread
	// explains, so where the rest went is unmeasured. Approaching this budget
	// means fixture tax is back.
	"github.com/promptctl/links-issue-tracker/internal/store": 440 * time.Second,

	// 385s (EPYC 7763); 249-358s on the other CPUs, most of it the serial e2e
	// set. 1.5x leaves almost no room under go test's 600s per-package
	// timeout, so this is the number with no slack left: its floor moves only
	// when the e2e tests can run in parallel (Run taking cwd and env as
	// parameters). Tighten it from the first runs after that lands.
	"github.com/promptctl/links-issue-tracker/internal/cli": 580 * time.Second,

	// 31s (EPYC 7763 and 9V74), and the ~18.5s isolated number is a floor,
	// not slack: the package's wall equals its one giant,
	// TestBurstOfMutationsNeverHitsEngineReadOnlyCollision, whose runtime is
	// the production mirror-contention it exists to reproduce.
	"github.com/promptctl/links-issue-tracker/cmd/lit": 50 * time.Second,

	// 37s (EPYC 7763); cold corpus load makes it the suite's noisiest
	// package. Five genuine buildEntries runs (~2s each) plus sub-second
	// tests; the content tests share one memoized inventory. The graph audit
	// is env-gated and does not run here.
	"github.com/promptctl/links-issue-tracker/tools/licenses": 60 * time.Second,

	// 27s (EPYC 9V74), over what the default's headroom absorbs.
	"github.com/promptctl/links-issue-tracker/tools/perfbench": 45 * time.Second,
}

// Every package not listed above — today all at or under ~15s on the slowest
// runner — gets this budget, so a new or newly slow package is caught
// without anyone remembering to enroll it. A package that legitimately
// outgrows the default graduates to an explicit entry with its rationale, not
// a bigger default.
const defaultBudget = 30 * time.Second
