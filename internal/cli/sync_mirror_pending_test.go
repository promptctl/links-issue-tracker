package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/filelock"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// mirrorPendingTestWorkspace builds the minimal workspace the claim protocol
// touches: a storage dir for the marker and a database path whose sibling
// position anchors the liveness beacon the claim probes.
func mirrorPendingTestWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	root := t.TempDir()
	return workspace.Info{Location: workspace.Location{
		StorageDir:   filepath.Join(root, ".lit"),
		DatabasePath: filepath.Join(root, ".lit", "dolt"),
	}}
}

// TestClaimMirrorPendingStateMachine pins the claim's inputs and two outputs
// under the beacon's kernel liveness proof (links-locking-il18.4): an absent
// marker is claimed (creating it); a marker whose mirror holds the beacon
// covers, without touching the marker's mtime and no matter what that mtime
// says — the verdict belongs to the kernel, never to a wall-clock reading;
// and the instant the holder is gone (release standing in for process death —
// an flock evaporates either way) the same marker is residue, re-claimed with
// its claim time refreshed so concurrent observers bind to the re-spawn.
func TestClaimMirrorPendingStateMachine(t *testing.T) {
	ws := mirrorPendingTestWorkspace(t)
	now := time.Now()

	claim, err := claimMirrorPending(ws, now)
	if err != nil {
		t.Fatalf("claimMirrorPending on absent marker: %v", err)
	}
	if claim != pendingClaimed {
		t.Fatal("an absent marker must be claimed — the claimant owes the spawn")
	}
	info, err := os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("claim did not create the marker: %v", err)
	}
	claimedAt := info.ModTime()

	releaseBeacon, err := store.HoldMirrorBeacon(context.Background(), ws.DatabasePath)
	if err != nil {
		t.Fatalf("hold mirror beacon as the live mirror: %v", err)
	}
	claim, err = claimMirrorPending(ws, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claimMirrorPending under a live holder: %v", err)
	}
	if claim != pendingCovered {
		t.Fatal("a marker with a live beacon holder must cover — that mirror's HEAD read is still ahead")
	}
	// Backdate far past any plausible healthy window: under the retired
	// age-out this read as abandoned residue; under the beacon the live hold
	// alone decides, so it still covers.
	longAgo := now.Add(-24 * time.Hour)
	if err := os.Chtimes(mirrorPendingMarkerPath(ws), longAgo, longAgo); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	info, err = os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("stat backdated marker: %v", err)
	}
	backdatedAt := info.ModTime()
	claim, err = claimMirrorPending(ws, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("claimMirrorPending on backdated marker under a live holder: %v", err)
	}
	if claim != pendingCovered {
		t.Fatal("a live holder must cover however old the marker is; liveness is the kernel's verdict, not the clock's")
	}
	info, err = os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("stat after covered observes: %v", err)
	}
	if !info.ModTime().Equal(backdatedAt) {
		t.Fatalf("a covered observe refreshed the marker mtime (%v -> %v); observers must never touch it", backdatedAt, info.ModTime())
	}

	if err := releaseBeacon(); err != nil {
		t.Fatalf("release beacon (the answerer dies): %v", err)
	}

	// An exclusive squatter must never read as covered: covered would spawn
	// nothing and stop pushes silently, while re-claiming routes the squatter
	// into the spawned mirror's loud beacon-hold failure.
	releaseSquat, acquired, err := filelock.Acquire(context.Background(), store.MirrorBeaconLockPath(ws.DatabasePath), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take exclusive squat: acquired=%v err=%v", acquired, err)
	}
	claim, err = claimMirrorPending(ws, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("claimMirrorPending under exclusive squatter: %v", err)
	}
	if claim != pendingClaimed {
		t.Fatal("an exclusively obstructed beacon must re-claim, never cover — a squatter would otherwise stop pushes silently")
	}
	if err := releaseSquat(); err != nil {
		t.Fatalf("release squat: %v", err)
	}

	reclaimNow := now.Add(4 * time.Second)
	claim, err = claimMirrorPending(ws, reclaimNow)
	if err != nil {
		t.Fatalf("claimMirrorPending on residue: %v", err)
	}
	if claim != pendingClaimed {
		t.Fatal("a marker with no live beacon holder is residue and must be re-claimed the moment it is observed")
	}
	info, err = os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("stat after residue re-claim: %v", err)
	}
	if !info.ModTime().After(backdatedAt) {
		t.Fatal("a residue re-claim must refresh the claim time so observers bind to the re-spawn")
	}
	if claimedAt.After(info.ModTime()) {
		t.Fatal("refreshed claim time went backwards")
	}
}

// TestClearMirrorPendingIdempotent pins that clearing is a truthful removal in
// both call orders: clearing an existing claim removes it, and clearing an
// already-absent marker (a racing attempt got there first) is a quiet no-op —
// the marker's absence IS the goal state, not an error.
func TestClearMirrorPendingIdempotent(t *testing.T) {
	ws := mirrorPendingTestWorkspace(t)
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	clearMirrorPending(ws)
	if mirrorPendingSet(ws) {
		t.Fatal("clear left the marker in place")
	}
	clearMirrorPending(ws)
	if mirrorPendingSet(ws) {
		t.Fatal("clearing an absent marker resurrected it")
	}
}

