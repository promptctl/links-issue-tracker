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

// TestMirrorPushHoldsNothingOnTheLiveStore is the contract in test form: the
// background mirror's push runs from a clone, so while the push is mid-flight —
// wedged, here, in a git shim that never returns — a foreground mutation on the
// live store opens, commits and returns. A mirror pushing under the live
// store's one read-write engine (and its journal lock) would make the same
// `lit new` wait out the whole hold.
//
// The same run pins the deadline: the wedged push is cut at
// store.MirrorPushDeadline, the cycle log says so, and the attempt's own trace
// record names the deadline — the single-flight mirror must not sit on a
// stalled transport forever, or every mirror spawned meanwhile loses the race
// and pushes stop silently.
//
// The test reproduces the exact field topology with no network: a git shim on
// PATH wedges `git push` (the engine-side subprocess DOLT_PUSH spawns) in an
// indefinite sleep, and the mirror's hidden subcommand runs in-process against
// a seeded git+file remote with a real unpushed commit. A wedge marker written
// by the shim pins that the push was genuinely mid-hang when the mutation ran,
// so the pass cannot come from a push that short-circuited before touching git.
func TestMirrorPushHoldsNothingOnTheLiveStore(t *testing.T) {
	t.Parallel()
	rig := setupMirrorDeadlineRepo(t)
	healthyCycle := measureHealthyMirrorCycle(t, rig)
	wedgedDeadline := shrinkMirrorPushDeadline(t, rig, healthyCycle)
	wedgeMarker := installGitWedge(t, rig, "push")

	// The mirror's own entrypoint, in the foreground of its own goroutine:
	// parent-pid 0 skips the parent wait, then the cycle clones, releases, and
	// pushes into the wedge.
	mirror := rig.start(unboundedRunTripwire(healthyCycle, wedgedDeadline), workerArgv(t, rig.root, backgroundMirrorSubcommand)...)
	wedgedAt := awaitWedge(t, wedgeMarker, unboundedRunTripwire(healthyCycle, wedgedDeadline), mirror)

	// The proof: with the push genuinely mid-hang, a foreground mutation on
	// the live store completes. Its own timeout is the tripwire — a `lit new`
	// that waits out the wedged push is the defect, and it fails here instead
	// of passing after the cut.
	mutationStart := time.Now()
	if out, err := rig.run(wedgedDeadline, "new", "--type", "epic", "--title", "mid-push probe", "--topic", "demo"); err != nil {
		t.Fatalf("foreground mutation during the wedged push failed — the live store is still held across the push: %v\noutput:\n%s", err, out.String())
	}
	mutationElapsed := time.Since(mutationStart)
	select {
	case res := <-mirror.done:
		t.Fatalf("the mirror had already finished when the mutation returned (%s), so nothing was proven about a push in flight:\noutput:\n%s", mutationElapsed, res.out.String())
	default:
	}

	res := mirror.wait(t)
	cycleEnded := time.Now()
	if res.err != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", res.err, res.out.String())
	}
	assertPushEndedWithinItsCeiling(t, wedgedAt, cycleEnded, wedgedDeadline, res.out)
	if !strings.Contains(res.out.String(), "push_deadline_cut=true") {
		t.Fatalf("cycle log does not report the deadline cut:\noutput:\n%s", res.out.String())
	}
	// The clone the push ran from is gone with the cycle — success or cut.
	ws, err := workspace.Resolve(runEnv(t), rig.root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	assertNoMirrorClone(t, ws)

	// The cut left the durable record that makes the next field episode
	// attributable: a sync trace naming the push deadline, folded into the
	// push attempt's own record (the one carrying the push metadata), never as
	// a second stand-alone record beside it.
	entries, err := os.ReadDir(syncTraceDir(ws))
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	found := false
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(syncTraceDir(ws), entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(content), "deadline") && strings.Contains(string(content), "sync_branch") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no push-attempt sync trace names the deadline cut; the episode is as unattributable as the field incident")
	}
	assertOutcomeRecordsTheCut(t, ws)
}

