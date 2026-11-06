package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func notifyTestWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	root := t.TempDir()
	return workspace.Info{RootDir: root, Location: workspace.LocationFromStorageDir(filepath.Join(root, ".lit")), Remotes: workspace.NewRemotes(runEnv(t), repositoryOf(t, root), checkoutRoot(t, root)), Project: testProject(t, checkoutRoot(t, root))}
}

// shPath spells path for a hook's sh command line: quoted, and with forward
// slashes, since sh reads a backslash as an escape and resolves D:/... on
// Windows as it does D:\....
func shPath(path string) string {
	return `"` + filepath.ToSlash(path) + `"`
}

// enableOwnerNotify opts one test back into the owner channel: the package
// TestMain disables all auto-sync side effects (which the hook is one of), and
// the global-config layer is pointed at an empty temp file so the developer's
// real config — possibly carrying a real notifier — can never leak into a test
// run.
func enableOwnerNotify(t *testing.T, ws workspace.Info, hook string) {
	t.Helper()
	setRunEnv(t, DisableAutoSyncEnvVar, "0")
	setRunEnv(t, "LIT_CONFIG_GLOBAL_PATH", filepath.Join(t.TempDir(), "global-config.toml"))
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

// TestOwnerNotifyEventForFailure pins the partition of the closed class set:
// every divergence class, an unsettled receive, and every schema refusal,
// whichever half detects it, notifies under its own kind with the failure's
// own domain sentence; a push refused over a peer's commits and a lost
// forced-push lease do not, because the push half pages them as push_failed.
// Both lists are spelled out by hand and then checked against
// syncFailureClasses, so a class added to the constants and to neither list
// here fails this test instead of rendering a block that pages nobody.
func TestOwnerNotifyEventForFailure(t *testing.T) {
	t.Parallel()
	// The schema classes carry the store's typed refusal, which their summary
	// reads, so they are built as a surface would build them.
	paging := []SyncFailure{
		{Class: syncFailureProseHeld, Remote: "origin", Branch: "master"},
		{Class: syncFailureDivergedUnresolved, Remote: "origin", Branch: "master"},
		{Class: syncFailureUnrelatedHistories, Remote: "origin", Branch: "master"},
		{Class: syncFailureIDCollision, Remote: "origin", Branch: "master"},
		{Class: syncFailureReceiveUnsettled, Remote: "origin", Branch: "master",
			StoppedAt: receiveStepSettle, UnsettledSince: time.Now().Add(-time.Hour), Age: time.Hour},
		{Class: syncFailureRemoteSchemaAhead, Remote: "origin", Branch: "master",
			RemoteAhead: &store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 9, BinarySupportedMax: 8}},
		{Class: syncFailureSchemaCeilingLift, Remote: "origin", Branch: "master",
			LiftRefused: &store.SchemaCeilingLiftError{Remote: "origin", Branch: "master", Lift: store.SchemaCeilingError{Raise: store.RaiseLift, WorkspaceVersion: 8, TargetVersion: 9}}},
		{Class: syncFailureSchemaCeiling, Remote: "origin", Branch: "master",
			AboveCeiling: &store.SchemaCeilingPushError{Remote: "origin", Branch: "master", CommitSchema: 9}},
		{Class: syncFailureSchemaRaiseRequiresRelease, Remote: "origin", Branch: "master",
			RaiseRefused: &store.SchemaRaiseRequiresReleaseError{Remote: "origin", Branch: "master", RemoteSchema: 8, RemoteKnown: true, CommitSchema: 9}},
		{Class: syncFailureSchemaLoweringRequiresHold, Remote: "origin", Branch: "master",
			LoweringRefused: &store.SchemaLoweringRequiresHoldError{Remote: "origin", Branch: "master", RemoteSchema: 9, CommitSchema: 8}},
	}
	pushFailed := []syncFailureClass{syncFailurePushDiverged, syncFailureForcedPushStale}

	placed := map[syncFailureClass]string{}
	for _, failure := range paging {
		placed[failure.Class] = "paging"
	}
	for _, class := range pushFailed {
		if placed[class] != "" {
			t.Fatalf("class %q is listed as both paging under its own kind and paging as push_failed", class)
		}
		placed[class] = "push_failed"
	}
	for _, class := range syncFailureClasses {
		if placed[class] == "" {
			t.Fatalf("class %q is in syncFailureClasses but this test places it under neither its own kind nor push_failed; decide which pages it", class)
		}
	}
	if len(placed) != len(syncFailureClasses) {
		t.Fatalf("this test places %d classes, syncFailureClasses holds %d; a class listed here is not in the closed set", len(placed), len(syncFailureClasses))
	}

	for _, failure := range paging {
		ev, ok := ownerNotifyEventForFailure(failure)
		if !ok {
			t.Fatalf("class %q did not map to an owner notification", failure.Class)
		}
		if ev.Kind != ownerNotifyKind(failure.Class) || ev.Remote != "origin" || ev.Branch != "master" || ev.Summary == "" {
			t.Fatalf("class %q mapped to %+v, want its own kind/target and a summary", failure.Class, ev)
		}
		if got, want := ev.fingerprint(), string(failure.Class)+" origin/master"; failure.Class != syncFailureReceiveUnsettled && got != want {
			t.Fatalf("class %q fingerprints as %q, want the per-target %q", failure.Class, got, want)
		}
	}
	for _, class := range pushFailed {
		if _, ok := ownerNotifyEventForFailure(SyncFailure{Class: class, Remote: "origin", Branch: "master"}); ok {
			t.Fatalf("class %q mapped to its own owner notification; the push half pages it as push_failed", class)
		}
	}
}

