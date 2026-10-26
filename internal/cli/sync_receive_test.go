package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// A receive whose target the clone skipped settles nothing, and its event and
// trace name the skip, each once.
func TestASkippedReceiveRecordsItsSkip(t *testing.T) {
	ws := compactWorkspace(t)
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	target := syncTarget{remote: remoteName(t, "origin"), skip: syncTargetRemoteEmpty}
	outcome, closeLive := performSyncReceive(ctx, testEnv(), ws, liveClone{}, receiveFetch{target: target}, leaveUnsettledRun(filepath.Join(t.TempDir(), "dolt")))
	if outcome.skip != syncTargetRemoteEmpty {
		t.Fatalf("performSyncReceive() = %+v; want the skip", outcome)
	}
	if err := closeLive(); err != nil {
		t.Fatal(err)
	}
	outcomes := writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil))).Occurrences["receive.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("receive.outcomes = %#v; want the skip's one value", outcomes)
	}
	if value, _ := outcomes[0].(map[string]any); value["receive.decision"] != string(syncTargetRemoteEmpty) || value["receive.remote"] != "origin" {
		t.Fatalf("receive.outcomes value = %v; want the remote_empty skip on origin", value)
	}
	if traces := readSyncTraces(t, ws); len(traces) != 1 || traces[0].Decision != string(syncTargetRemoteEmpty) {
		t.Fatalf("sync traces = %+v; want the one remote_empty trace", traces)
	}
}

// A receive that ends before it settles anything, here on a repo with no git
// remote, still says it owes no push, rather than leaving the fact absent.
func TestAReceiveThatSettlesNothingOwesNoPush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitInit(t, dir)
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	receiveOnce(ctx, testEnv(), workspace.Info{Location: workspace.LocationFromStorageDir(t.TempDir()), RootDir: dir, Remotes: workspace.NewRemotes(testEnv(), repositoryOf(t, dir), checkoutRoot(t, dir)), Project: testProject(checkoutRoot(t, dir))})
	facts := writtenFacts(t, recorder.Finish(invocationEnd(time.Now(), nil, nil)))
	if owed, present := facts["receive.push_owed"]; !present || owed != false {
		t.Fatalf("receive.push_owed = %#v (present %t); want false", owed, present)
	}
}

// TestAutomaticReceiveFastForwardsEstablishedClone is the end-to-end proof of the
// feature: an established clone that adopted the remote on init, then has another
// machine push a new ticket, sees that ticket after running an ordinary command —
// with NO manual `lit sync pull`. It drives the real CLI for both clones over a
// real git remote, and runs the receive worker's body in this process
// (receiveNow), so the assertion is deterministic rather than racing a detached
// worker; the spawn itself is pinned end to end in cmd/lit.
func TestAutomaticReceiveFastForwardsEstablishedClone(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	// Producer publishes the initial backlog, then a second ticket.
	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks")
	epicID := newEpicInDir(t, producer, "shared-epic")
	runCLIInDir(t, producer, "new", "--title", "first-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	// Consumer adopts the producer's backlog on init.
	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks")
	if !strings.Contains(runCLIInDir(t, consumer, "backlog"), "first-ticket") {
		t.Fatalf("consumer did not adopt first-ticket on init")
	}

	// Another machine pushes a second ticket to the remote.
	runCLIInDir(t, producer, "new", "--title", "second-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")

	// With automatic sync still disabled (TestMain default), the established
	// consumer cannot yet see it.
	if strings.Contains(runCLIInDir(t, consumer, "backlog"), "second-ticket") {
		t.Fatalf("consumer saw second-ticket before any receive — test cannot prove receive")
	}

	// One automatic receive fast-forwards the consumer.
	receiveNow(t, consumer)

	backlog := runCLIInDir(t, consumer, "backlog")
	if !strings.Contains(backlog, "second-ticket") {
		t.Fatalf("consumer backlog missing second-ticket after automatic receive:\n%s", backlog)
	}
}

// receiveNow runs one automatic receive for the workspace at dir to
// completion, in this process: the receive worker's body, without the spawn
// and without the wait for a parent that this test process never lets exit.
func receiveNow(t *testing.T, dir string) {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("resolve workspace %s: %v", dir, err)
	}
	receiveOnce(underInvocation(context.Background()), testEnv(), ws)
}

