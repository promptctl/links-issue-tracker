package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// seedRealWorkspace builds a real Dolt workspace at doltRoot carrying one issue
// per title, then returns its below-the-gate dump. The dump's DoltHead is the
// workspace's actual live head, so a candidate Recover builds from it promotes
// cleanly unless the workspace advances afterward — the realistic recover input,
// unlike a synthetic dump whose head would never match a real canonical.
func seedRealWorkspace(t *testing.T, ctx context.Context, doltRoot string, titles ...string) RawDump {
	t.Helper()
	withStore(t, ctx, doltRoot, func(st *Store) {
		for _, title := range titles {
			if _, err := st.CreateIssue(ctx, CreateIssueInput{
				Title: title, IssueType: "task", Topic: "recovery", Prefix: "links",
			}); err != nil {
				t.Fatalf("seed issue %q: %v", title, err)
			}
		}
	})
	dump, err := DumpRaw(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("DumpRaw seed: %v", err)
	}
	return dump
}

// freshExportIDs reads the issue IDs from the on-disk workspace at src via a
// never-before-opened path. The embedded Dolt driver caches engine state per path
// within a process, so reopening a path that was opened and then swapped earlier
// in the same test can return STALE rows; copying to a fresh path and opening that
// reflects committed disk state. Production reopens are fresh processes, so this is
// a test-only concern, but it is load-bearing for asserting a swap actually landed.
func freshExportIDs(t *testing.T, ctx context.Context, src string) map[string]bool {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "freshread")
	if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("copy %q for fresh read: %v (%s)", src, err, out)
	}
	st, err := Open(ctx, dst, "test-workspace-id")
	if err != nil {
		t.Fatalf("open fresh copy of %q: %v", src, err)
	}
	defer st.Close()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export fresh copy of %q: %v", src, err)
	}
	ids := map[string]bool{}
	for _, is := range export.Issues {
		ids[is.ID] = true
	}
	return ids
}

// hasPromotionBackup reports whether any promotion backup exists beside the
// canonical dir — the disk evidence that a swap moved the original aside.
func hasPromotionBackup(t *testing.T, canonicalDoltDir string) bool {
	t.Helper()
	backup, err := newestBackup(canonicalDoltDir)
	if err != nil {
		t.Fatalf("scan backups: %v", err)
	}
	return backup != ""
}

// markerDir creates a stand-in workspace directory carrying one identifying file,
// so a test can prove which directory ended up at the canonical path.
func markerDir(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "marker"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, "marker"))
	if err != nil {
		t.Fatalf("read marker at %q: %v", path, err)
	}
	return string(data)
}

// TestPromoteCandidateEndToEnd is the epic's acceptance, run on the workspace
// shape the lost-update guard protects: a real, openable workspace recovers
// autonomously to a Doctor-clean rebuild promoted in place — its head unchanged
// across the recovery — with the pre-recovery contents preserved as a backup and
// the rebuilt data readable afterward. Because the dump is taken from the live
// workspace, its recorded head matches at promote time and the lost-update gate
// passes silently.
func TestPromoteCandidateEndToEnd(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")

	dump := seedRealWorkspace(t, ctx, canonical, "alpha rescue subject", "beta rescue subject")
	originalIDs := freshExportIDs(t, ctx, canonical)
	if len(originalIDs) != 2 {
		t.Fatalf("seed produced %d issues, want 2", len(originalIDs))
	}

	outcome, err := Recover(ctx, canonical, dump, DeterministicMapper, 1)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	recon, ok := outcome.(Reconciled)
	if !ok {
		t.Fatalf("want Reconciled, got %T", outcome)
	}

	result, err := PromoteCandidate(ctx, canonical, recon.Candidate)
	if err != nil {
		t.Fatalf("PromoteCandidate: %v", err)
	}
	if err := recon.Candidate.Discard(); err != nil {
		t.Fatalf("discard candidate scratch: %v", err)
	}

	// The pre-recovery workspace is preserved as a backup, never wiped: every
	// original issue survives in it.
	backupIDs := freshExportIDs(t, ctx, result.Backup)
	for id := range originalIDs {
		if !backupIDs[id] {
			t.Fatalf("backup did not preserve original issue %q; backup has %v", id, backupIDs)
		}
	}

	// The rebuilt data is now the canonical workspace: readable, Doctor-clean, and
	// every source issue conserved. Read via a fresh path so the assertion reflects
	// the swapped-in directory on disk, not a cached engine for the canonical path.
	promotedIDs := freshExportIDs(t, ctx, canonical)
	if len(promotedIDs) != 2 {
		t.Fatalf("promoted workspace lost data: want 2 issues, got %d", len(promotedIDs))
	}
	for id := range originalIDs {
		if !promotedIDs[id] {
			t.Fatalf("rebuild did not conserve original issue %q; promoted has %v", id, promotedIDs)
		}
	}
	st, err := Open(ctx, canonical, "test-workspace-id")
	if err != nil {
		t.Fatalf("reopen promoted workspace: %v", err)
	}
	defer st.Close()
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor on promoted workspace: %v", err)
	}
	mustClean(t, report)
}