// TestForegroundReconcileNotDivergedEndsTheEpisodes pins that a foreground
// reconcile answering not-diverged ends every settle-ended episode and retires
// the pending block, and ends the remote-schema episode on what its plan read
// of the remote head: a head within support ends it, a head ahead of this
// binary leaves it standing, since a not-diverged answer by itself proves
// nothing about the remote's schema.
func TestForegroundReconcileNotDivergedEndsTheEpisodes(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	notDiverged := storage.SyncReconcileResult{State: storage.SyncReconcileNotDiverged}
	aheadNotDiverged := storage.SyncReconcileResult{State: storage.SyncReconcileNotDiverged, RemoteSchemaAhead: storage.RemoteSchemaAhead{RemoteSchema: 9, BinarySupportedMax: 8}}

	plantSettleEndedEpisodes(t, ws)
	var out strings.Builder
	if err := reportTakeOutcome(testStderr(t), runEnv(t), &out, ws, "lit sync reconcile take remote", remoteName(t, "origin"), "master", notDiverged); err != nil {
		t.Fatalf("reportTakeOutcome: %v", err)
	}
	requireSettleEndedEpisodesEnded(t, ws, "take not-diverged", storage.RemoteSchemaAhead{})

	plantSettleEndedEpisodes(t, ws)
	out.Reset()
	if err := reportReconcileResult(context.Background(), testStderr(t), runEnv(t), &out, ws, syncSession{}, "lit sync reconcile", remoteName(t, "origin"), "master", notDiverged, false); err != nil {
		t.Fatalf("reportReconcileResult: %v", err)
	}
	requireSettleEndedEpisodesEnded(t, ws, "reconcile not-diverged", storage.RemoteSchemaAhead{})

	plantSettleEndedEpisodes(t, ws)
	out.Reset()
	if err := reportReconcileResult(context.Background(), testStderr(t), runEnv(t), &out, ws, syncSession{}, "lit sync reconcile", remoteName(t, "origin"), "master", aheadNotDiverged, false); err != nil {
		t.Fatalf("reportReconcileResult(ahead): %v", err)
	}
	requireSettleEndedEpisodesEnded(t, ws, "reconcile not-diverged against an ahead remote", aheadNotDiverged.RemoteSchemaAhead)
}