// assertOutcomeRecordsTheCut pins that the push-outcome record reads the same
// verdict the cycle log and the trace did: the deadline cut the attempt, and
// its reason says so.
func assertOutcomeRecordsTheCut(t *testing.T, ws workspace.Info) {
	t.Helper()
	rec, _, ok := lastPushOutcome(testStderr(t), ws, time.Now())
	if !ok {
		t.Fatalf("no push-outcome record after a cut attempt")
	}
	if !rec.PushDeadlineCut || !strings.Contains(rec.Reason, "before blaming the remote") {
		t.Fatalf("push-outcome record = %+v; want push_deadline_cut and a reason naming the push deadline", rec)
	}
}

// TestMirrorPushDeadlineCutsHungResolveJoinsOneTrace pins the could-not-attempt
// arm of the deadline cut: the clone's session opens, but the pre-push remote
// resolution (git ls-remote) hangs and the push deadline kills it, so
// performSyncPush records no trace of its own and mirrorOnce's out-of-band
// record is the event's one owner — the deadline explanation joined onto the
// resolve failure where the resolve returned. Exactly one record must carry
// the deadline text, it must be the join, not a push record, and the
// push-outcome record must read the same verdict.
func TestMirrorPushDeadlineCutsHungResolveJoinsOneTrace(t *testing.T) {
	t.Parallel()
	rig := setupMirrorDeadlineRepo(t)
	healthyCycle := measureHealthyMirrorCycle(t, rig)
	wedgedDeadline := shrinkMirrorPushDeadline(t, rig, healthyCycle)
	wedgeMarker := installGitWedge(t, rig, "ls-remote")

	mirrorOut, mirrorErr := rig.run(unboundedRunTripwire(healthyCycle, wedgedDeadline), workerArgv(t, rig.root, backgroundMirrorSubcommand)...)
	cycleEnded := time.Now()
	if mirrorErr != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", mirrorErr, mirrorOut.String())
	}
	assertPushEndedWithinItsCeiling(t, wedgeEngagedAt(t, wedgeMarker, "resolve", mirrorOut), cycleEnded, wedgedDeadline, mirrorOut)
	if !strings.Contains(mirrorOut.String(), "push_deadline_cut=true") {
		t.Fatalf("cycle log does not report the deadline cut:\noutput:\n%s", mirrorOut.String())
	}

	ws, err := workspace.Resolve(runEnv(t), rig.root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	assertNoMirrorClone(t, ws)
	entries, err := os.ReadDir(syncTraceDir(ws))
	if err != nil {
		t.Fatalf("read sync trace dir: %v", err)
	}
	var deadlineRecords []string
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(syncTraceDir(ws), entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(content), "deadline") {
			deadlineRecords = append(deadlineRecords, string(content))
		}
	}
	if len(deadlineRecords) != 1 {
		t.Fatalf("want exactly one trace record owning the deadline cut, got %d — the could-not-attempt arm is double- or under-recording", len(deadlineRecords))
	}
	// The join, not a push record: the resolve failure rides in the same
	// record as the deadline explanation, and no push metadata exists because
	// no push was attempted.
	if !strings.Contains(deadlineRecords[0], "check remote refs") {
		t.Fatalf("the deadline record does not carry the joined resolve failure:\n%s", deadlineRecords[0])
	}
	if strings.Contains(deadlineRecords[0], "sync_branch") {
		t.Fatalf("the deadline record carries push metadata for a push that never ran:\n%s", deadlineRecords[0])
	}
	assertOutcomeRecordsTheCut(t, ws)
}