// TestMirrorPendingSetIgnoresLiveness pins the existence read's semantics:
// ANY marker — its mirror alive or long dead — means a claim may sit behind
// the last HEAD read and deserves a cycle. Liveness matters only to the claim
// (who spawns), never to this read (whether a mirror is still owed).
func TestMirrorPendingSetIgnoresLiveness(t *testing.T) {
	ws := mirrorPendingTestWorkspace(t)
	if mirrorPendingSet(ws) {
		t.Fatal("an absent marker read as set")
	}
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !mirrorPendingSet(ws) {
		t.Fatal("a just-claimed marker read as unset")
	}
	longAgo := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(mirrorPendingMarkerPath(ws), longAgo, longAgo); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	if !mirrorPendingSet(ws) {
		t.Fatal("a dead claimant's residue read as unset; the re-check must cycle for it")
	}
}

// TestCompleteMirrorWithoutAttempt pins this ticket's seam contract
// (links-sync-pgct.12, closing the gap links-sync-pgct.10 documented): a
// mirror failure BEFORE the push attempt lands in the same push-outcome
// marker and the same owner notification as an attempt that ran — one
// completion record, two consumers, no second representation of push health.
func TestCompleteMirrorWithoutAttempt(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s\n" "$LIT_NOTIFY_KIND" >> `+sink)

	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	cause := errors.New("spawning command (pid 4242) still running after 30s")
	if err := completeMirrorWithoutAttempt(context.Background(), ws, cause); err != nil {
		t.Fatalf("completeMirrorWithoutAttempt must be best-effort nil, got %v", err)
	}

	if mirrorPendingSet(ws) {
		t.Fatal("a dying mirror left its claim behind for the next claim's probe to recover; code-running endings must release it themselves")
	}

	rec, _, ok := lastPushOutcome(ws, time.Now())
	if !ok {
		t.Fatal("a pre-attempt mirror failure left no push-outcome marker; the staleness banner cannot see it")
	}
	if !rec.failed() {
		t.Fatalf("pre-attempt failure recorded decision %q, want %q", rec.Decision, pushDecisionError)
	}
	if !strings.Contains(rec.Reason, "still running after") {
		t.Fatalf("marker reason %q lost the cause", rec.Reason)
	}
	payload, err := os.ReadFile(sink)
	if err != nil || string(payload) != "push_failed\n" {
		t.Fatalf("pre-attempt failure did not reach the owner exactly once: payload=%q err=%v", payload, err)
	}
}

// TestCompleteMirrorWithoutAttemptCancelledSkipsOwner pins the cancellation
// class riding through the shared seam: a mirror torn down by its own context
// mid-startup records the ending as "canceled" — never "error", so no FAILING
// banner colors later commands over a deliberate teardown — and the owner
// hook stays silent, identical to a cancelled explicit push.
func TestCompleteMirrorWithoutAttemptCancelledSkipsOwner(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf sent >> `+sink)

	cause := fmt.Errorf("open sync store: %w", context.Canceled)
	if err := completeMirrorWithoutAttempt(context.Background(), ws, cause); err != nil {
		t.Fatalf("completeMirrorWithoutAttempt must be best-effort nil, got %v", err)
	}
	rec, _, ok := lastPushOutcome(ws, time.Now())
	if !ok || rec.Decision != pushDecisionCanceled || rec.failed() {
		t.Fatalf("a cancelled mirror must record the non-failed canceled decision (ok=%v rec=%+v)", ok, rec)
	}
	if _, err := os.Stat(sink); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cancelled mirror notified the owner (stat err=%v)", err)
	}
}

// TestCompleteMirrorWithoutAttemptBusySkipsOwner pins the workspace-busy
// class: a mirror timing out behind another lit process legitimately holding
// the workspace's one write engine (a long reconcile or import) is a healthy
// serialization, not channel degradation — no FAILING banner, no owner page,
// while the ending is still recorded and the claim still released.
func TestCompleteMirrorWithoutAttemptBusySkipsOwner(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf sent >> `+sink)
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}

	cause := fmt.Errorf("open sync store: %w", store.ErrWorkspaceBusy)
	if err := completeMirrorWithoutAttempt(context.Background(), ws, cause); err != nil {
		t.Fatalf("completeMirrorWithoutAttempt must be best-effort nil, got %v", err)
	}
	rec, _, ok := lastPushOutcome(ws, time.Now())
	if !ok || rec.Decision != pushDecisionWorkspaceBusy || rec.failed() {
		t.Fatalf("a busy-workspace ending must record the non-failed workspace_busy decision (ok=%v rec=%+v)", ok, rec)
	}
	if _, err := os.Stat(sink); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a busy-workspace ending notified the owner (stat err=%v)", err)
	}
	if mirrorPendingSet(ws) {
		t.Fatal("a busy-workspace ending left its claim behind")
	}
}