// plantSettleEndedEpisodes leaves a live marker of every kind a settle can
// end and a pending sync-failure block, the state a settle must clear.
func plantSettleEndedEpisodes(t *testing.T, ws workspace.Info) {
	t.Helper()
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		t.Fatalf("mkdir storage dir: %v", err)
	}
	for _, kind := range kindsEndedBy(endedBySettle, endedBySchemaRead) {
		if err := os.WriteFile(ownerNotifyMarkerPath(ws, kind), []byte("fp"), 0o644); err != nil {
			t.Fatalf("write %s marker: %v", kind, err)
		}
	}
	if err := os.WriteFile(receiveBlockPendingPath(ws), []byte("block\n"), 0o644); err != nil {
		t.Fatalf("write pending block: %v", err)
	}
}

// requireSettleEndedEpisodesEnded fails unless every marker a settle that
// read the remote head at remote ends, and the pending block, are gone, and
// unless the remote-schema marker still stands when that read was ahead;
// surface names what was supposed to end them.
func requireSettleEndedEpisodesEnded(t *testing.T, ws workspace.Info, surface string, remote storage.RemoteSchemaAhead) {
	t.Helper()
	ended := kindsEndedBy(settleProofs(remote)...)
	for _, kind := range ended {
		if _, err := os.Stat(ownerNotifyMarkerPath(ws, kind)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left the %s episode live (stat err=%v)", surface, kind, err)
		}
	}
	schema := ownerNotifyKind(syncFailureRemoteSchemaAhead)
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, schema)); remote.Ahead() && err != nil {
		t.Fatalf("%s ended the %s episode though it read the remote head ahead of this binary: %v", surface, schema, err)
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s left the pending block waiting (stat err=%v)", surface, err)
	}
}

// TestKindsEndedByIsTheOneTable pins the ender table: every class that pages
// under its own kind has an ender, so none can page and never clear; the
// divergence kinds and the ceiling lift end on a settle alone; the push kinds
// end on a landed push alone; and the remote schema ahead, detected by both
// halves, ends on a landed push or on a read of the remote head within
// support, never on a settle's state alone.
func TestKindsEndedByIsTheOneTable(t *testing.T) {
	t.Parallel()
	// The classes with no ender of their own: an unsettled receive, whose
	// episode is its run of stops, and the two the push half pages as
	// push_failed. Every other class, and push_failed itself, has one.
	noEnder := []syncFailureClass{syncFailureReceiveUnsettled, syncFailurePushDiverged, syncFailureForcedPushStale}
	for _, class := range syncFailureClasses {
		enders := ownerNotifyEnders[ownerNotifyKind(class)]
		if slices.Contains(noEnder, class) != (len(enders) == 0) {
			t.Fatalf("class %q has %d enders; the table and the paging set disagree", class, len(enders))
		}
	}
	if len(ownerNotifyEnders[ownerNotifyPushFailed]) == 0 {
		t.Fatalf("%s has no ender", ownerNotifyPushFailed)
	}
	if len(ownerNotifyEnders) != len(syncFailureClasses)-len(noEnder)+1 {
		t.Fatalf("the table holds %d kinds; want every paging class plus %s", len(ownerNotifyEnders), ownerNotifyPushFailed)
	}
	schema := ownerNotifyKind(syncFailureRemoteSchemaAhead)
	if slices.Contains(kindsEndedBy(endedBySettle), schema) {
		t.Fatalf("a settle's state alone ends %s", schema)
	}
	if slices.Contains(kindsEndedBy(endedByPush), schema) {
		t.Fatalf("a landed push ends %s, though a push at the remote's own schema lands", schema)
	}
	if !slices.Contains(kindsEndedBy(endedBySchemaRead), schema) {
		t.Fatalf("a read of the remote head within support does not end %s", schema)
	}
	if got := kindsEndedBy(settleProofs(storage.RemoteSchemaAhead{RemoteSchema: 9, BinarySupportedMax: 8})...); slices.Contains(got, schema) {
		t.Fatalf("a settle that read the remote head ahead ends %s: %v", schema, got)
	}
	if got := kindsEndedBy(settleProofs(storage.RemoteSchemaAhead{})...); !slices.Contains(got, schema) {
		t.Fatalf("a settle that read the remote head within support does not end %s: %v", schema, got)
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
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s %s/%s\n" "$LIT_NOTIFY_KIND" "$LIT_NOTIFY_REMOTE" "$LIT_NOTIFY_BRANCH" >> `+shPath(sink))

	ev := ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "push failing", Remote: "origin", Branch: "master"}
	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ev)
	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ev)

	payload, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("hook never ran: %v", err)
	}
	if got, want := string(payload), "push_failed origin/master\n"; got != want {
		t.Fatalf("hook output = %q, want exactly one %q (dedupe failed or env wrong)", got, want)
	}

	clearOwnerNotify(testStderr(t), ws, ownerNotifyPushFailed)
	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ev)
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
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, "false")

	ev := ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "push failing", Remote: "origin", Branch: "master"}
	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ev)
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed hook still wrote the sent marker (stat err=%v); the owner was never notified", err)
	}

	// The channel repaired: the very next detection sends.
	enableOwnerNotify(t, ws, `printf sent >> `+shPath(sink))
	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ev)
	if _, err := os.Stat(sink); err != nil {
		t.Fatalf("the detection after a failed hook did not retry: %v", err)
	}
}

