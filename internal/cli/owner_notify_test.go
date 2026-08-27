package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func notifyTestWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	root := t.TempDir()
	return workspace.Info{RootDir: root, Location: workspace.LocationFromStorageDir(filepath.Join(root, ".lit"))}
}

// enableOwnerNotify opts one test back into the owner channel: the package
// TestMain disables all auto-sync side effects (which the hook is one of), and
// the global-config layer is pointed at an empty temp file so the developer's
// real config — possibly carrying a real notifier — can never leak into a test
// run.
func enableOwnerNotify(t *testing.T, ws workspace.Info, hook string) {
	t.Helper()
	t.Setenv(DisableAutoSyncEnvVar, "0")
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", filepath.Join(t.TempDir(), "global-config.toml"))
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		t.Fatalf("mkdir storage dir: %v", err)
	}
	payload := "[sync]\nowner_notify_cmd = '" + hook + "'\n"
	if err := os.WriteFile(filepath.Join(ws.StorageDir, "config.toml"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

// TestOwnerNotifyDue pins the dedupe decision: absent marker fires, a fresh
// marker with the same fingerprint suppresses, a changed fingerprint fires
// regardless of age, and the cooldown re-fires a persisting episode.
func TestOwnerNotifyDue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "owner-notify.push_failed.last")
	now := time.Now()

	if !ownerNotifyDue(marker, "push_failed origin/master", now) {
		t.Fatal("absent marker suppressed the first notification")
	}
	if err := os.WriteFile(marker, []byte("push_failed origin/master\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if ownerNotifyDue(marker, "push_failed origin/master", now) {
		t.Fatal("a fresh same-fingerprint marker did not suppress")
	}
	if !ownerNotifyDue(marker, "push_failed backup/master", now) {
		t.Fatal("a different fingerprint (new episode) was suppressed")
	}
	if !ownerNotifyDue(marker, "push_failed origin/master", now.Add(ownerNotifyCooldown+time.Minute)) {
		t.Fatal("a persisting episode did not re-fire after the cooldown")
	}
}

// TestOwnerNotifyEventForFailure pins the trigger set to the ticket's "real
// divergence" definition: the three divergence classes notify with the
// failure's own domain sentence; a remote-schema-ahead block (a version
// condition, not a divergence) does not.
func TestOwnerNotifyEventForFailure(t *testing.T) {
	t.Parallel()
	for _, class := range []syncFailureClass{syncFailureProseHeld, syncFailureDivergedUnresolved, syncFailureUnrelatedHistories} {
		ev, ok := ownerNotifyEventForFailure(SyncFailure{Class: class, Remote: "origin", Branch: "master"})
		if !ok {
			t.Fatalf("class %q did not map to an owner notification", class)
		}
		if ev.Kind != ownerNotifyKind(class) || ev.Remote != "origin" || ev.Branch != "master" || ev.Summary == "" {
			t.Fatalf("class %q mapped to %+v, want its own kind/target and a summary", class, ev)
		}
	}
	if _, ok := ownerNotifyEventForFailure(SyncFailure{Class: syncFailureRemoteSchemaAhead}); ok {
		t.Fatal("remote-schema-ahead mapped to an owner notification; it is not a divergence")
	}
}

// TestOwnerNotifyEventFingerprint pins the episode identity: a divergence
// episode is per sync target (a re-pointed remote is a new fork), while a push
// episode is per kind — one broken channel failing at different stages (refs
// check vs. refused push) must not read as several episodes.
func TestOwnerNotifyEventFingerprint(t *testing.T) {
	t.Parallel()
	unrelatedA := ownerNotifyEvent{Kind: ownerNotifyKind(syncFailureUnrelatedHistories), Remote: "origin", Branch: "master"}
	unrelatedB := ownerNotifyEvent{Kind: ownerNotifyKind(syncFailureUnrelatedHistories), Remote: "backup", Branch: "master"}
	if unrelatedA.fingerprint() == unrelatedB.fingerprint() {
		t.Fatal("divergences against different remotes share a fingerprint; a re-pointed remote would be silently deduplicated")
	}
	pushResolved := ownerNotifyEvent{Kind: ownerNotifyPushFailed, Remote: "origin", Branch: "master"}
	pushUnresolved := ownerNotifyEvent{Kind: ownerNotifyPushFailed}
	if pushResolved.fingerprint() != pushUnresolved.fingerprint() {
		t.Fatal("push failures at different stages have different fingerprints; one outage would ping the owner per flap")
	}
}

// TestMaybeNotifyOwnerRunsHookOncePerEpisode drives the full send path: the
// hook runs with the event's facts in LIT_NOTIFY_* env, a repeat detection of
// the same episode is suppressed by the sent marker, and clearing the kind
// makes the next detection a new episode that fires again.
func TestMaybeNotifyOwnerRunsHookOncePerEpisode(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s %s/%s\n" "$LIT_NOTIFY_KIND" "$LIT_NOTIFY_REMOTE" "$LIT_NOTIFY_BRANCH" >> `+sink)

	ev := ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "push failing", Remote: "origin", Branch: "master"}
	maybeNotifyOwner(context.Background(), ws, ev)
	maybeNotifyOwner(context.Background(), ws, ev)

	payload, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("hook never ran: %v", err)
	}
	if got, want := string(payload), "push_failed origin/master\n"; got != want {
		t.Fatalf("hook output = %q, want exactly one %q (dedupe failed or env wrong)", got, want)
	}

	clearOwnerNotify(ws, ownerNotifyPushFailed)
	maybeNotifyOwner(context.Background(), ws, ev)
	payload, err = os.ReadFile(sink)
	if err != nil {
		t.Fatalf("read sink after clear: %v", err)
	}
	if got := strings.Count(string(payload), "push_failed origin/master"); got != 2 {
		t.Fatalf("after clearing the episode the next detection fired %d times total, want 2", got)
	}
}

