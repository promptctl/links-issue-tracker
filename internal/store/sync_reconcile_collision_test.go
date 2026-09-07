package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
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

// retagIssueLocally rewrites one issue's id in place, carrying every edge,
// label, comment and event that names it. It plants the premise these tests
// need: two stores holding one id for two unrelated jobs.
//
// The minter no longer produces that pair by itself — links-multi-machine-qn6x
// made child ids content-hashed, so two disconnected stores mint different ids
// for different work. Colliding pairs still reach reconcile from the field:
// every child minted before that change carries a locally-counted number, and
// an import or a restore writes whatever ids its file names. Reconcile refuses
// two rows under one id however they came to share it, so these tests state the
// shared id outright rather than leaning on a minter that once produced it by
// accident. [LAW:behavior-not-structure]
func retagIssueLocally(t *testing.T, ctx context.Context, root, workspace, oldID, newID string) {
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
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export(%s): %v", root, err)
	}
	swap := func(id string) string {
		if id == oldID {
			return newID
		}
		return id
	}
	found := false
	for i := range export.Issues {
		if export.Issues[i].ID == oldID {
			found = true
		}
		export.Issues[i].ID = swap(export.Issues[i].ID)
	}
	if !found {
		t.Fatalf("retagIssueLocally: %q is not in %s's export", oldID, root)
	}
	for i := range export.Relations {
		export.Relations[i].SrcID = swap(export.Relations[i].SrcID)
		export.Relations[i].DstID = swap(export.Relations[i].DstID)
	}
	for i := range export.Comments {
		export.Comments[i].IssueID = swap(export.Comments[i].IssueID)
	}
	for i := range export.Labels {
		export.Labels[i].IssueID = swap(export.Labels[i].IssueID)
	}
	for i := range export.Events {
		export.Events[i].IssueID = swap(export.Events[i].IssueID)
	}
	if err := st.ReplaceFromExport(ctx, export); err != nil {
		t.Fatalf("ReplaceFromExport(%s): %v", root, err)
	}
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

	// The premise, stated outright: the two stores hold one id for two unrelated
	// jobs. B's row is retagged onto A's id because the minter no longer produces
	// the pair; a store carrying pre-hash children, or one restored from an
	// import, arrives at reconcile in exactly this state.
	retagIssueLocally(t, ctx, rootB, "wsB", theirsID, oursID)
	theirsID = oursID

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

// TestTwoDisconnectedStoresMintDistinctChildIDs is this ticket's defect driven
// end to end through two real stores and the real minter — the prevention half
// of what TestSyncReconcileRefusesIDCollisionAndCommitsNothing detects.
//
// Both clones hold the same epic and neither can see the other's work. Under the
// old rule each counted the epic's children locally and each handed out the same
// next number: nothing raced, the number was simply computed from a view that
// was only ever partial. The ids must now differ, and reconcile must carry both
// tickets through rather than refusing.
func TestTwoDisconnectedStoresMintDistinctChildIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	epicID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	oursID := createChildLocally(t, ctx, rootA, "wsA", epicID, "Wire the release watchdog",
		"alarm when a pending release sits past its window")
	pushRootOrFatal(t, ctx, rootA)
	theirsID := createChildLocally(t, ctx, rootB, "wsB", epicID, "Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues")

	if oursID == theirsID {
		t.Fatalf("both stores minted %q for two unrelated jobs; a child id must not be a locally-counted position", oursID)
	}
	for _, id := range []string{oursID, theirsID} {
		if !strings.HasPrefix(id, epicID+".") {
			t.Fatalf("child id = %q, want it to hang under the epic %q", id, epicID)
		}
	}

	// The pair now merges instead of colliding, which is the whole point: two
	// machines filing different work under one epic is ordinary, not a conflict.
	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State == storage.SyncReconcileIDCollision {
		t.Fatalf("reconcile refused a collision between %q and %q; distinct ids must not collide", oursID, theirsID)
	}
	for _, id := range []string{oursID, theirsID} {
		if _, err := syncB.GetIssue(ctx, id); err != nil {
			t.Fatalf("GetIssue(%s) after reconcile: %v; both machines' tickets must survive", id, err)
		}
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

	theirsID := createChildLocally(t, ctx, rootA, "wsA", epicID, "Wire the release watchdog",
		"alarm when a pending release sits past its window")
	pushRootOrFatal(t, ctx, rootA)
	oursID := createChildLocally(t, ctx, rootB, "wsB", epicID, "Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues")
	retagIssueLocally(t, ctx, rootB, "wsB", oursID, theirsID)

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

// removeIssueLocally hard-removes one issue in its own local commit, through the
// export replace an ordinary `lit import` bottoms out in — the only path that
// deletes an issues row. Afterwards the id is absent from local head and present
// only in the commit above it.
func removeIssueLocally(t *testing.T, ctx context.Context, root, workspace, id string) {
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
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export(%s): %v", root, err)
	}
	export.Issues = filterRows(export.Issues, func(i model.Issue) bool { return i.ID != id })
	export.Relations = filterRows(export.Relations, func(r model.Relation) bool { return r.SrcID != id && r.DstID != id })
	export.Comments = filterRows(export.Comments, func(c model.Comment) bool { return c.IssueID != id })
	export.Labels = filterRows(export.Labels, func(l model.Label) bool { return l.IssueID != id })
	export.Events = filterRows(export.Events, func(e model.IssueEvent) bool { return e.IssueID != id })
	if err := st.replaceFromExport(ctx, export, commitStamp{Message: "drop the locally filed child"}); err != nil {
		t.Fatalf("replaceFromExport(%s): %v", root, err)
	}
}