// TestPromoteCandidateAbortsOnConcurrentCommit is the j0vl.7 acceptance: when a
// concurrent commit advances the live workspace between the dump and the
// promotion, the promote refuses rather than silently regressing the workspace to
// dump-time state. Nothing on disk changes — no backup is made, and the concurrent
// commit stays live.
func TestPromoteCandidateAbortsOnConcurrentCommit(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")

	dump := seedRealWorkspace(t, ctx, canonical, "original subject")
	outcome, err := Recover(ctx, canonical, dump, DeterministicMapper, 1)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	recon, ok := outcome.(Reconciled)
	if !ok {
		t.Fatalf("want Reconciled, got %T", outcome)
	}

	// A concurrent committer advances the live workspace in place, past the head
	// the candidate was rebuilt from.
	var concurrentID string
	withStore(t, ctx, canonical, func(st *Store) {
		issue, err := st.CreateIssue(ctx, CreateIssueInput{
			Title: "landed during recovery", IssueType: "task", Topic: "recovery", Prefix: "links",
		})
		if err != nil {
			t.Fatalf("concurrent commit: %v", err)
		}
		concurrentID = issue.ID
	})

	_, err = PromoteCandidate(ctx, canonical, recon.Candidate)
	if !errors.Is(err, ErrWorkspaceAdvanced) {
		t.Fatalf("PromoteCandidate over an advanced workspace: err = %v; want ErrWorkspaceAdvanced", err)
	}
	if err := recon.Candidate.Discard(); err != nil {
		t.Fatalf("discard candidate: %v", err)
	}

	// Nothing was changed: no backup was made (the abort precedes the move-aside),
	// and the concurrent commit is still live — not regressed out.
	if hasPromotionBackup(t, canonical) {
		t.Fatal("an aborted promotion made a backup; nothing should have moved")
	}
	liveIDs := freshExportIDs(t, ctx, canonical)
	if !liveIDs[concurrentID] {
		t.Fatalf("the concurrent commit was regressed out of the live workspace; live has %v", liveIDs)
	}
}