// TestMirrorPushDeadlineCutsTheOpen pins the open arm of the deadline cut: the
// clone's session opens under the push deadline, so a deadline already spent
// when the open begins ends the attempt there, before any transport. The
// cycle log, the push-outcome record and its reason all read the one verdict.
// The deadline is spent before the open starts, so the cut is certain, not a
// race against the open's cost.
func TestMirrorPushDeadlineCutsTheOpen(t *testing.T) {
	t.Parallel()
	rig := setupMirrorDeadlineRepo(t)
	rig.pushDeadline = time.Nanosecond

	out, err := rig.run(unboundedRunTripwire(0, store.MirrorPushDeadline), workerArgv(t, rig.root, backgroundMirrorSubcommand)...)
	if err != nil {
		t.Fatalf("mirror run returned error (the mirror is best-effort and must exit 0): %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "attempted=false push_deadline_cut=true") {
		t.Fatalf("cycle log does not report the open the deadline cut:\noutput:\n%s", out.String())
	}
	ws, err := workspace.Resolve(runEnv(t), rig.root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	assertNoMirrorClone(t, ws)
	assertOutcomeRecordsTheCut(t, ws)
	rec, _, _ := lastPushOutcome(testStderr(t), ws, time.Now())
	if !strings.Contains(rec.Reason, "open sync store on the clone") {
		t.Fatalf("push-outcome reason %q does not name the open the deadline cut", rec.Reason)
	}
}

// TestMirrorCycleSweepsADeadMirrorsClone pins the residue contract: a clone a
// crashed mirror left behind is collected by the next cycle before it takes
// its own, and nothing of either survives the cycle — the next mirror trips on
// nothing. It also pins the healthy cycle's log shape, which is the record a
// field check reads: each hold on the live store reports its own
// elapsed time, and the cycle end carries the phases apart.
func TestMirrorCycleSweepsADeadMirrorsClone(t *testing.T) {
	t.Parallel()
	rig := setupMirrorDeadlineRepo(t)
	ws, err := workspace.Resolve(runEnv(t), rig.root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	corpse := filepath.Join(mirrorCloneBase(ws), "1", "dolt")
	if err := os.MkdirAll(corpse, 0o755); err != nil {
		t.Fatalf("plant a dead mirror's clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corpse, "residue"), []byte("x"), 0o644); err != nil {
		t.Fatalf("plant residue: %v", err)
	}

	out, err := rig.run(unboundedRunTripwire(0, store.MirrorPushDeadline), workerArgv(t, rig.root, backgroundMirrorSubcommand)...)
	if err != nil {
		t.Fatalf("mirror cycle: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "attempted=true push_deadline_cut=false") {
		t.Fatalf("the cycle did not push healthily:\noutput:\n%s", out.String())
	}
	assertNoMirrorClone(t, ws)
	for _, want := range []string{
		"mirror hold released step=clone elapsed=",
		"mirror hold released step=record elapsed=",
		" hold=", " record=", " open=", " push=", " elapsed=",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("cycle log lacks %q — the field check reads the holds' elapsed= off this log:\noutput:\n%s", want, out.String())
		}
	}
	// open= is the clone engine's open and push= the whole span that holds
	// it and the push after it, so the open is a real share of push= and a
	// strictly smaller one: the push to the remote is the rest.
	open, push := cycleEndDuration(t, out.String(), "open"), cycleEndDuration(t, out.String(), "push")
	if open <= 0 || open >= push {
		t.Fatalf("open=%s is not a nonzero share of push=%s:\noutput:\n%s", open, push, out.String())
	}
}

// cycleEndDuration reads one duration field off the log's cycle-end line.
func cycleEndDuration(t *testing.T, log, field string) time.Duration {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "mirror cycle end ") {
			continue
		}
		for _, tok := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(tok, field+"="); ok {
				d, err := time.ParseDuration(value)
				if err != nil {
					t.Fatalf("cycle end %s=%q is not a duration: %v", field, value, err)
				}
				return d
			}
		}
	}
	t.Fatalf("no cycle-end line carries %s=:\n%s", field, log)
	return 0
}

// assertNoMirrorClone pins that no clone survives a cycle: the base directory
// is absent or holds nothing.
func assertNoMirrorClone(t *testing.T, ws workspace.Info) {
	t.Helper()
	entries, err := os.ReadDir(mirrorCloneBase(ws))
	if err != nil && os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read mirror clone base: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a mirror clone survived the cycle: %v", names)
	}
}