// TestMaybeNotifyOwnerHonorsAutoSyncKillSwitch pins the suite-safety property:
// under LIT_DISABLE_AUTO_SYNC (CI, sandboxes, this test suite's own default)
// no hook runs, however the config is set.
func TestMaybeNotifyOwnerHonorsAutoSyncKillSwitch(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf sent >> `+shPath(sink))
	setRunEnv(t, DisableAutoSyncEnvVar, "1")

	maybeNotifyOwner(context.Background(), testStderr(t), runEnv(t), ws, ownerNotifyEvent{Kind: ownerNotifyPushFailed, Summary: "s", Remote: "o", Branch: "m"})
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
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s\n" "$LIT_NOTIFY_KIND" >> `+shPath(sink))
	ctx := context.Background()
	// The failures here are no schema refusal, which is what the completion
	// seam would have parsed; TestPushRefusedBySchemaPagesUnderItsClass pins
	// that parse.
	observe := func(rec pushOutcomeRecord) {
		observePushOutcomeForOwner(ctx, testStderr(t), runEnv(t), ws, rec, SyncFailure{}, false)
	}

	failed := pushOutcomeRecord{Decision: pushDecisionError, Reason: "remote unreachable", Remote: "origin", Branch: "master"}
	observe(failed)
	if payload, err := os.ReadFile(sink); err != nil || string(payload) != "push_failed\n" {
		t.Fatalf("failed push did not notify exactly once: payload=%q err=%v", payload, err)
	}
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); err != nil {
		t.Fatalf("failed push left no episode marker: %v", err)
	}

	observe(pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"})
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a landed push did not end the episode (stat err=%v)", err)
	}

	cancelled, _, _ := pushOutcomeOf(syncPushOutcome{}, context.Canceled, "")
	if cancelled.Decision != pushDecisionCanceled || cancelled.failed() {
		t.Fatalf("pushOutcomeOf(canceled) = %+v, want the non-failed canceled decision", cancelled)
	}
	observe(cancelled)
	if payload, _ := os.ReadFile(sink); strings.Count(string(payload), "push_failed") != 1 {
		t.Fatalf("a cancelled push notified the owner: %q", payload)
	}

	// A push refused over a peer's commits a receive merges neither notifies
	// nor ends a live episode: the channel is not broken, and it has not been
	// shown healthy.
	observe(failed)
	diverged, _, _ := pushOutcomeOf(syncPushOutcome{remote: remoteName(t, "origin"), branch: "master", receiveMerges: true, pushErr: fmt.Errorf("%w: rejected", storage.ErrPushDiverged)}, nil, "")
	if diverged.Decision != pushDecisionDiverged || diverged.failed() {
		t.Fatalf("pushOutcomeOf(diverged) = %+v, want the non-failed diverged decision", diverged)
	}
	observe(diverged)
	if payload, _ := os.ReadFile(sink); strings.Count(string(payload), "push_failed") != 2 {
		t.Fatalf("want the second failure's one notification and none for the diverged push: %q", payload)
	}
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); err != nil {
		t.Fatalf("a diverged push ended the live failure episode: %v", err)
	}
}

