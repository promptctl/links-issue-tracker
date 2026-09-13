//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// lockedBuffer is a bytes.Buffer whose every access holds one mutex, so the
// test can read output while an abandoned in-process Run may still write it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMirrorHoldBudgetCutsHungPushAndReleasesEngine is the regression pin for
// links-sync-pgct.11.1: the background mirror's push shells out to git while
// this process holds the store's one read-write engine (and its journal lock),
// and before the fix NOTHING bounded that hold — a hung transport held the
// lock for as long as the remote cared to stall, starving every foreground
// command past its own open-retry budget (store.engineOpenRetryMaxElapsed —
// named rather than restated, since links-sync-dauk made it a derived value).
//
// The test reproduces the exact field topology with no network: a git shim on
// PATH wedges `git push` (the engine-side subprocess DOLT_PUSH spawns) in an
// indefinite sleep, and the mirror's hidden subcommand runs in-process against
// a seeded git+file remote with a real unpushed commit. The fix must
// (a) cut the whole cycle at store.MirrorHoldBudget — which requires the
// deadline to travel from the session-open ctx through the embedded driver's
// connection context into the git subprocess's kill — and (b) leave the engine
// released, proven by a foreground mutation succeeding immediately after.
// A wedge marker written by the shim pins that the push was genuinely mid-hang
// when the budget fired, so the pass cannot come from a push that
// short-circuited before touching git.
//
// Not parallel: it mutates PATH (t.Setenv) and store.MirrorHoldBudget.
func TestMirrorHoldBudgetCutsHungPushAndReleasesEngine(t *testing.T) {
	base, root, gitPath, runInProcess := setupMirrorHoldBudgetRepo(t)
	healthyCycle := measureHealthyMirrorCycle(t, runInProcess)
	wedgedBudget := shrinkMirrorHoldBudget(t, healthyCycle)
	wedgeMarker := installGitWedge(t, base, gitPath, "push")

	// The mirror's own entrypoint, in the foreground: parent-pid 0 skips the
	// parent wait, then the cycle opens the engine and pushes into the wedge.
	mirrorOut, mirrorErr := runInProcess(unboundedHoldTripwire(healthyCycle, wedgedBudget), "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	cycleEnded := time.Now()
	if mirrorErr != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", mirrorErr, mirrorOut.String())
	}
	assertHoldEndedWithinItsCeiling(t, wedgeEngagedAt(t, wedgeMarker, "push", mirrorOut), cycleEnded, wedgedBudget, mirrorOut)
	if !strings.Contains(mirrorOut.String(), "hold_budget_cut=true") {
		t.Fatalf("cycle log does not report the budget cut:\noutput:\n%s", mirrorOut.String())
	}

	// The engine (and journal lock) must be free: a foreground mutation opens
	// and commits without waiting out any residue of the wedged push.
	if out, err := runInProcess(30*time.Second, "new", "--title", "post-wedge probe", "--topic", "demo"); err != nil {
		t.Fatalf("foreground mutation after the cut mirror failed — the engine was not released: %v\noutput:\n%s", err, out.String())
	}

	// The cut left the durable record that makes the next field episode
	// attributable: a sync trace naming the hold budget.
	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	entries, err := os.ReadDir(syncTraceDir(ws))
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	// One trace owner per event: the budget explanation must arrive folded
	// into the push attempt's own record (the one carrying the push metadata),
	// never as a second stand-alone record beside it.
	found := false
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(syncTraceDir(ws), entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(content), "hold budget") && strings.Contains(string(content), "sync_branch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no push-attempt sync trace names the hold budget cut; the episode is as unattributable as the field incident")
	}
}