// TestClaimMirrorPendingClockStepIrrelevant pins that the verdict survives
// any clock reading: a marker stamped in the future (a crash orphan seen
// across a backward RTC/NTP correction) is residue like any other when no
// mirror holds the beacon — the retired age-out needed a dedicated
// negative-age branch for this; the kernel's answer never consulted the
// clock in the first place.
func TestClaimMirrorPendingClockStepIrrelevant(t *testing.T) {
	ws := mirrorPendingTestWorkspace(t)
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	future := time.Now().Add(90 * time.Second)
	if err := os.Chtimes(mirrorPendingMarkerPath(ws), future, future); err != nil {
		t.Fatalf("future-date marker: %v", err)
	}
	claim, err := claimMirrorPending(ws, time.Now())
	if err != nil {
		t.Fatalf("claimMirrorPending on future marker: %v", err)
	}
	if claim != pendingClaimed {
		t.Fatal("a future-stamped marker with no live holder read as covered; it must be re-claimed")
	}
}

// TestRecheckMirrorPending pins the holder's post-release verdict: no marker
// ends the loop cleanly; a marker claimed after the cycle began demands
// another cycle; a marker OLDER than the cycle survived the cycle's own
// entry-clear — the clear is failing — and must stop the loop with an error
// rather than cycle forever against a marker that cannot be removed.
func TestRecheckMirrorPending(t *testing.T) {
	ws := mirrorPendingTestWorkspace(t)
	cycleStart := time.Now()

	again, err := recheckMirrorPending(ws, cycleStart)
	if err != nil || again {
		t.Fatalf("absent marker: (again=%v err=%v), want clean done", again, err)
	}

	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	again, err = recheckMirrorPending(ws, cycleStart)
	if err != nil || !again {
		t.Fatalf("post-cycle claim: (again=%v err=%v), want another cycle", again, err)
	}

	before := cycleStart.Add(-time.Minute)
	if err := os.Chtimes(mirrorPendingMarkerPath(ws), before, before); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	again, err = recheckMirrorPending(ws, cycleStart)
	if err == nil || again {
		t.Fatalf("pre-cycle survivor: (again=%v err=%v), want a loud stop — the entry-clear failed", again, err)
	}
}

// TestRunBackgroundMirrorRefusesWithoutBeacon pins the hold side of the
// liveness contract: a mirror that cannot take the beacon must not run —
// invisible to every claimant's probe, its work would only draw redundant
// siblings — and that ending flows through the same completion seam as every
// other pre-attempt death: claim released, outcome recorded as a FAILED
// error, never the non-paging workspace-busy class. A persistent exclusive
// holder is anomalous by the beacon's own contract (probes hold for
// microseconds), and while it squats every mirror refuses to run — channel
// degradation the FAILING banner and the owner channel must hear about, not
// healthy engine serialization.
func TestRunBackgroundMirrorRefusesWithoutBeacon(t *testing.T) {
	ws := notifyTestWorkspace(t)
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	releaseSquat, acquired, err := filelock.Acquire(context.Background(), store.MirrorBeaconLockPath(ws.DatabasePath), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("squat on beacon: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		if err := releaseSquat(); err != nil {
			t.Fatalf("release squat: %v", err)
		}
	}()

	if err := runBackgroundMirror(context.Background(), os.Stderr, ws, nil); err != nil {
		t.Fatalf("runBackgroundMirror must be best-effort nil, got %v", err)
	}
	if mirrorPendingSet(ws) {
		t.Fatal("a beacon-refused mirror left its claim behind; the next mutation must re-claim at once")
	}
	rec, _, ok := lastPushOutcome(ws, time.Now())
	if !ok {
		t.Fatal("a beacon-refused mirror left no push-outcome record")
	}
	if !rec.failed() {
		t.Fatalf("beacon contention outlasting the probe budget must record a FAILED ending (pushes are stopped and nobody else will say so), got %+v", rec)
	}
}

// TestRunBackgroundMirrorTeardownReleasesClaim pins the teardown ending: a
// mirror whose context is already done releases the pending claim (so the
// next mutation re-claims and re-spawns at once, without even needing its
// beacon probe) and records NO push outcome — the teardown attempted nothing,
// so the last completed attempt's record must keep answering "where do things
// stand".
func TestRunBackgroundMirrorTeardownReleasesClaim(t *testing.T) {
	ws := notifyTestWorkspace(t)
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runBackgroundMirror(ctx, os.Stderr, ws, nil); err != nil {
		t.Fatalf("runBackgroundMirror teardown must be best-effort nil, got %v", err)
	}
	if mirrorPendingSet(ws) {
		t.Fatal("teardown left the claim behind; mutations would falsely read as covered")
	}
	if _, _, ok := lastPushOutcome(ws, time.Now()); ok {
		t.Fatal("teardown wrote a push outcome; the last completed attempt's record must stand")
	}
}