// TestMaybeNotifyOwnerFailedHookRetries pins the no-silent-failure half: a
// failing hook writes no sent marker, so the next detection retries instead of
// silently considering the owner informed.
func TestMaybeNotifyOwnerFailedHookRetries(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, "false")

	ev := ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "push failing", Remote: "origin", Branch: "master"}
	maybeNotifyOwner(context.Background(), ws, ev)
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed hook still wrote the sent marker (stat err=%v); the owner was never notified", err)
	}

	// The channel repaired: the very next detection sends.
	enableOwnerNotify(t, ws, `printf sent >> `+sink)
	maybeNotifyOwner(context.Background(), ws, ev)
	if _, err := os.Stat(sink); err != nil {
		t.Fatalf("the detection after a failed hook did not retry: %v", err)
	}
}

// TestMaybeNotifyOwnerHonorsAutoSyncKillSwitch pins the suite-safety property:
// under LIT_DISABLE_AUTO_SYNC (CI, sandboxes, this test suite's own default)
// no hook runs, however the config is set.
func TestMaybeNotifyOwnerHonorsAutoSyncKillSwitch(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf sent >> `+sink)
	t.Setenv(DisableAutoSyncEnvVar, "1")

	maybeNotifyOwner(context.Background(), ws, ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "s", Remote: "o", Branch: "m"})
	if _, err := os.Stat(sink); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hook ran under the auto-sync kill switch (stat err=%v)", err)
	}
}

// TestObservePushOutcomeForOwner pins the push half's episode semantics: a
// failed attempt notifies, a landed push deletes the episode marker, and a
// deliberately cancelled attempt does neither — an operator abandoning a push
// is not the remote degrading. The cancelled record is built through
// pushOutcomeOf, the real seam, so this also pins that the derivation (not
// this observer) owns the cancellation class.
func TestObservePushOutcomeForOwner(t *testing.T) {
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s\n" "$LIT_NOTIFY_KIND" >> `+sink)
	ctx := context.Background()

	failed := pushOutcomeRecord{Decision: pushDecisionError, Reason: "remote unreachable", Remote: "origin", Branch: "master"}
	observePushOutcomeForOwner(ctx, ws, failed)
	if payload, err := os.ReadFile(sink); err != nil || string(payload) != "push_failed\n" {
		t.Fatalf("failed push did not notify exactly once: payload=%q err=%v", payload, err)
	}
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); err != nil {
		t.Fatalf("failed push left no episode marker: %v", err)
	}

	observePushOutcomeForOwner(ctx, ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a landed push did not end the episode (stat err=%v)", err)
	}

	cancelled := pushOutcomeOf(syncPushOutcome{}, context.Canceled)
	if cancelled.Decision != pushDecisionCanceled || cancelled.failed() {
		t.Fatalf("pushOutcomeOf(canceled) = %+v, want the non-failed canceled decision", cancelled)
	}
	observePushOutcomeForOwner(ctx, ws, cancelled)
	if payload, _ := os.ReadFile(sink); strings.Count(string(payload), "push_failed") != 1 {
		t.Fatalf("a cancelled push notified the owner: %q", payload)
	}
}

