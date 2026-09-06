package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

// TestSyncPullSurfacesIDCollision drives the same planted defect through `lit sync
// pull`'s engine path. The pull maps every reconcile outcome to its own state, and
// an unmapped one is a returned error — so without this the operator's explicit
// pull answers a collision with "unhandled reconcile state", which reads as a bug
// in lit rather than as two tickets wearing one id.
func TestSyncPullSurfacesIDCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	epicID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	createChildLocally(t, ctx, rootA, "wsA", epicID, "Wire the release watchdog",
		"alarm when a pending release sits past its window")
	pushRootOrFatal(t, ctx, rootA)
	theirsID := createChildLocally(t, ctx, rootB, "wsB", epicID, "Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues")

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	headBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncPull(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncPull(B) returned an error instead of a collision state: %v", err)
	}
	if res.State != storage.SyncPullIDCollision {
		t.Fatalf("pull state = %q, want %q", res.State, storage.SyncPullIDCollision)
	}
	if len(res.Collisions) != 1 || res.Collisions[0].IssueID != theirsID {
		t.Fatalf("pull collisions = %+v, want the one colliding id %q", res.Collisions, theirsID)
	}
	if res.Collisions[0].Theirs.Title != "Wire the release watchdog" {
		t.Fatalf("theirs title = %q; the pull payload is the only place B sees A's ticket", res.Collisions[0].Theirs.Title)
	}
	if after := headCommit(t, ctx, syncB); after != headBefore {
		t.Fatalf("data branch moved from %s to %s; a refused pull must commit nothing", headBefore, after)
	}
}

// seedUnrelatedCollision builds two stores with NO shared history that each carry
// one id naming a DIFFERENT job. A's issue is planted on B under A's id but with
// B's own text and its own birth certificate — the shape two independently created
// backlogs reach on their own, and the one the combine resolution must refuse
// rather than union.
func seedUnrelatedCollision(t *testing.T, ctx context.Context, rootA, rootB, remoteURL string) string {
	t.Helper()
	stA, err := Open(ctx, rootA, "wsA")
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	theirs, err := stA.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Wire the release watchdog", Topic: "collide", IssueType: "task",
		Description: "alarm when a pending release sits past its window",
	})
	if err != nil {
		t.Fatalf("CreateIssue(A): %v", err)
	}
	exportA, err := stA.Export(ctx)
	if err != nil {
		t.Fatalf("Export(A): %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	if err := syncA.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(A): %v", err)
	}
	if _, err := syncA.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(A): %v", err)
	}
	if err := syncA.Close(); err != nil {
		t.Fatalf("Close(A sync): %v", err)
	}

	stB, err := Open(ctx, rootB, "wsB")
	if err != nil {
		t.Fatalf("Open(B): %v", err)
	}
	exportB, err := stB.Export(ctx)
	if err != nil {
		t.Fatalf("Export(B): %v", err)
	}
	planted := issueByID(t, exportA, theirs.ID)
	planted.Title = "Adaptive id length for large backlogs"
	planted.Description = "hash ids grow a character past 4k issues"
	planted.CreatedAt = planted.CreatedAt.Add(-90 * time.Minute)
	exportB.Issues = append(exportB.Issues, planted)
	if err := stB.replaceFromExport(ctx, exportB, commitStamp{Message: "plant B's own job under A's id"}); err != nil {
		t.Fatalf("replaceFromExport(B): %v", err)
	}
	if err := stB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncBSetup := openSyncOrFatal(t, ctx, rootB)
	if err := syncBSetup.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(B): %v", err)
	}
	if err := syncBSetup.Close(); err != nil {
		t.Fatalf("Close(B setup): %v", err)
	}
	return theirs.ID
}

// TestSyncReconcileCombineRefusesIDCollision covers the OTHER producer of the
// merge-and-replay tail. Combine runs with an empty base, so every shared id
// arrives base-less and the birth certificate is the only thing separating one
// replicated ticket from two independently minted ones — which makes two stores
// that never shared history the most realistic way to reach this at all. The union
// must refuse the pair and commit nothing, exactly as the three-way does.
func TestSyncReconcileCombineRefusesIDCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	collidingID := seedUnrelatedCollision(t, ctx, rootA, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcileCombine(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcileCombine(B): %v", err)
	}
	if res.State != storage.SyncReconcileIDCollision {
		t.Fatalf("combine state = %q (pending=%d), want %q: the union must not fuse two tickets under one id",
			res.State, len(res.Pending), storage.SyncReconcileIDCollision)
	}
	if len(res.Collisions) != 1 || res.Collisions[0].IssueID != collidingID {
		t.Fatalf("combine collisions = %+v, want the one colliding id %q", res.Collisions, collidingID)
	}
	c := res.Collisions[0]
	if c.Ours.Title != "Adaptive id length for large backlogs" || c.Theirs.Title != "Wire the release watchdog" {
		t.Fatalf("collision sides = {%q, %q}; both jobs must travel whole", c.Ours.Title, c.Theirs.Title)
	}
	if after := headCommit(t, ctx, syncB); after != headBefore {
		t.Fatalf("data branch moved from %s to %s; a refused combine must commit nothing", headBefore, after)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	local := getIssueOrFatal(t, ctx, syncB, collidingID)
	if local.Title != "Adaptive id length for large backlogs" {
		t.Fatalf("local row = %q; B's ticket must survive the refusal unmodified", local.Title)
	}
}
