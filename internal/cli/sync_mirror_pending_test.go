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

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestClaimMirrorPendingStateMachine pins the claim's three inputs and two
// outputs: an absent marker is claimed (creating it), a fresh marker covers
// (without touching it — an observer refreshing the mtime would keep a dead
// mirror's residue eternally fresh on a busy workspace, exactly the stranding
// the staleness bound exists to recover), and a stale marker is re-claimed
// with its claim time refreshed so concurrent observers bind to the re-spawn.
func TestClaimMirrorPendingStateMachine(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), ".lit")}}
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

	claim, err = claimMirrorPending(ws, now.Add(time.Second))
	if err != nil {
		t.Fatalf("claimMirrorPending on fresh marker: %v", err)
	}
	if claim != pendingCovered {
		t.Fatal("a fresh marker must cover — its dedicated mirror's HEAD read is still ahead")
	}
	info, err = os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("stat after covered observe: %v", err)
	}
	if !info.ModTime().Equal(claimedAt) {
		t.Fatalf("a covered observe refreshed the marker mtime (%v -> %v); observers must never touch it", claimedAt, info.ModTime())
	}

	staleNow := now.Add(mirrorPendingStaleAfter + time.Minute)
	claim, err = claimMirrorPending(ws, staleNow)
	if err != nil {
		t.Fatalf("claimMirrorPending on stale marker: %v", err)
	}
	if claim != pendingClaimed {
		t.Fatal("a stale marker must be re-claimed — its dedicated mirror died before clearing")
	}
	info, err = os.Stat(mirrorPendingMarkerPath(ws))
	if err != nil {
		t.Fatalf("stat after stale re-claim: %v", err)
	}
	if !info.ModTime().After(claimedAt) {
		t.Fatal("a stale re-claim must refresh the claim time so observers bind to the re-spawn")
	}
}

// TestClearMirrorPendingIdempotent pins that clearing is a truthful removal in
// both call orders: clearing an existing claim removes it, and clearing an
// already-absent marker (a racing attempt got there first) is a quiet no-op —
// the marker's absence IS the goal state, not an error.
func TestClearMirrorPendingIdempotent(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), ".lit")}}
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

// TestMirrorPendingSetIgnoresStaleness pins the holder's post-release re-check
// semantics: ANY marker — fresh or stale — means a claim may sit behind the
// last HEAD read and deserves a cycle. Staleness matters only to the claim
// (who spawns), never to the re-check (whether to push again).
func TestMirrorPendingSetIgnoresStaleness(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), ".lit")}}
	if mirrorPendingSet(ws) {
		t.Fatal("an absent marker read as set")
	}
	if _, err := claimMirrorPending(ws, time.Now()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !mirrorPendingSet(ws) {
		t.Fatal("a fresh marker read as unset")
	}
	longAgo := time.Now().Add(-2 * mirrorPendingStaleAfter)
	if err := os.Chtimes(mirrorPendingMarkerPath(ws), longAgo, longAgo); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}
	if !mirrorPendingSet(ws) {
		t.Fatal("a stale marker read as unset; the re-check must cycle for it")
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
		t.Fatal("a dying mirror left its claim behind; the next mutation would falsely read as covered until the staleness bound")
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

// TestClaimMirrorPendingFutureMtimeIsStale pins the backward-clock-step
// recovery: a marker stamped in the future can only be a crash orphan seen
// across a clock correction, and reading it as covered would suppress every
// spawn until wall clock caught up — it must be re-claimed instead.
func TestClaimMirrorPendingFutureMtimeIsStale(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), ".lit")}}
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
		t.Fatal("a future-stamped marker read as covered; it must be re-claimed")
	}
}

// TestRecheckMirrorPending pins the holder's post-release verdict: no marker
// ends the loop cleanly; a marker claimed after the cycle began demands
// another cycle; a marker OLDER than the cycle survived the cycle's own
// entry-clear — the clear is failing — and must stop the loop with an error
// rather than cycle forever against a marker that cannot be removed.
func TestRecheckMirrorPending(t *testing.T) {
	ws := workspace.Info{Location: workspace.Location{StorageDir: filepath.Join(t.TempDir(), ".lit")}}
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

// TestRunBackgroundMirrorTeardownReleasesClaim pins the teardown ending: a
// mirror whose context is already done releases the pending claim (so the
// next mutation re-claims and re-spawns at once, instead of the claim falsely
// covering mutations for the staleness window) and records NO push outcome —
// the teardown attempted nothing, so the last completed attempt's record must
// keep answering "where do things stand".
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