// TestHealCanonicalRestoresInterruptedSwap covers the one interrupted-at-rest
// state a swap can leave — canonical absent, backup present — and asserts the heal
// restores the original (rolls back), not forward to anything else.
func TestHealCanonicalRestoresInterruptedSwap(t *testing.T) {
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	backup := canonical + ".backup-1700000000000000001"
	markerDir(t, backup, "original")
	// canonical is deliberately absent: the crash window between the two renames.

	if err := healCanonical(canonical); err != nil {
		t.Fatalf("healCanonical: %v", err)
	}
	if got := readMarker(t, canonical); got != "original" {
		t.Fatalf("heal restored wrong contents: want original, got %q", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup should have been consumed by the restore, stat err=%v", err)
	}
}

// TestHealCanonicalPicksNewestBackup proves the restore is chronological: among
// several backups the newest (highest fixed-width stamp) is the one restored.
func TestHealCanonicalPicksNewestBackup(t *testing.T) {
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	markerDir(t, canonical+".backup-1700000000000000001", "older")
	markerDir(t, canonical+".backup-1700000000000000002", "newer")

	if err := healCanonical(canonical); err != nil {
		t.Fatalf("healCanonical: %v", err)
	}
	if got := readMarker(t, canonical); got != "newer" {
		t.Fatalf("heal restored the wrong backup: want newer, got %q", got)
	}
}

// TestUniqueBackupPathStepsPastCollision proves a promotion never reuses an
// existing backup path even if the nanosecond stamp repeats, so the prior backup —
// the most precious artifact in the flow — is never clobbered.
func TestUniqueBackupPathStepsPastCollision(t *testing.T) {
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	const stamp int64 = 1700000000000000001
	taken := fmt.Sprintf("%s.backup-%019d", canonical, stamp)
	markerDir(t, taken, "prior")

	got, err := uniqueBackupPath(canonical, stamp)
	if err != nil {
		t.Fatalf("uniqueBackupPath: %v", err)
	}
	if got == taken {
		t.Fatalf("must not reuse the existing backup path %q", taken)
	}
	if !isPromotionBackup(filepath.Base(got), filepath.Base(canonical)+".backup-") {
		t.Fatalf("stepped path %q must still be a recognized backup name", got)
	}
}

// TestHealCanonicalIgnoresForeignBackupNames proves only producer-stamped backups
// are restorable: a hand-named directory sharing the prefix is never selected,
// even though it sorts lexicographically after the numeric stamps and would
// otherwise roll the workspace back to the wrong contents.
func TestHealCanonicalIgnoresForeignBackupNames(t *testing.T) {
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	markerDir(t, canonical+".backup-1700000000000000001", "real")
	markerDir(t, canonical+".backup-manual", "foreign")

	if err := healCanonical(canonical); err != nil {
		t.Fatalf("healCanonical: %v", err)
	}
	if got := readMarker(t, canonical); got != "real" {
		t.Fatalf("heal selected a foreign backup: want real, got %q", got)
	}
}

// TestHealWorkspaceRestoresAfterCrash is the standalone repair a fresh recovery
// runs first: a workspace left with its canonical directory absent (a swap killed
// between its two renames) is restored from the newest backup; a healthy
// workspace is left untouched.
func TestHealWorkspaceRestoresAfterCrash(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	markerDir(t, canonical+".backup-1700000000000000007", "pre-crash")

	if err := HealWorkspace(ctx, canonical); err != nil {
		t.Fatalf("HealWorkspace: %v", err)
	}
	if got := readMarker(t, canonical); got != "pre-crash" {
		t.Fatalf("heal did not restore the pre-crash workspace: marker=%q", got)
	}

	// Running again on the now-healthy workspace must be a no-op, not a re-restore.
	if err := HealWorkspace(ctx, canonical); err != nil {
		t.Fatalf("HealWorkspace (no-op): %v", err)
	}
	if got := readMarker(t, canonical); got != "pre-crash" {
		t.Fatalf("no-op heal disturbed the workspace: marker=%q", got)
	}
}

// TestPromoteCandidateRejectsDiscardedCandidate proves an already-consumed
// candidate cannot drive a promotion: detachForPromotion fails loudly rather than
// handing back a cwd-relative "workspace" path that would rename an unintended
// directory into the canonical location.
func TestPromoteCandidateRejectsDiscardedCandidate(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	markerDir(t, canonical, "original")

	cand, err := RebuildCandidate(ctx, storageDir, preGooseDump(), mustMap(t, preGooseDump()))
	if err != nil {
		t.Fatalf("RebuildCandidate: %v", err)
	}
	if err := cand.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := PromoteCandidate(ctx, canonical, cand); err == nil {
		t.Fatal("PromoteCandidate must reject a discarded candidate")
	}
	// The canonical workspace must be untouched — no swap was attempted.
	if got := readMarker(t, canonical); got != "original" {
		t.Fatalf("a rejected promotion disturbed the canonical workspace: marker=%q", got)
	}
}

// TestPromoteCandidateRollsBackOnInstallFailure exercises the deferred rollback:
// when the second rename cannot complete, the moved-aside original is restored to
// the canonical path and the failure is surfaced — never a half-swapped or empty
// canonical, never the unverified candidate.
func TestPromoteCandidateRollsBackOnInstallFailure(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")

	// A real workspace at canonical, whose head the candidate is built to match, so
	// the lost-update gate passes and the install-failure path is the one exercised.
	dump := seedRealWorkspace(t, ctx, canonical, "original subject")
	originalIDs := freshExportIDs(t, ctx, canonical)

	cand, err := RebuildCandidate(ctx, storageDir, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	// Make the install fail deterministically: surrender the candidate's Dolt
	// directory and remove it, so PromoteCandidate's second rename hits a missing
	// source. (detach + remove leaves the candidate's scratch root intact.)
	src, err := cand.detachForPromotion()
	if err != nil {
		t.Fatalf("detach candidate: %v", err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatalf("remove candidate dolt dir: %v", err)
	}

	if _, err := PromoteCandidate(ctx, canonical, cand); err == nil {
		t.Fatal("PromoteCandidate must fail when the rebuilt directory is missing")
	}

	// Rolled back: the original workspace is restored at the canonical path with
	// its data intact (read fresh, since the path was swapped during the failed
	// install and then restored).
	restoredIDs := freshExportIDs(t, ctx, canonical)
	for id := range originalIDs {
		if !restoredIDs[id] {
			t.Fatalf("rollback did not restore original issue %q; canonical has %v", id, restoredIDs)
		}
	}
}
