package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// loopedSyncPair seeds a remote holding a seed epic and two top-level epics E
// and F, adopts it on a second clone, and returns both roots, the seed epic's
// id and the two epics. A clone that then stores a loop over E and F holds what
// an older lit, or a hand edit, could write; neither edge of that loop is in
// the shared history, so a cut the lift makes is the only one any landing sees.
func loopedSyncPair(t *testing.T, ctx context.Context) (rootA, rootB, seedID string, tree []model.Issue) {
	t.Helper()
	rootA = migratedDoltDir(t)
	rootB = unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	seedID = seedReconcileRemote(t, ctx, rootA, remoteURL)
	st, err := Open(ctx, testEnv(), testStderr(), rootA, "ws")
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	for _, title := range []string{"E", "F"} {
		tree = append(tree, parentChain(t, ctx, st, title)[0])
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
	push(t, ctx, rootA)
	adoptRemote(t, ctx, rootB, remoteURL)
	return rootA, rootB, seedID, tree
}

// plantLoop stores E under F and F under E, past the write boundary that
// refuses the second, and commits them, so the loop is in the clone's history.
func plantLoop(t *testing.T, ctx context.Context, root string, tree []model.Issue) {
	t.Helper()
	st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("Open(plant): %v", err)
	}
	seedParentEdge(t, ctx, st, tree[0].ID, tree[1].ID)
	seedParentEdge(t, ctx, st, tree[1].ID, tree[0].ID)
	if _, err := st.db.ExecContext(ctx, `CALL DOLT_COMMIT('-Am', 'plant a parent loop')`); err != nil {
		t.Fatalf("commit the planted loop: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(plant): %v", err)
	}
}

func push(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(%s): %v", root, err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(push): %v", err)
	}
}

// reconcileB fetches and reconciles clone B, which must linearize, and returns
// the result with B's store still open on the reconciled data branch.
func reconcileB(t *testing.T, ctx context.Context, rootB string) (storage.SyncReconcileResult, *Store) {
	t.Helper()
	syncB := openSyncOrFatal(t, ctx, rootB)
	t.Cleanup(func() { _ = syncB.Close() })
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		var refusal storage.ParentCycleError
		if errors.As(err, &refusal) {
			t.Fatalf("SyncReconcile(B) refused a loop in the history it read: %v", err)
		}
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	if _, err := syncB.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
		t.Fatalf("ListIssues(B) after the reconcile: %v, want the store readable", err)
	}
	return res, syncB
}

func sameCuts(a, b []storage.Unlooped) bool {
	return slices.EqualFunc(a, b, func(x, y storage.Unlooped) bool {
		return x.IssueID == y.IssueID && x.Parent == y.Parent && slices.Equal(x.Cycle, y.Cycle)
	})
}

// The cut a loop gets is worked by hand from the rule: the member with the
// greater id loses its edge onto the other, and the loop is named from the
// lesser.
func expectedCut(tree []model.Issue) (lo, hi string, want []storage.Unlooped) {
	lo, hi = min(tree[0].ID, tree[1].ID), max(tree[0].ID, tree[1].ID)
	return lo, hi, []storage.Unlooped{{IssueID: hi, Parent: lo, Cycle: []string{lo, hi}}}
}

// An older lit can push a loop. Every sync over that remote head reads it, and
// a read of a loop is refused, so the sync cuts it where it lifts the head and
// names the cut, rather than refusing the sync on every attempt.
func TestSyncReconcileCutsALoopTheRemoteHeadHolds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rootA, rootB, seedID, tree := loopedSyncPair(t, ctx)
	plantLoop(t, ctx, rootA, tree)
	push(t, ctx, rootA)
	updateLocal(t, ctx, rootB, seedID, storage.UpdateIssueInput{Priority: ptr(model.PriorityUrgent)})

	res, syncB := reconcileB(t, ctx, rootB)
	lo, hi, want := expectedCut(tree)
	if !sameCuts(res.Landing.Unlooped, want) {
		t.Errorf("Landing.Unlooped = %+v, want %+v", res.Landing.Unlooped, want)
	}
	if got := parentOf(t, ctx, syncB, tree); got[hi] != "" || got[lo] != hi {
		t.Errorf("parents after the reconcile = %v, want %s under %s and %s at the top level", got, lo, hi, hi)
	}
}

// A loop the local head holds lands through the merge, cut by the lift of
// the local head, and the sync names the cut it made.
func TestSyncReconcileCutsALoopTheLocalHeadHolds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rootA, rootB, seedID, tree := loopedSyncPair(t, ctx)
	updateAndPush(t, ctx, rootA, seedID, storage.UpdateIssueInput{Lane: strptr("alpha")})
	plantLoop(t, ctx, rootB, tree)

	res, syncB := reconcileB(t, ctx, rootB)
	lo, hi, want := expectedCut(tree)
	if !sameCuts(res.Landing.Unlooped, want) {
		t.Errorf("Landing.Unlooped = %+v, want %+v", res.Landing.Unlooped, want)
	}
	if got := parentOf(t, ctx, syncB, tree); got[hi] != "" || got[lo] != hi {
		t.Errorf("parents after the reconcile = %v, want %s under %s and %s at the top level", got, lo, hi, hi)
	}
}

// The repair a read's refusal names is made at the head, and a commit does
// not rewrite the history under it: the loop is still in a local commit the
// sync replays. The sync reads that commit, cuts it there, and lands the
// repaired head as the operator left it, with no cut to report, because the
// head the backlog settles on holds no loop.
func TestSyncReconcileReplaysALoopAnUnpushedCommitHeldAfterItsRepair(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rootA, rootB, seedID, tree := loopedSyncPair(t, ctx)
	updateAndPush(t, ctx, rootA, seedID, storage.UpdateIssueInput{Lane: strptr("alpha")})
	plantLoop(t, ctx, rootB, tree)
	st, err := Open(ctx, testEnv(), testStderr(), rootB, "ws")
	if err != nil {
		t.Fatalf("Open(B): %v", err)
	}
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: tree[0].ID, ParentID: seedID, CreatedBy: "test"}); err != nil {
		t.Fatalf("SetParent(%s under %s), the named repair: %v", tree[0].ID, seedID, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}

	res, syncB := reconcileB(t, ctx, rootB)
	if len(res.Landing.Unlooped) != 0 {
		t.Errorf("Landing.Unlooped = %+v, want none: the repaired head holds no loop", res.Landing.Unlooped)
	}
	if got := parentOf(t, ctx, syncB, tree); got[tree[0].ID] != seedID || got[tree[1].ID] != tree[0].ID {
		t.Errorf("parents after the reconcile = %v, want %s under %s and %s under %s, the repair as made", got, tree[0].ID, seedID, tree[1].ID, tree[0].ID)
	}
}