// TestMirrorHoldBudgetCutsHungResolveJoinsOneTrace pins the could-not-attempt
// arm of the budget cut, the arm a prior review round found double-recording:
// the session opens, but the pre-push remote resolution (git ls-remote) hangs
// and cycleCtx kills it, so performSyncPush records no trace of its own and
// mirrorCycle's out-of-band record is the event's one owner — the budget
// explanation joined onto the resolve failure. Exactly one record must carry
// the budget text, and it must be the join, not a push record.
//
// Not parallel: it mutates PATH (t.Setenv) and store.MirrorHoldBudget.
func TestMirrorHoldBudgetCutsHungResolveJoinsOneTrace(t *testing.T) {
	base, root, gitPath, runInProcess := setupMirrorHoldBudgetRepo(t)
	healthyCycle := measureHealthyMirrorCycle(t, runInProcess)
	wedgedBudget := shrinkMirrorHoldBudget(t, healthyCycle)
	wedgeMarker := installGitWedge(t, base, gitPath, "ls-remote")

	mirrorOut, mirrorErr := runInProcess(unboundedHoldTripwire(healthyCycle, wedgedBudget), "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	cycleEnded := time.Now()
	if mirrorErr != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", mirrorErr, mirrorOut.String())
	}
	assertHoldEndedWithinItsCeiling(t, wedgeEngagedAt(t, wedgeMarker, "resolve", mirrorOut), cycleEnded, wedgedBudget, mirrorOut)
	if !strings.Contains(mirrorOut.String(), "hold_budget_cut=true") {
		t.Fatalf("cycle log does not report the budget cut:\noutput:\n%s", mirrorOut.String())
	}

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	entries, err := os.ReadDir(syncTraceDir(ws))
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	var budgetRecords []string
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(syncTraceDir(ws), entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(content), "hold budget") {
			budgetRecords = append(budgetRecords, string(content))
		}
	}
	if len(budgetRecords) != 1 {
		t.Fatalf("want exactly one trace record owning the budget cut, got %d — the could-not-attempt arm is double- or under-recording", len(budgetRecords))
	}
	// The join, not a push record: the resolve failure rides in the same
	// record as the budget explanation, and no push metadata exists because
	// no push was attempted.
	if !strings.Contains(budgetRecords[0], "check remote refs") {
		t.Fatalf("the budget record does not carry the joined resolve failure:\n%s", budgetRecords[0])
	}
	if strings.Contains(budgetRecords[0], "sync_branch") {
		t.Fatalf("the budget record carries push metadata for a push that never ran:\n%s", budgetRecords[0])
	}
}

// setupMirrorHoldBudgetRepo seeds the shared field topology of the
// hold-budget regression tests: a workspace with a real git+file remote, lit
// initialized, the remote's dolt ref established with real git, and one real
// unpushed commit so a mirror cycle has work to reach the wedged subprocess.
// It chdirs into the workspace for the test's duration and returns an
// in-process Run wrapper whose timeout is the unbounded-hold tripwire.
func setupMirrorHoldBudgetRepo(t *testing.T) (base, root, gitPath string, runInProcess func(time.Duration, ...string) (*lockedBuffer, error)) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	base = t.TempDir()
	root = filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "hold-budget@test.co")
	runGit(t, root, "config", "user.name", "hold-budget")
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, root, "add", "-A")
	runGit(t, root, "commit", "-m", "seed")

	remoteGit := filepath.Join(base, "remote.git")
	runGit(t, base, "init", "--bare", "remote.git")
	runGit(t, root, "remote", "add", "origin", remoteGit)
	runGit(t, root, "push", "-u", "origin", "HEAD")

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(root) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	t.Setenv("HOME", base)
	t.Setenv("CODEX_HOME", filepath.Join(base, ".codex-home"))

	// The timeout branch reads the buffer while Run's goroutine may still be
	// writing it, so every access goes through one lock.
	runInProcess = func(timeout time.Duration, args ...string) (*lockedBuffer, error) {
		out := &lockedBuffer{}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		errCh := make(chan error, 1)
		go func() { errCh <- Run(ctx, out, out, args) }()
		select {
		case runErr := <-errCh:
			return out, runErr
		case <-ctx.Done():
			t.Fatalf("Run(%v) still blocked after %s — the hold is unbounded:\noutput:\n%s", args, timeout, out.String())
			return out, nil
		}
	}

	if out, err := runInProcess(60*time.Second, "init", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out.String())
	}
	// First dolt push establishes the remote's dolt ref and upstream, with real
	// git, so the wedged run is the mirror's ordinary incremental push.
	if out, err := runInProcess(60*time.Second, "sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out.String())
	}
	// A real unpushed commit, so the mirror's push has work and must reach the
	// wedged git subprocess rather than short-circuiting as up-to-date.
	if out, err := runInProcess(60*time.Second, "new", "--title", "hold-budget probe", "--topic", "demo"); err != nil {
		t.Fatalf("lit new (pre-wedge): %v\noutput:\n%s", err, out.String())
	}
	return base, root, gitPath, runInProcess
}