// TestPushRefusedBySchemaPagesUnderItsClass pins that the push half pages a
// schema refusal under the refusal's own kind, through the completion seam
// every push attempt ends at: a push the released ceiling refused pages
// schema_ceiling, keyed to the remote and branch, and writes no push_failed
// marker; the same refusal again is the same episode and pages nothing; a
// landed push ends it, so the next refusal pages at once. A remote whose
// schema is ahead is the receive half's condition: the reconcile's refusal
// pages remote_schema_ahead, a landed push leaves that episode standing (a
// push at the remote's own schema lands, so it proves nothing about the
// remote's schema), and a settle whose read found the remote head within
// support ends it.
func TestPushRefusedBySchemaPagesUnderItsClass(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s %s %s\n" "$LIT_NOTIFY_KIND" "$LIT_NOTIFY_REMOTE" "$LIT_NOTIFY_BRANCH" >> `+shPath(sink))
	// The completion seam lands the attempt on the invocation's event, so it
	// runs under one, as every command does.
	ctx := telemetry.With(context.Background(), telemetry.Begin(telemetry.Event{}))
	complete := func(pushErr error) {
		completePushAttempt(ctx, testStderr(t), runEnv(t), ws, syncPushOutcome{remote: remoteName(t, "origin"), branch: "master", pushErr: pushErr}, nil)
	}
	ceiling := &store.SchemaCeilingPushError{Remote: "origin", Branch: "master", CommitSchema: 9}

	complete(ceiling)
	if payload, err := os.ReadFile(sink); err != nil || string(payload) != "schema_ceiling origin master\n" {
		t.Fatalf("a push refused by the ceiling did not page schema_ceiling exactly once: payload=%q err=%v", payload, err)
	}
	if _, err := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyPushFailed)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a schema refusal left a push_failed marker (stat err=%v)", err)
	}
	marker := ownerNotifyMarkerPath(ws, ownerNotifyKind(syncFailureSchemaCeiling))
	if payload, err := os.ReadFile(marker); err != nil || string(payload) != "schema_ceiling origin/master\n" {
		t.Fatalf("schema_ceiling marker = %q (err=%v), want the per-target fingerprint", payload, err)
	}

	complete(ceiling)
	if payload, _ := os.ReadFile(sink); strings.Count(string(payload), "schema_ceiling") != 1 {
		t.Fatalf("the same refusal paged again inside its episode: %q", payload)
	}

	complete(nil)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a landed push did not end the schema_ceiling episode (stat err=%v)", err)
	}
	complete(ceiling)
	if payload, _ := os.ReadFile(sink); strings.Count(string(payload), "schema_ceiling") != 2 {
		t.Fatalf("a refusal after a landed push did not page as a new episode: %q", payload)
	}

	// The receive half: the reconcile's replay refused by a remote head at a
	// schema above this binary's pages remote_schema_ahead. A landed push
	// leaves that episode standing; a settle that read the head within
	// support ends it.
	ahead, ok := remoteSchemaAheadFailure(&store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 9, BinarySupportedMax: 8})
	if !ok {
		t.Fatal("the receive half did not classify the reconcile's refusal")
	}
	notifyOwnerForFailure(ctx, testStderr(t), runEnv(t), ws, ahead)
	if payload, _ := os.ReadFile(sink); !strings.HasSuffix(string(payload), "remote_schema_ahead origin master\n") {
		t.Fatalf("the reconcile's refusal did not page remote_schema_ahead: %q", payload)
	}
	aheadMarker := ownerNotifyMarkerPath(ws, ownerNotifyKind(syncFailureRemoteSchemaAhead))
	complete(nil)
	if _, err := os.Stat(aheadMarker); err != nil {
		t.Fatalf("a landed push ended the remote_schema_ahead episode, which a push at the remote's own schema does not prove gone: %v", err)
	}
	endSettleEndedEpisodes(testStderr(t), ws, storage.RemoteSchemaAhead{})
	if _, err := os.Stat(aheadMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a settle that read the remote head within support did not end the remote_schema_ahead episode (stat err=%v)", err)
	}

	complete(errors.New("remote unreachable"))
	if payload, _ := os.ReadFile(sink); !strings.HasSuffix(string(payload), "push_failed origin master\n") {
		t.Fatalf("a failure that is no schema refusal did not page push_failed: %q", payload)
	}

	// An attempt error outranks the push step's own error in the record (a
	// panic after a refused push is recorded as the panic), and the page
	// follows the record: the refusal beside it is not consulted.
	complete(nil)
	completePushAttempt(ctx, testStderr(t), runEnv(t), ws, syncPushOutcome{remote: remoteName(t, "origin"), branch: "master", pushErr: ceiling}, errors.New("sync push panicked: boom"))
	payload, _ := os.ReadFile(sink)
	if !strings.HasSuffix(string(payload), "push_failed  \n") || strings.Count(string(payload), "schema_ceiling") != 2 {
		t.Fatalf("an attempt error beside a refused push did not page as the record's failure: %q", payload)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an attempt error beside a refused push opened a schema_ceiling episode (stat err=%v)", err)
	}
}

