package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestLiftWorkingSetToRegistryRecoversDowngradedSchema isolates the schema-lift
// primitive from the sync dance: a store whose working set sits at an OLDER
// schema (the multi-machine skew the reconcile must be total over) cannot be
// Export()ed — the binary's SELECT names columns the old schema lacks — and the
// lift is exactly what makes that state readable again by replaying the missing
// migrations' DDL. Downgrade manufactures the old schema faithfully: Dolt does
// not care whether a v2 commit came from an old binary or a downgrade.
func TestLiftWorkingSetToRegistryRecoversDowngradedSchema(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "orig", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Drop below the resolution migration (00003) — the exact column at the
	// centre of the incident. baselineVersion+1 = v2, which predates resolution.
	if err := st.Downgrade(ctx, baselineVersion+1); err != nil {
		t.Fatalf("Downgrade to v%d: %v", baselineVersion+1, err)
	}

	// Pre-lift: the binary's Export cannot read the old-schema working set. This
	// IS the incident's raw backend failure; it must fire before the fix helps.
	if _, err := st.Export(ctx); err == nil {
		t.Fatalf("Export on downgraded (v%d) working set unexpectedly succeeded; the skew was not reproduced", baselineVersion+1)
	} else if !strings.Contains(err.Error(), "resolution") {
		t.Fatalf("Export failed but not on the resolution column: %v", err)
	}

	// The lift replays 00003/00004 over the working set, uncommitted.
	if err := st.liftWorkingSetToRegistry(ctx); err != nil {
		t.Fatalf("liftWorkingSetToRegistry: %v", err)
	}

	// Post-lift: Export succeeds, the row survives, and the lifted column carries
	// its NULL default (no resolution recorded on a non-closed issue).
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export after lift: %v", err)
	}
	if len(export.Issues) != 1 {
		t.Fatalf("export has %d issues after lift, want 1", len(export.Issues))
	}
	got := export.Issues[0]
	if got.ID != issue.ID {
		t.Fatalf("lifted issue id = %q, want %q", got.ID, issue.ID)
	}
	if got.Title != "orig" {
		t.Fatalf("lifted issue title = %q, want orig (data lost across the lift)", got.Title)
	}
	if res := got.ResolutionValue(); res != nil {
		t.Fatalf("lifted issue resolution = %q, want nil (new column must default NULL)", *res)
	}
}

// TestSyncReconcileHealsSchemaSkew replays the incident shape end-to-end: an
// old-schema remote (base and theirs both predate the resolution migration) vs a
// migrated local, with a genuine divergence. The reconcile must lift both older
// anchors, merge the two sides' edits, and land linear history — with zero
// manual steps and the lifted rows carrying the new column's default. This is
// the epic acceptance: the state that used to fail on every retry forever now
// self-heals.
func TestSyncReconcileHealsSchemaSkew(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	// Seed the remote at an OLD schema: create the issue, then downgrade below
	// resolution before the first push. The remote head (the reconcile's base)
	// is now a pre-migration commit.
	id := seedOldSchemaRemote(t, ctx, rootA, remoteURL)

	// B adopts the old-schema base, then migrates locally and edits PRIORITY —
	// this is "migrated local" with an unpushed commit.
	adoptRemote(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Priority: ptr(model.PriorityUrgent)})

	// A advances the remote with a divergent LANE edit, kept at the OLD schema:
	// migrate to edit, then downgrade back below resolution before pushing. The
	// remote head (the reconcile's theirs) is a second pre-migration commit.
	advanceOldSchemaRemote(t, ctx, rootA, id, UpdateIssueInput{Lane: strptr("from-remote")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != SyncDiverged {
		t.Fatalf("pre-reconcile state = %v (ahead %d / behind %d), want diverged", fresh.State(), fresh.Ahead, fresh.Behind)
	}

	// The reconcile that used to fail forever with `table "i" does not have
	// column "resolution"` now heals the divergence with no manual steps.
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile across schema skew: %v", err)
	}
	if res.State != SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q (pending=%v), want %q", res.State, res.Pending, SyncReconcileLinearized)
	}

	// Both sides' edits survive on the merged row, and the lifted row carries the
	// new column's default.
	merged := getIssueOrFatal(t, ctx, syncB, id)
	if merged.Lane != "from-remote" {
		t.Fatalf("merged lane = %q, want from-remote (remote's edit lost across the lift)", merged.Lane)
	}
	if merged.Priority != model.PriorityUrgent {
		t.Fatalf("merged priority = %d, want urgent (local's edit lost)", merged.Priority)
	}
	if r := merged.ResolutionValue(); r != nil {
		t.Fatalf("merged resolution = %q, want nil (lifted rows default NULL)", *r)
	}

	// Linear history that fast-forward pushes, and no scratch residue.
	assertSingleParentHead(t, ctx, syncB, res.RemoteHead)
	assertScratchBranchCleanedUp(t, ctx, syncB)

	// Snapshot-first: the mutating reconcile left a recovery point so the
	// automatic data-branch advance is reversible.
	snaps, err := dbsnapshot.List(migrationSnapshotsDir(syncB.doltRootDir))
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	var reconcileSnaps int
	for _, snap := range snaps {
		if IsReconcileSnapshotName(snap.Name) {
			reconcileSnaps++
		}
	}
	if reconcileSnaps == 0 {
		t.Fatalf("reconcile took no recovery snapshot (%d total); snapshot-first not honored", len(snaps))
	}

	// Property: a linearized reconcile leaves a clean working set.
	assertWorkingSetClean(t, ctx, syncB)
}

