package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSyncReconcileDetectsUnrelatedHistories drives the ticket's acceptance: two
// stores created INDEPENDENTLY (disjoint bootstrap roots) that share a remote and
// diverge have no common ancestor, so DOLT_MERGE_BASE yields nothing. Reconcile
// must DETECT that as a first-class unrelated-histories state and commit nothing —
// never crash on the absent merge-base (the pre-fix behavior was an obscure
// "sql: no rows in result set" surfaced from the base-assuming path). The
// shared-ancestor divergence still reconciling is proved by
// TestSyncReconcileLinearizesDivergenceAndFastForwardPushes.
func TestSyncReconcileDetectsUnrelatedHistories(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	// A seeds the remote with its own bootstrap root; B forks an UNRELATED clone —
	// its own bootstrap root, its own local issue, pointed at the same remote but
	// never adopting its head — so the two histories share no commit.
	seedReconcileRemote(t, ctx, rootA, remoteURL)
	forkUnrelatedClone(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	// Precondition: B is genuinely diverged (commits on both sides), so control would
	// enter the three-way path if detection did not intercept it.
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != SyncDiverged {
		t.Fatalf("precondition: freshness = %q ahead=%d behind=%d, want diverged", fresh.State(), fresh.Ahead, fresh.Behind)
	}

	// Capture the local head before reconcile: the no-partial-write property means the
	// data branch must be byte-for-byte where it started.
	headBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B) errored instead of classifying unrelated histories: %v", err)
	}
	if res.State != SyncReconcileUnrelated {
		t.Fatalf("reconcile state = %q, want %q", res.State, SyncReconcileUnrelated)
	}

	// No partial write: the data branch never moved, no scratch branch was left, and
	// the working set is clean — detection stops before any of that.
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved during unrelated-histories reconcile: %s -> %s", headBefore, got)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)

	// Nothing merged: B is still diverged, so the epic's later resolutions have the
	// live divergence to act on.
	after, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after reconcile: %v", err)
	}
	if after.State() != SyncDiverged {
		t.Fatalf("post-reconcile freshness = %q, want still diverged (nothing merged)", after.State())
	}
}

// forkUnrelatedClone creates a store at root with its OWN bootstrap root (never
// adopting the remote's history), seeds one local issue, points it at remoteURL,
// and leaves it configured to fetch. Unlike adoptRemote, it never resets to the
// remote head, so the store's history is disjoint from the remote's — the
// unrelated-histories scenario. It returns the local-only issue's id.
func forkUnrelatedClone(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(fork %s): %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "fork-local", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(fork): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(fork): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(fork): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(fork sync): %v", err)
	}
	return issue.ID
}
