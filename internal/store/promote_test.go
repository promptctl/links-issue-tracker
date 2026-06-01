package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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

// TestPromoteCandidateEndToEnd is the epic's acceptance: a recognized broken
// workspace recovers autonomously to a Doctor-clean rebuild promoted in place,
// with the pre-recovery contents preserved as a backup and the rebuilt data
// readable at the canonical path afterward.
func TestPromoteCandidateEndToEnd(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")

	// The deadended workspace at the canonical path — what recovery preserves.
	// It stands in for a store Open() refuses, so it is a plain directory rather
	// than a clean workspace: recovery reads its data via DumpRaw (here supplied
	// directly as the dump), never by opening it.
	markerDir(t, canonical, "deadended-original")

	dump := preGooseDump()
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

	// The pre-recovery workspace is preserved verbatim, never wiped.
	if got := readMarker(t, result.Backup); got != "deadended-original" {
		t.Fatalf("backup did not preserve the original verbatim: marker=%q", got)
	}

	// The rebuilt data is now the canonical workspace: readable, Doctor-clean, and
	// every source issue conserved.
	st, err := Open(ctx, canonical, "legacy-ws")
	if err != nil {
		t.Fatalf("reopen promoted workspace: %v", err)
	}
	defer st.Close()
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor on promoted workspace: %v", err)
	}
	mustClean(t, report)
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export promoted workspace: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("promoted workspace lost data: want 2 issues, got %d", len(export.Issues))
	}
}

// TestHealCanonicalRestoresInterruptedSwap covers the one interrupted-at-rest
// state a swap can leave — canonical absent, backup present — and asserts the heal
// restores the original (rolls back), not forward to anything else.
func TestHealCanonicalRestoresInterruptedSwap(t *testing.T) {
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	backup := canonical + ".backup-00000000000000001"
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
	markerDir(t, canonical+".backup-00000000000000001", "older")
	markerDir(t, canonical+".backup-00000000000000002", "newer")

	if err := healCanonical(canonical); err != nil {
		t.Fatalf("healCanonical: %v", err)
	}
	if got := readMarker(t, canonical); got != "newer" {
		t.Fatalf("heal restored the wrong backup: want newer, got %q", got)
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
	markerDir(t, canonical+".backup-00000000000000007", "pre-crash")

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

// TestPromoteCandidateRollsBackOnInstallFailure exercises the deferred rollback:
// when the second rename cannot complete, the moved-aside original is restored to
// the canonical path and the failure is surfaced — never a half-swapped or empty
// canonical, never the unverified candidate.
func TestPromoteCandidateRollsBackOnInstallFailure(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()
	canonical := filepath.Join(storageDir, "dolt")
	markerDir(t, canonical, "original")

	cand, err := RebuildCandidate(ctx, storageDir, preGooseDump(), mustMap(t, preGooseDump()))
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

	// Rolled back: the original is restored at the canonical path.
	if got := readMarker(t, canonical); got != "original" {
		t.Fatalf("rollback did not restore the original: marker=%q", got)
	}
}