// TestLandedPushEndsEveryPushEndedKind pins the group a landed push ends: a
// live marker of every kind in it is gone after one landed push, and a marker
// only a settle ends still stands, since a landed push says nothing about a
// lift the ceiling refused, nor about the remote head's schema (a push at the
// remote's own schema lands).
func TestLandedPushEndsEveryPushEndedKind(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	if err := os.MkdirAll(ws.StorageDir, 0o755); err != nil {
		t.Fatalf("mkdir storage dir: %v", err)
	}
	standing := []ownerNotifyKind{ownerNotifyKind(syncFailureSchemaCeilingLift), ownerNotifyKind(syncFailureRemoteSchemaAhead)}
	for _, kind := range append(kindsEndedBy(endedByPush), standing...) {
		if err := os.WriteFile(ownerNotifyMarkerPath(ws, kind), []byte(string(kind)+" origin/master\n"), 0o644); err != nil {
			t.Fatalf("plant %s marker: %v", kind, err)
		}
	}
	observePushOutcomeForOwner(context.Background(), testStderr(t), runEnv(t), ws, pushOutcomeRecord{Decision: pushDecisionPushed, Remote: "origin", Branch: "master"}, SyncFailure{}, false)
	for _, kind := range kindsEndedBy(endedByPush) {
		if _, err := os.Stat(ownerNotifyMarkerPath(ws, kind)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a landed push left the %s episode standing (stat err=%v)", kind, err)
		}
	}
	for _, kind := range standing {
		if _, err := os.Stat(ownerNotifyMarkerPath(ws, kind)); err != nil {
			t.Fatalf("a landed push ended the %s episode, which only a settle proves gone: %v", kind, err)
		}
	}
}