// installGitWedge prepends a PATH shim wedging the named git subcommand in an
// indefinite sleep — every other git call passes through to the real binary —
// and stamps a marker first, so a test can prove the budget cut a genuinely
// hung subprocess rather than a call that never ran. Returns the marker path.
func installGitWedge(t *testing.T, base, gitPath, subcommand string) (wedgeMarker string) {
	t.Helper()
	wedgeMarker = filepath.Join(base, "wedge-engaged")
	shimDir := filepath.Join(base, "git-shim")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	shim := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in
    %s)
      : > %q
      exec /bin/sleep 600
      ;;
  esac
done
exec %q "$@"
`, subcommand, wedgeMarker, gitPath)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return wedgeMarker
}

// shrinkMirrorHoldBudget installs the wedged run's hold budget for the test's
// duration — margin over the healthy cycle measured on this machine — so a
// wedged cycle cuts in seconds, and restores the production figure at cleanup.
//
// It is the one home of that derivation, and handing the installed budget back
// is what keeps it one: the tripwire and the ceiling below take the budget as
// data, so neither reads it back out of a global it would then have to be
// called after. The ordering is a data dependency rather than a line of prose
// asking for it. [LAW:one-source-of-truth] [LAW:no-ambient-temporal-coupling]
func shrinkMirrorHoldBudget(t *testing.T, healthyCycle time.Duration) time.Duration {
	t.Helper()
	budget := wedgeHoldBudgetMargin * healthyCycle
	restore := store.MirrorHoldBudget
	store.MirrorHoldBudget = budget
	t.Cleanup(func() { store.MirrorHoldBudget = restore })
	t.Logf("healthy mirror cycle measured at %s; wedged run budgeted at %s",
		healthyCycle.Round(time.Millisecond), budget.Round(time.Millisecond))
	return budget
}

// wedgeHoldBudgetMargin is how far above a measured healthy cycle the shrunk
// budget sits, and why these tests no longer name a duration at all.
//
// A hold budget is a stall detector: its whole claim is that work still running
// at the deadline has stopped making progress, and that claim is false the
// moment the deadline lands inside the cost of healthy work — the defect
// links-sync-dauk filed against the production figure. The shrink here used to
// be a flat 8s, which made the same mistake one level down: 8s was a wall-clock
// bet against setup this test does not own. Measured at load average 270 on the
// dev box, a cycle spent more than 8s between opening its engine and spawning
// git push, so the budget fired before the wedge could engage and the run
// reported the invariant broken when it had only failed to reach it. Raising
// the number would have moved that threshold without removing the bet
// (links-testperf-6vfg).
//
// So the base is measured rather than named, and this is the margin over it.
// One whole healthy cycle is already a strict over-estimate of what has to fit
// under the budget — it includes a completed push and the close, while only the
// work BEFORE the wedge engages must fit — and the factor covers load moving
// between the sample and the wedged run. It is deliberately this test's own
// number and not store's mirrorHoldStallFactor, which is margin over a recorded
// tail rather than over a single sample; sharing the value would claim a
// derivation that is not there. [LAW:no-ambient-temporal-coupling]
const wedgeHoldBudgetMargin = 2

// measureHealthyMirrorCycle runs one unwedged mirror cycle and reports what a
// healthy cycle costs on THIS machine right now — the figure the wedged run's
// budget and tripwire are both derived from. It leaves behind the unpushed
// commit the timed cycle consumed, so it hands back the fixture it was given.
//
// It must run BEFORE installGitWedge (a cycle timed through the shim would be
// timing the wedge). Running before the shrink needs no saying: the shrink
// takes this function's result. A sample that was itself cut is censored — the
// budget standing in for the work, which is the very reading error
// links-sync-dauk had to correct in the field log — so a cut sample is a
// failure here rather than a smaller number. [LAW:no-silent-failure]
func measureHealthyMirrorCycle(t *testing.T, runInProcess func(time.Duration, ...string) (*lockedBuffer, error)) time.Duration {
	t.Helper()
	started := time.Now()
	out, err := runInProcess(90*time.Second, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	healthyCycle := time.Since(started)
	if err != nil {
		t.Fatalf("unwedged mirror cycle failed, so there is no healthy cost to size the wedged run against: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "attempted=true hold_budget_cut=false") {
		t.Fatalf("the sample cycle did not complete a push under the production budget, so its cost is not the cost of a healthy cycle:\noutput:\n%s", out.String())
	}
	if probe, probeErr := runInProcess(90*time.Second, "new", "--title", "post-measurement probe", "--topic", "demo"); probeErr != nil {
		t.Fatalf("re-seed the unpushed commit the sample cycle pushed: %v\noutput:\n%s", probeErr, probe.String())
	}
	return healthyCycle
}

// unboundedHoldTripwire is when a wedged mirror run is hung rather than
// working: a whole healthy cycle of work ahead of the wedge, the budget in
// force for this run, and the lag the cut takes to unwind — then the same
// margin again, because a tripwire that lands on legal work reports the wrong
// failure, which is the mistake this ticket is about.
func unboundedHoldTripwire(healthyCycle, wedgedBudget time.Duration) time.Duration {
	return wedgeHoldBudgetMargin * (healthyCycle + wedgedBudget + store.MirrorCancelLagObserved)
}

// wedgeEngagedAt is the vacuous-pass guard and the hold's clock in one. The
// shim stamps its marker before it sleeps, so the marker exists only if the
// wedged git subcommand genuinely ran, and its timestamp is the instant the
// hang began. A run whose budget fired first proves nothing and says so.
func wedgeEngagedAt(t *testing.T, wedgeMarker, wedged string, out *lockedBuffer) time.Time {
	t.Helper()
	marker, err := os.Stat(wedgeMarker)
	if err != nil {
		t.Fatalf("the wedge never engaged (marker missing: %v) — the %s short-circuited and the test proved nothing:\noutput:\n%s", err, wedged, out.String())
	}
	return marker.ModTime()
}

// assertHoldEndedWithinItsCeiling pins where the hold ends, measured from the
// wedge and not from the cycle's start: everything before the wedge engaged is
// work whose cost this test does not own, and folding it into the bound is the
// same wall-clock bet that made the budget itself flaky.
//
// The bound is the ceiling and not the budget, which is why the name says so:
// the budget is when the cut BEGINS, and the hold outlives it by the lag
// cancellation takes to unwind — store's own mirrorHoldCeiling relation. Both
// terms are the system's own, the budget this run installed and store's
// measured lag, rather than the bare 30s the two tests used to restate.
// [LAW:one-source-of-truth]
func assertHoldEndedWithinItsCeiling(t *testing.T, wedgedAt, cycleEnded time.Time, wedgedBudget time.Duration, out *lockedBuffer) {
	t.Helper()
	ceiling := wedgedBudget + store.MirrorCancelLagObserved
	if held := cycleEnded.Sub(wedgedAt); held > ceiling {
		t.Fatalf("the mirror held its session %s past the wedge (budget %s, ceiling %s) — the hold bound is not working:\noutput:\n%s",
			held.Round(time.Millisecond), wedgedBudget, ceiling, out.String())
	}
}

// TestHoldBudgetCutFramingSurvivesTheBanner pins the claim
// holdBudgetCutExplanation's comment makes about its own word order: the
// FAILING banner renders this message through oneLineReason, which keeps the
// first line and caps it at 160 runes, so the part that stops a reader
// blaming the network has to survive that cut.
//
// links-sync-dauk's first wording left five runes of margin and nothing
// measured it, which is the same shape as the defect the ticket was about — an
// invariant asserted in a comment and enforced nowhere. A later reword, or a
// MirrorHoldBudget whose Duration formats longer than "40s", would have
// truncated the framing away silently and left the banner saying only that a
// budget was exceeded.
//
// The budget cases are the enumeration this needs: the production value, and a
// value in minutes, which Go renders as "1h40m0s" — more than twice the runes.
// Not parallel: it mutates store.MirrorHoldBudget.
func TestHoldBudgetCutFramingSurvivesTheBanner(t *testing.T) {
	for _, budget := range []time.Duration{40 * time.Second, 100 * time.Minute} {
		func() {
			restore := store.MirrorHoldBudget
			store.MirrorHoldBudget = budget
			defer func() { store.MirrorHoldBudget = restore }()

			banner := oneLineReason(holdBudgetCutExplanation().Error())
			for _, want := range []string{
				budget.String(),
				"a deadline, not a diagnosis",
				"mirror.log",
			} {
				if !strings.Contains(banner, want) {
					t.Fatalf("with MirrorHoldBudget=%s the banner dropped %q — the 160-rune cut landed before the framing, so the one line a reader sees says a budget was exceeded and nothing about how to tell a slow remote from an undersized budget.\nbanner: %s",
						budget, want, banner)
				}
			}
			if strings.HasSuffix(banner, "…") && !strings.Contains(banner, "before blaming the remote") {
				t.Fatalf("with MirrorHoldBudget=%s the banner truncated mid-framing: %s", budget, banner)
			}
		}()
	}
}