// mirrorRun is a run started on its own goroutine, so a test can act on the
// live store while the mirror's push is in flight.
type mirrorRun struct {
	done chan mirrorResult
}

type mirrorResult struct {
	out *lockedBuffer
	err error
}

// wait blocks for the run's result; the run's own tripwire is what bounds it.
func (r mirrorRun) wait(t *testing.T) mirrorResult {
	t.Helper()
	return <-r.done
}

// mirrorRig is the field topology of the mirror deadline tests and the two
// knobs a test turns on the mirror runs it starts: the push deadline they run
// under and the git shim they resolve git through.
//
// The mirror runs as a child of the test binary (TestMain's worker branch),
// the shape production gives it, because both knobs are process-level in the
// mirror: Dolt resolves git through the process PATH, and the push deadline is
// store.MirrorPushDeadline. Set in a child, neither reaches any other test.
// Every other command runs in-process, in root, with the test's environment.
type mirrorRig struct {
	t                   *testing.T
	base, root, gitPath string
	// pushDeadline is the mirror runs' push deadline; zero runs them under
	// the production one.
	pushDeadline time.Duration
	// gitShim is a directory put ahead of PATH for the mirror runs; empty
	// puts nothing there.
	gitShim string
}

// start starts args on its own goroutine under timeout, the unbounded-run
// tripwire: a run still going at timeout returns an error that says so.
func (r *mirrorRig) start(timeout time.Duration, args ...string) mirrorRun {
	out := &lockedBuffer{}
	done := make(chan mirrorResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := r.runUnder(ctx, out, args)
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("Run(%v) still blocked after %s — the run is unbounded", args, timeout)
		}
		done <- mirrorResult{out: out, err: err}
	}()
	return mirrorRun{done: done}
}

// run is start's result, failing the test on a run the tripwire cut.
func (r *mirrorRig) run(timeout time.Duration, args ...string) (*lockedBuffer, error) {
	res := <-r.start(timeout, args...).done
	if res.err != nil && strings.Contains(res.err.Error(), "the run is unbounded") {
		r.t.Fatalf("%v:\noutput:\n%s", res.err, res.out.String())
	}
	return res.out, res.err
}

