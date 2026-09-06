package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// createChildLocally adds one child under parentID and leaves it uncommitted to
// any remote — the id it gets is whatever the LOCAL store computes, which is the
// whole point: nothing coordinates the number across machines.
func createChildLocally(t *testing.T, ctx context.Context, root, workspace, parentID, title, description string) string {
	t.Helper()
	st, err := Open(ctx, root, workspace)
	if err != nil {
		t.Fatalf("Open(%s): %v", root, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%s): %v", root, err)
		}
	}()
	child, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Description: description, Topic: "collide",
		IssueType: "task", ParentID: parentID, Placement: storage.RankBottom,
	})
	if err != nil {
		t.Fatalf("CreateIssue(child on %s): %v", root, err)
	}
	return child.ID
}

// pushRootOrFatal sends a root's committed local work to the shared remote.
func pushRootOrFatal(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(%s): %v", root, err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(%s push): %v", root, err)
	}
}

// TestSyncReconcileRefusesIDCollisionAndCommitsNothing is the ticket's defect
// driven end to end through two real stores, the real id minter, and the real
// reconcile — not a hand-built export. Both clones hold one epic; each files its
// own next child while disconnected; both minters count the epic's children
// locally and both hand out the SAME id. Nothing races: the number is computed,
// deterministically, from what each store can see.
//
// The reconcile must refuse rather than field-merge the pair into one row, and it
// must leave the branch where it found it so the clone keeps working on its own
// truth.
func TestSyncReconcileRefusesIDCollisionAndCommitsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	epicID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	// A files its child and pushes; B, still disconnected, files a DIFFERENT job.
	oursID := createChildLocally(t, ctx, rootA, "wsA", epicID, "Wire the release watchdog",
		"alarm when a pending release sits past its window")
	pushRootOrFatal(t, ctx, rootA)
	theirsID := createChildLocally(t, ctx, rootB, "wsB", epicID, "Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues")

	// The premise, asserted rather than assumed: two disconnected stores minted
	// one id for two unrelated jobs, with nothing racing.
	if oursID != theirsID {
		t.Fatalf("the two stores minted %q and %q; this test's premise is that a locally-counted child number collides", oursID, theirsID)
	}

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileIDCollision {
		t.Fatalf("reconcile state = %q, want %q: two independently created tickets under one id must not be field-merged",
			res.State, storage.SyncReconcileIDCollision)
	}
	if len(res.Collisions) != 1 {
		t.Fatalf("collisions = %d, want 1", len(res.Collisions))
	}
	c := res.Collisions[0]
	if c.IssueID != theirsID {
		t.Fatalf("collision id = %q, want %q", c.IssueID, theirsID)
	}
	// Both jobs reach the operator whole — the report is the ONLY place B can see
	// A's ticket, since nothing was merged into B's store.
	if c.Ours.Title != "Adaptive id length for large backlogs" {
		t.Fatalf("ours title = %q, want B's own job (ours is the local side)", c.Ours.Title)
	}
	if c.Theirs.Title != "Wire the release watchdog" {
		t.Fatalf("theirs title = %q, want A's job carried through the report", c.Theirs.Title)
	}

	// Nothing committed: the data branch is exactly where it was, so B's clone is
	// still usable and still diverged. [LAW:no-silent-failure]
	if after := headCommit(t, ctx, syncB); after != headBefore {
		t.Fatalf("data branch moved from %s to %s; a refused merge must commit nothing", headBefore, after)
	}
	// B's own ticket is untouched — not blended with A's.
	local := getIssueOrFatal(t, ctx, syncB, theirsID)
	if local.Title != "Adaptive id length for large backlogs" || local.Description != "hash ids grow a character past 4k issues" {
		t.Fatalf("local row = {%q, %q}; B's ticket must survive the refusal unmodified", local.Title, local.Description)
	}
}