// TestSurfaceSyncFailurePagesTheOwner pins the foreground half of the receive
// side: an explicit pull or reconcile whose engine call returns a schema refusal
// renders the contract AND pages the owner, since on a checkout with no
// automatic receive that command is the only detector. A class the push half
// pages as push_failed, returned through the same point, pages nobody and is
// reported on stderr as a bug: no pull or reconcile pushes, so the push half
// never saw it. An error that is no sync failure passes through untouched.
func TestSurfaceSyncFailurePagesTheOwner(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	sink := filepath.Join(t.TempDir(), "notifications")
	enableOwnerNotify(t, ws, `printf "%s\n" "$LIT_NOTIFY_KIND" >> `+shPath(sink))
	ctx := context.Background()

	lift := &store.SchemaCeilingLiftError{Remote: "origin", Branch: "master", Lift: store.SchemaCeilingError{Raise: store.RaiseLift, WorkspaceVersion: 8, TargetVersion: 9}}
	err := surfaceSyncFailure(ctx, testStderr(t), runEnv(t), ws, lift)
	var failure SyncFailureError
	if !errors.As(err, &failure) || failure.Failure.Class != syncFailureSchemaCeilingLift {
		t.Fatalf("surfaceSyncFailure(lift) = %v, want the schema_ceiling_lift contract", err)
	}
	if payload, readErr := os.ReadFile(sink); readErr != nil || string(payload) != "schema_ceiling_lift\n" {
		t.Fatalf("a refused lift did not page exactly once: payload=%q err=%v", payload, readErr)
	}
	if _, statErr := os.Stat(ownerNotifyMarkerPath(ws, ownerNotifyKind(syncFailureSchemaCeilingLift))); statErr != nil {
		t.Fatalf("a refused lift left no episode marker: %v", statErr)
	}

	diverged := storage.PushDivergedError{Remote: remoteName(t, "origin"), Branch: "master", Refusal: errors.New("rejected")}
	var reported bytes.Buffer
	err = surfaceSyncFailure(ctx, &reported, runEnv(t), ws, diverged)
	if !errors.As(err, &failure) || failure.Failure.Class != syncFailurePushDiverged {
		t.Fatalf("surfaceSyncFailure(diverged push) = %v, want the push_diverged contract", err)
	}
	if payload, _ := os.ReadFile(sink); string(payload) != "schema_ceiling_lift\n" {
		t.Fatalf("a class with no kind of its own paged from a foreground command: %q", payload)
	}
	if !strings.Contains(reported.String(), `"push_diverged" reached the owner channel with no kind of its own`) {
		t.Fatalf("a class with no kind of its own reaching a foreground command was not reported as a bug; stderr=%q", reported.String())
	}

	plain := errors.New("remote unreachable")
	if err := surfaceSyncFailure(ctx, testStderr(t), runEnv(t), ws, plain); err != plain {
		t.Fatalf("surfaceSyncFailure(plain) = %v, want the error itself", err)
	}
}

// TestEndSettleEndedEpisodesClearsEveryKind pins the group a settle ends: a
// live marker of every kind in it is gone after one settle, so the next
// episode of that kind pages immediately instead of being deduped for a day.
func TestEndSettleEndedEpisodesClearsEveryKind(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	plantSettleEndedEpisodes(t, ws)
	endSettleEndedEpisodes(testStderr(t), ws, storage.RemoteSchemaAhead{})
	requireSettleEndedEpisodesEnded(t, ws, "a settle", storage.RemoteSchemaAhead{})
}