// TestSyncReceiveOutcomeSettledCleanly pins which receive outcomes end a
// divergence episode: only a completed receive that left no unresolved
// divergence. Skips and failures carry no information; a non-converged
// reconcile keeps the episode standing.
func TestSyncReceiveOutcomeSettledCleanly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome syncReceiveOutcome
		want    bool
	}{
		{"fast-forwarded", syncReceiveOutcome{status: "ok"}, true},
		{"skipped no remote", syncReceiveOutcome{status: "skipped", reason: "no_sync_remote"}, false},
		{"receive failed", syncReceiveOutcome{status: "ok", receiveErr: errors.New("network")}, false},
		{"reconcile linearized", syncReceiveOutcome{status: "ok", reconcile: &reconcileOutcome{state: storage.SyncReconcileLinearized}}, true},
		{"reconcile no longer diverged", syncReceiveOutcome{status: "ok", reconcile: &reconcileOutcome{state: storage.SyncReconcileNotDiverged}}, true},
		{"reconcile held prose", syncReceiveOutcome{status: "ok", reconcile: &reconcileOutcome{state: storage.SyncReconcileProsePending}}, false},
		{"reconcile unrelated", syncReceiveOutcome{status: "ok", reconcile: &reconcileOutcome{state: storage.SyncReconcileUnrelated}}, false},
		{"reconcile errored", syncReceiveOutcome{status: "ok", reconcile: &reconcileOutcome{state: storage.SyncReconcileLinearized, err: errors.New("gc")}}, false},
	}
	for _, tc := range cases {
		if got := tc.outcome.settledCleanly(); got != tc.want {
			t.Errorf("%s: settledCleanly = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestOwnerApprovalRefusalBlock pins the take refusal's contract: conflict
// exit, no drifting remediation line (the block IS the remediation), and a
// block that names the destruction, the keep-everything alternative, the
// owner-approved rerun with its token, and — for a stale token — that the
// approval no longer matches.
func TestOwnerApprovalRefusalBlock(t *testing.T) {
	t.Parallel()
	refusal := ownerApprovalRefusalError{
		Approval: store.OwnerApprovalRequiredError{
			Choice:        storage.TakeLocal,
			ApprovalToken: "deadbeef0123",
			LocalHead:     "aaaaaaaaaaaaaaaaaaaa",
			RemoteHead:    "bbbbbbbbbbbbbbbbbbbb",
			Inventory: &storage.UnrelatedInventory{
				OnlyLocal:  []string{"proj-mine"},
				OnlyRemote: []string{"proj-theirs"},
			},
		},
		Remote: "origin",
		Branch: "master",
	}
	if code := ExitCode(refusal); code != ExitConflict {
		t.Fatalf("refusal exit code = %d, want %d (ExitConflict)", code, ExitConflict)
	}
	if reason := commandErrorReason(refusal); reason != "owner_approval_required" {
		t.Fatalf("refusal reason = %q, want owner_approval_required", reason)
	}
	if remediation := commandErrorRemediation(commandErrorReason(refusal)); remediation != "" {
		t.Fatalf("refusal has a second remediation line %q; the block is the remediation", remediation)
	}
	block := refusal.Error()
	for _, want := range []string{
		"DESTRUCTIVE",
		"proj-theirs",
		"WHAT EACH SIDE HOLDS",
		"lit sync reconcile combine",
		"lit sync reconcile take local --owner-approved deadbeef0123",
		"owner",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("refusal block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "no longer matches") {
		t.Errorf("a fresh refusal rendered the stale-token sentence:\n%s", block)
	}

	refusal.Approval.Stale = true
	if !strings.Contains(refusal.Error(), "no longer matches") {
		t.Errorf("a stale refusal did not say the token no longer matches:\n%s", refusal.Error())
	}
}