func (r *mirrorRig) runUnder(ctx context.Context, out *lockedBuffer, args []string) error {
	if len(args) == 0 || args[0] != backgroundMirrorSubcommand {
		errCh := make(chan error, 1)
		go func() { errCh <- Run(ctx, dirInvocation(r.t, r.root, out, out, args)) }()
		select {
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the test binary: %w", err)
	}
	env := runEnv(r.t)
	if r.gitShim != "" {
		env = env.With("PATH=" + r.gitShim + string(os.PathListSeparator) + env.Get("PATH"))
	}
	if r.pushDeadline != 0 {
		env = env.With(testMirrorPushDeadlineEnvVar + "=" + r.pushDeadline.String())
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir, cmd.Env = r.root, env.List()
	cmd.Stdin = strings.NewReader(workerHandoffStdin(r.t, r.root))
	cmd.Stdout, cmd.Stderr = out, out
	// A descendant that outlives the kill holds the output pipes open; without
	// a bound on that, Run never returns and the tripwire never fires.
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// setupMirrorDeadlineRepo seeds the shared field topology of the mirror
// deadline tests: a workspace with a real git+file remote, lit initialized,
// the remote's dolt ref established with real git, and one real unpushed
// commit so a mirror cycle has work to reach the wedged subprocess.
func setupMirrorDeadlineRepo(t *testing.T) *mirrorRig {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	base := t.TempDir()
	root := filepath.Join(base, "workspace")
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
	setRunEnv(t, "HOME", base)

	rig := &mirrorRig{t: t, base: base, root: root, gitPath: gitPath}
	if out, err := rig.run(60*time.Second, "init", "--skip-hooks"); err != nil {
		t.Fatalf("lit init: %v\noutput:\n%s", err, out.String())
	}
	// First dolt push establishes the remote's dolt ref and upstream, with real
	// git, so the wedged run is the mirror's ordinary incremental push.
	if out, err := rig.run(60*time.Second, "sync", "push", "--set-upstream"); err != nil {
		t.Fatalf("bootstrap lit sync push: %v\noutput:\n%s", err, out.String())
	}
	// A real unpushed commit, so the mirror's push has work and must reach the
	// wedged git subprocess rather than short-circuiting as up-to-date.
	if out, err := rig.run(60*time.Second, "new", "--type", "epic", "--title", "hold-budget probe", "--topic", "demo"); err != nil {
		t.Fatalf("lit new (pre-wedge): %v\noutput:\n%s", err, out.String())
	}
	return rig
}

// installGitWedge gives the rig's mirror runs a git shim wedging the named git
// subcommand in an indefinite sleep — every other git call passes through to
// the real binary — and stamps a marker first, so a test can prove the deadline
// cut a genuinely hung subprocess rather than a call that never ran. Returns
// the marker path.
func installGitWedge(t *testing.T, rig *mirrorRig, subcommand string) (wedgeMarker string) {
	t.Helper()
	wedgeMarker = filepath.Join(rig.base, "wedge-engaged")
	shimDir := filepath.Join(rig.base, "git-shim")
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
`, subcommand, wedgeMarker, rig.gitPath)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	rig.gitShim = shimDir
	return wedgeMarker
}

// shrinkMirrorPushDeadline sets the rig's mirror runs' push deadline to the
// wedged run's — margin over the healthy cycle measured on this machine — so a
// wedged push cuts in seconds.
//
// It is the one home of that derivation, and handing the deadline back is
// what keeps it one: the tripwire and the ceiling below take the deadline as
// data, so neither reads it back out of the rig it would then have to be
// called after. The ordering is a data dependency rather than a line of prose
// asking for it. [LAW:one-source-of-truth] [LAW:no-ambient-temporal-coupling]
func shrinkMirrorPushDeadline(t *testing.T, rig *mirrorRig, healthyCycle time.Duration) time.Duration {
	t.Helper()
	deadline := wedgeDeadlineMargin * healthyCycle
	rig.pushDeadline = deadline
	t.Logf("healthy mirror cycle measured at %s; wedged run's push deadline %s",
		healthyCycle.Round(time.Millisecond), deadline.Round(time.Millisecond))
	return deadline
}

// wedgeDeadlineMargin is how far above a measured healthy cycle the shrunk
// deadline sits, and why these tests name no duration at all.
//
// A deadline is a stall detector: its whole claim is that work still running
// at the deadline has stopped making progress, and that claim is false the
// moment the deadline lands inside the cost of healthy work. A flat shrink
// makes the same mistake one level down: it is a wall-clock bet against setup
// this test does not own. Under heavy load a cycle can spend longer than any
// named figure between opening its engine and spawning git push, so the budget
// fires before the wedge can engage and the run reports the invariant broken
// when it has only failed to reach it. Raising the number moves that threshold
// without removing the bet.
//
// So the base is measured rather than named, and this is the margin over it.
// One whole healthy cycle is already a strict over-estimate of what has to fit
// under the deadline — it includes a completed push and the close, while only
// the work BEFORE the wedge engages must fit — and the factor covers load
// moving between the sample and the wedged run. It is deliberately this test's
// own number and not store's mirrorPushStallFactor, which is margin over a
// recorded tail rather than over a single sample; sharing the value would claim
// a derivation that is not there. [LAW:no-ambient-temporal-coupling]
const wedgeDeadlineMargin = 2

// measureHealthyMirrorCycle runs one unwedged mirror cycle and reports what a
// healthy cycle costs on THIS machine right now — the figure the wedged run's
// deadline and tripwire are both derived from. It leaves behind the unpushed
// commit the timed cycle consumed, so it hands back the fixture it was given.
//
// It must run BEFORE installGitWedge (a cycle timed through the shim would be
// timing the wedge). Running before the shrink needs no saying: the shrink
// takes this function's result. A sample that was itself cut is censored — the
// deadline standing in for the work — so a cut sample is a failure here rather
// than a smaller number. So is a sample whose push failed fast: the mirror is
// best-effort and exits clean either way, and a rejected push is cheaper than
// a landed one. Whether the push landed is read from the
// push-outcome marker, the one record of that fact, and the marker must be
// this sample's rather than the bootstrap push's. [LAW:no-silent-failure]
// [LAW:one-source-of-truth]
func measureHealthyMirrorCycle(t *testing.T, rig *mirrorRig) time.Duration {
	t.Helper()
	started := time.Now()
	// The sample runs under the production deadline, so its hung bound is that
	// deadline's, not a figure of this test's choosing. [LAW:one-source-of-truth]
	out, err := rig.run(unboundedRunTripwire(0, store.MirrorPushDeadline), workerArgv(t, rig.root, backgroundMirrorSubcommand)...)
	healthyCycle := time.Since(started)
	if err != nil {
		t.Fatalf("unwedged mirror cycle failed, so there is no healthy cost to size the wedged run against: %v\noutput:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "attempted=true push_deadline_cut=false") {
		t.Fatalf("the sample cycle was cut or never attempted under the production deadline, so its cost is not the cost of a healthy cycle:\noutput:\n%s", out.String())
	}
	ws, err := workspace.Resolve(runEnv(t), rig.root)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	now := time.Now()
	outcome, age, recorded := lastPushOutcome(testStderr(t), ws, now)
	if !recorded || outcome.Decision != pushDecisionPushed || age > now.Sub(started) {
		t.Fatalf("the sample cycle did not land a push (outcome %+v, recorded %t, %s old against a %s sample), so its cost is not the cost of a healthy cycle:\noutput:\n%s",
			outcome, recorded, age, now.Sub(started), out.String())
	}
	if probe, probeErr := rig.run(90*time.Second, "new", "--type", "epic", "--title", "post-measurement probe", "--topic", "demo"); probeErr != nil {
		t.Fatalf("re-seed the unpushed commit the sample cycle pushed: %v\noutput:\n%s", probeErr, probe.String())
	}
	return healthyCycle
}

// unboundedRunTripwire is when a mirror run is hung rather than working: the
// measured work ahead of the wedge (none for the sample, which has no measure
// yet and no wedge), the deadline in force for this run, and the lag a cut
// takes to unwind — then the same margin again, because a tripwire that lands
// on legal work reports the wrong failure.
func unboundedRunTripwire(healthyCycle, wedgedDeadline time.Duration) time.Duration {
	return wedgeDeadlineMargin * (healthyCycle + wedgedDeadline + store.MirrorPushCancelLagObserved)
}

// awaitWedge waits for the shim to stamp its marker — the instant the wedged
// push began — or for the mirror to finish first, which proves nothing about a
// push in flight and says so. Polling is the only way to observe another
// process's file write; the bound is the run's own tripwire.
func awaitWedge(t *testing.T, wedgeMarker string, tripwire time.Duration, mirror mirrorRun) time.Time {
	t.Helper()
	deadline := time.Now().Add(tripwire)
	for {
		if marker, err := os.Stat(wedgeMarker); err == nil {
			return marker.ModTime()
		}
		select {
		case res := <-mirror.done:
			t.Fatalf("the mirror finished before its push engaged the wedge, so the mutation would have raced nothing:\noutput:\n%s", res.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wedge never engaged within %s", tripwire)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wedgeEngagedAt is the vacuous-pass guard and the push's clock in one. The
// shim stamps its marker before it sleeps, so the marker exists only if the
// wedged git subcommand genuinely ran, and its timestamp is the instant the
// hang began. A run whose deadline fired first proves nothing and says so.
func wedgeEngagedAt(t *testing.T, wedgeMarker, wedged string, out *lockedBuffer) time.Time {
	t.Helper()
	marker, err := os.Stat(wedgeMarker)
	if err != nil {
		t.Fatalf("the wedge never engaged (marker missing: %v) — the %s short-circuited and the test proved nothing:\noutput:\n%s", err, wedged, out.String())
	}
	return marker.ModTime()
}

// assertPushEndedWithinItsCeiling pins where a cut push ends, measured from
// the wedge and not from the cycle's start: everything before the wedge engaged
// is work whose cost this test does not own, and folding it into the bound
// would be a wall-clock bet.
//
// The bound is the deadline plus the lag cancellation takes to unwind — the
// store's own MirrorPushCancelLagObserved. [LAW:one-source-of-truth]
func assertPushEndedWithinItsCeiling(t *testing.T, wedgedAt, cycleEnded time.Time, wedgedDeadline time.Duration, out *lockedBuffer) {
	t.Helper()
	ceiling := wedgedDeadline + store.MirrorPushCancelLagObserved
	if held := cycleEnded.Sub(wedgedAt); held > ceiling {
		t.Fatalf("the mirror's push ran %s past the wedge (deadline %s, ceiling %s) — the deadline is not working:\noutput:\n%s",
			held.Round(time.Millisecond), wedgedDeadline, ceiling, out.String())
	}
}

// TestDeadlineCutFramingSurvivesTheBanner pins the claim the two cut
// explanations make about their own word order: the FAILING banner renders
// them through oneLineReason, which keeps the first line and caps it at 160
// runes, so the part that stops a reader blaming the network (or the disk) has
// to survive that cut. A reword, or a deadline whose Duration formats longer
// than "40s", would otherwise truncate the framing away silently and leave the
// banner saying only that a budget was exceeded.
//
// The duration cases are the enumeration this needs: a value as short as the
// shipped "1s", and a value in minutes, which Go renders as "1h40m0s" — more
// than twice the runes of the shipped "40s". Not parallel: it mutates
// store.MirrorPushDeadline and store.MirrorHoldBudget.
func TestDeadlineCutFramingSurvivesTheBanner(t *testing.T) {
	cases := []struct {
		name    string
		knob    *time.Duration
		explain func() error
		blame   string
	}{
		{"push deadline", &store.MirrorPushDeadline, pushDeadlineCutExplanation, "before blaming the remote"},
		{"hold budget, clone step", &store.MirrorHoldBudget, func() error { return holdBudgetCutExplanation("cloning the store") }, "before blaming the disk"},
		{"hold budget, record step", &store.MirrorHoldBudget, func() error { return holdBudgetCutExplanation("recording the pushed head") }, "before blaming the disk"},
	}
	for _, tc := range cases {
		for _, value := range []time.Duration{time.Second, 100 * time.Minute} {
			func() {
				restore := *tc.knob
				*tc.knob = value
				defer func() { *tc.knob = restore }()

				banner := oneLineReason(tc.explain().Error())
				for _, want := range []string{
					value.String(),
					"a deadline, not a diagnosis",
					"mirror.log",
				} {
					if !strings.Contains(banner, want) {
						t.Fatalf("%s at %s: the banner dropped %q — the 160-rune cut landed before the framing, so the one line a reader sees says a deadline was exceeded and nothing about how to tell a slow operation from an undersized bound.\nbanner: %s",
							tc.name, value, want, banner)
					}
				}
				if strings.HasSuffix(banner, "…") && !strings.Contains(banner, tc.blame) {
					t.Fatalf("%s at %s: the banner truncated mid-framing: %s", tc.name, value, banner)
				}
			}()
		}
	}
}