// TestSyncPullHealsSchemaSkewDivergence is the pull-path acceptance: `lit sync
// pull` on a diverged clone whose remote is at an OLDER schema resolves the
// divergence through the field-aware reconcile engine and produces linear
// history — WITHOUT the "@autocommit must be disabled so that merge conflicts
// can be resolved" failure that native DOLT_PULL raises. The conflicting edits
// are code-owned, so the engine settles them (no prose-pending).
func TestSyncPullHealsSchemaSkewDivergence(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedOldSchemaRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Priority: ptr(model.PriorityUrgent)})
	advanceOldSchemaRemote(t, ctx, rootA, id, UpdateIssueInput{Lane: strptr("from-remote")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()

	res, err := syncB.SyncPull(ctx, "origin", "master")
	if err != nil {
		// A native DOLT_PULL would fail here with the autocommit error; the
		// reconcile-backed pull must not.
		if strings.Contains(err.Error(), "autocommit") {
			t.Fatalf("SyncPull raised the native-merge autocommit failure: %v", err)
		}
		t.Fatalf("SyncPull across schema skew: %v", err)
	}
	if res.State != SyncPullLinearized {
		t.Fatalf("pull state = %q, want %q", res.State, SyncPullLinearized)
	}

	merged := getIssueOrFatal(t, ctx, syncB, id)
	if merged.Lane != "from-remote" {
		t.Fatalf("merged lane = %q, want from-remote", merged.Lane)
	}
	if merged.Priority != model.PriorityUrgent {
		t.Fatalf("merged priority = %d, want urgent", merged.Priority)
	}

	// Property: after the pull the working set is clean — no unstaged tables, no
	// held conflicts (the failure mode the incident's manual repair had to catch).
	assertWorkingSetClean(t, ctx, syncB)
}

// assertWorkingSetClean fails if the store's Dolt working set has any staged or
// unstaged change or any held merge conflict. A clean working set after every
// reconcile outcome is the ticket's stated property: the incident's manual
// repair had to hand-stage auto-merged tables the native merge left behind, and
// the export/replay reconcile must never leave such residue.
func assertWorkingSetClean(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var statusRows int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_status`).Scan(&statusRows); err != nil {
		t.Fatalf("read dolt_status: %v", err)
	}
	if statusRows != 0 {
		t.Fatalf("working set not clean: %d dolt_status row(s) (unstaged/staged/conflicted)", statusRows)
	}
	var conflicts int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_conflicts`).Scan(&conflicts); err != nil {
		t.Fatalf("read dolt_conflicts: %v", err)
	}
	if conflicts != 0 {
		t.Fatalf("working set holds %d table(s) with merge conflicts", conflicts)
	}
}

// seedOldSchemaRemote creates one issue at root, downgrades the store below the
// resolution migration, then pushes — so the remote head is a pre-migration
// commit. Returns the issue id.
func seedOldSchemaRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(seed %s): %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	if err := st.Downgrade(ctx, baselineVersion+1); err != nil {
		t.Fatalf("Downgrade(seed) to v%d: %v", baselineVersion+1, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed): %v", err)
	}
	if _, err := sync.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(seed sync): %v", err)
	}
	return issue.ID
}

// advanceOldSchemaRemote applies a field edit at root (which migrates the store
// forward to edit), downgrades back below the resolution migration, and pushes —
// so the remote advances to a SECOND pre-migration commit that diverges from the
// base. This is the old-binary machine writing a stale-schema commit.
func advanceOldSchemaRemote(t *testing.T, ctx context.Context, root, id string, in UpdateIssueInput) {
	t.Helper()
	updateLocal(t, ctx, root, id, in) // Open() migrates to registry max, then edits
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(advance %s): %v", root, err)
	}
	if err := st.Downgrade(ctx, baselineVersion+1); err != nil {
		t.Fatalf("Downgrade(advance) to v%d: %v", baselineVersion+1, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(advance): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(advance): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(advance push): %v", err)
	}
}