// TestSyncReconcileRefusesIDCollisionFoundInFoldedCommit drives the collision the
// HEAD-level classification cannot see. B files its own `<epic>.1`, then hard-removes
// it in a later local commit, so local head no longer holds the id while the folded
// commit under it still does — and the remote carries a DIFFERENT ticket under that
// same id. merge.ThreeWay(base, ours@localHead, theirs) therefore sees a clean
// remote-only add and raises nothing; the pair meets Classify for the first and only
// time inside the fold replay.
//
// The refusal there must reach the operator as the SAME outcome a head-detected one
// does — the collision state carrying both rows — not as an opaque replay error, or
// every surface built on it renders an internal failure instead of the block, the
// guidance and the owner notification.
func TestSyncReconcileRefusesIDCollisionFoundInFoldedCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	epicID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	oursID := createChildLocally(t, ctx, rootA, "wsA", epicID, "Wire the release watchdog",
		"alarm when a pending release sits past its window")
	pushRootOrFatal(t, ctx, rootA)
	theirsID := createChildLocally(t, ctx, rootB, "wsB", epicID, "Adaptive id length for large backlogs",
		"hash ids grow a character past 4k issues")
	retagIssueLocally(t, ctx, rootB, "wsB", theirsID, oursID)
	theirsID = oursID
	removeIssueLocally(t, ctx, rootB, "wsB", theirsID)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// The premise of the FOLD half, asserted rather than assumed: the id is gone
	// from local head, so nothing the head-level check reads can classify it.
	if _, err := syncB.GetIssue(ctx, theirsID); err == nil {
		t.Fatalf("%q still present at local head; this test's premise is that only a folded commit holds it", theirsID)
	}
	headBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B) returned an error instead of a collision state: %v", err)
	}
	if res.State != storage.SyncReconcileIDCollision {
		t.Fatalf("reconcile state = %q, want %q: a collision found in the fold replay must surface like one found at the head",
			res.State, storage.SyncReconcileIDCollision)
	}
	if len(res.Collisions) != 1 || res.Collisions[0].IssueID != theirsID {
		t.Fatalf("collisions = %+v, want the one colliding id %q carried out of the fold step", res.Collisions, theirsID)
	}
	c := res.Collisions[0]
	if c.Ours.Title != "Adaptive id length for large backlogs" {
		t.Fatalf("ours title = %q, want B's own job as the folded commit held it", c.Ours.Title)
	}
	if c.Theirs.Title != "Wire the release watchdog" {
		t.Fatalf("theirs title = %q, want A's job carried through the report", c.Theirs.Title)
	}
	if len(res.Pending) != 0 {
		t.Fatalf("pending = %+v; a collision is not a divergence an agent can finish", res.Pending)
	}
	if after := headCommit(t, ctx, syncB); after != headBefore {
		t.Fatalf("data branch moved from %s to %s; a refused replay must commit nothing", headBefore, after)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
}