// TestTheReceiveBlockWaitsForTheNextCommandUntilAReceiveSettles pins the
// pending block's lifecycle from the receive's side: a receive that could not
// converge leaves its sync-failure block for the next command, and a later
// receive that settled cleanly retires an unprinted one, because the
// divergence it described is gone and printing it would be a false alarm.
// [LAW:behavior-not-structure] asserted through the file a command reads.
func TestTheReceiveBlockWaitsForTheNextCommandUntilAReceiveSettles(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.LocationFromStorageDir(t.TempDir()), Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	now := time.Now()

	unconverged := syncReceiveOutcome{
		remote: remoteName(t, "origin"), branch: "master", ahead: 1, behind: 1,
		reconcile: &reconcileOutcome{},
		stopped:   receiveStepReconcile, err: errors.New("reconcile backend unavailable"),
	}
	surfaceReceiveOutcome(context.Background(), testEnv(), ws, unconverged, now)
	block, err := os.ReadFile(receiveBlockPendingPath(ws))
	if err != nil {
		t.Fatalf("an unconverged receive left no block for the next command: %v", err)
	}
	if !strings.Contains(string(block), "reconcile backend unavailable") {
		t.Fatalf("pending block does not carry the receive's failure:\n%s", block)
	}

	surfaceReceiveOutcome(context.Background(), testEnv(), ws, syncReceiveOutcome{remote: remoteName(t, "origin"), branch: "master"}, now)
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a receive that settled cleanly left the earlier block pending (stat err = %v)", err)
	}

	// Every other surface that converges a divergence — `lit sync pull`,
	// `lit sync reconcile` — ends the episode the same way, so a block left
	// before it is retired too.
	surfaceReceiveOutcome(context.Background(), testEnv(), ws, unconverged, now)
	endDivergenceEpisode(ws)
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a converged divergence left the receive's block pending (stat err = %v)", err)
	}
}

// TestTheReceiveWaitsForALiveMirror pins the worker's ordering: while a
// mirror is owed (the claim a mutating command leaves, before its mirror has
// started) or answers on the liveness beacon, the receive does not start; it
// starts once both are gone, and a mirror that outlives the bound stops
// holding it. Claim and beacon are held here the way a command and a live
// mirror hold them.
func TestTheReceiveWaitsForALiveMirror(t *testing.T) {
	t.Parallel()
	ws := workspace.Info{Location: workspace.LocationFromStorageDir(t.TempDir()), Remotes: workspace.NewRemotes(testEnv(), workspace.Repository{}, workspace.CheckoutRoot{}), Project: testProject(workspace.CheckoutRoot{})}
	if err := os.MkdirAll(ws.DatabasePath, 0o755); err != nil {
		t.Fatalf("mkdir database path: %v", err)
	}
	if waited, err := awaitNoLiveMirror(context.Background(), ws, time.Minute); err != nil || waited > time.Second {
		t.Fatalf("with no mirror live the receive waited %s (err %v), want no wait", waited, err)
	}

	// Owed, not yet live: the spawning command has exited, its mirror has not
	// taken the beacon yet.
	if err := os.WriteFile(mirrorPendingMarkerPath(ws), nil, 0o644); err != nil {
		t.Fatalf("write mirror-pending marker: %v", err)
	}
	if _, err := awaitNoLiveMirror(context.Background(), ws, 300*time.Millisecond); err == nil {
		t.Fatalf("the receive stopped waiting while a mirror was owed")
	}
	clearMirrorPending(ws)

	release, err := store.HoldMirrorBeacon(context.Background(), ws.DatabasePath)
	if err != nil {
		t.Fatalf("hold the mirror beacon: %v", err)
	}
	if _, err := awaitNoLiveMirror(context.Background(), ws, 300*time.Millisecond); err == nil {
		t.Fatalf("the receive stopped waiting while a mirror was still live")
	}
	const held = 500 * time.Millisecond
	go func() {
		time.Sleep(held)
		_ = release()
	}()
	waited, err := awaitNoLiveMirror(context.Background(), ws, time.Minute)
	if err != nil {
		t.Fatalf("the receive did not start once the mirror let go: %v", err)
	}
	if waited < held-100*time.Millisecond {
		t.Fatalf("the receive waited %s for a mirror held %s; it started while the mirror was live", waited, held)
	}
}