// TestASettleAgainstAnAheadRemoteLeavesTheSchemaEpisode pins the hole the
// read closes: a settle that landed or sat on a head whose schema is ahead of
// this binary converged, so the divergence kinds end and the block retires,
// and the remote-schema episode stands, so the next refused push is a repeat
// inside the cooldown rather than a new page.
func TestASettleAgainstAnAheadRemoteLeavesTheSchemaEpisode(t *testing.T) {
	t.Parallel()
	ws := notifyTestWorkspace(t)
	ahead := storage.RemoteSchemaAhead{RemoteSchema: 9, BinarySupportedMax: 8}
	plantSettleEndedEpisodes(t, ws)
	endSettleEndedEpisodes(testStderr(t), ws, ahead)
	requireSettleEndedEpisodesEnded(t, ws, "a settle against an ahead remote", ahead)
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
		{"fast-forwarded", syncReceiveOutcome{}, true},
		{"skipped no remote", syncReceiveOutcome{skip: syncTargetNoRemote}, false},
		{"fetch failed", syncReceiveOutcome{stopped: receiveStepFetch, err: errors.New("network")}, false},
		{"settle stopped", syncReceiveOutcome{stopped: receiveStepSettle, err: errors.New("follow")}, false},
		{"reconcile linearized", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileLinearized}}, true},
		{"reconcile no longer diverged", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileNotDiverged}}, true},
		{"reconcile held prose", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileProsePending}}, false},
		{"reconcile unrelated", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileUnrelated}}, false},
		{"reconcile refused an id collision", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileIDCollision}}, false},
		{"reconcile errored", syncReceiveOutcome{reconcile: &reconcileOutcome{state: storage.SyncReconcileLinearized}, stopped: receiveStepReconcile, err: errors.New("gc")}, false},
	}
	for _, tc := range cases {
		if got := tc.outcome.settledCleanly(); got != tc.want {
			t.Errorf("%s: settledCleanly = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSyncReceiveOutcomeAccess pins which receives the cadence owner reads as
// a write owed a push: a settle that left commits the remote lacks. Every
// receive state is listed, and every reconcile state on the diverged settle
// the inline reconcile runs after.
func TestSyncReceiveOutcomeAccess(t *testing.T) {
	t.Parallel()
	settled := func(state storage.SyncReceiveState) syncReceiveOutcome { return syncReceiveOutcome{state: state} }
	reconciled := func(state storage.SyncReconcileState) syncReceiveOutcome {
		return syncReceiveOutcome{state: storage.SyncReceiveDiverged, reconcile: &reconcileOutcome{state: state}}
	}
	cases := []struct {
		name    string
		outcome syncReceiveOutcome
		want    app.AccessMode
	}{
		{"up to date", settled(storage.SyncReceiveUpToDate), app.AccessRead},
		{"fast-forwarded", settled(storage.SyncReceiveFastForwarded), app.AccessRead},
		{"ahead", settled(storage.SyncReceiveAhead), app.AccessWrite},
		{"never synced", settled(storage.SyncReceiveNeverSynced), app.AccessRead},
		{"skipped no remote", syncReceiveOutcome{skip: syncTargetNoRemote, state: storage.SyncReceiveAhead}, app.AccessRead},
		{"fetch failed", syncReceiveOutcome{state: storage.SyncReceiveAhead, stopped: receiveStepFetch, err: errors.New("network")}, app.AccessRead},
		{"settle stopped", syncReceiveOutcome{state: storage.SyncReceiveAhead, stopped: receiveStepSettle, err: errors.New("follow")}, app.AccessRead},
		{"reconcile linearized", reconciled(storage.SyncReconcileLinearized), app.AccessWrite},
		{"reconcile no longer diverged", reconciled(storage.SyncReconcileNotDiverged), app.AccessRead},
		{"reconcile held prose", reconciled(storage.SyncReconcileProsePending), app.AccessRead},
		{"reconcile refused an id collision", reconciled(storage.SyncReconcileIDCollision), app.AccessRead},
		{"reconcile unrelated", reconciled(storage.SyncReconcileUnrelated), app.AccessRead},
	}
	for _, tc := range cases {
		if got := tc.outcome.access(); got != tc.want {
			t.Errorf("%s: access = %v, want %v", tc.name, got, tc.want)
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
				OnlyLocal:  []model.ID{"proj-mine"},
				OnlyRemote: []model.ID{"proj-theirs"},
			},
		},
		Remote: "origin",
		Branch: "master",
	}
	if code := ExitCode(refusal); code != ExitConflict {
		t.Fatalf("refusal exit code = %d, want %d (ExitConflict)", code, ExitConflict)
	}
	if reason := commandErrorReason(refusal); reason != reasonOwnerApprovalRequired {
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
