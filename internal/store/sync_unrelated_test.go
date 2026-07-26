package store

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
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
	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)

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

	// The unrelated state carries the both-sides inventory read off the two anchors:
	// B's local issue is only-local, A's pushed issue is only-remote, and — since the
	// two stores' ids are independently generated — nothing is on both.
	if res.Unrelated == nil {
		t.Fatalf("unrelated reconcile carried no both-sides inventory")
	}
	assertIDSet(t, "only-local", res.Unrelated.OnlyLocal, []string{localIssueID})
	assertIDSet(t, "only-remote", res.Unrelated.OnlyRemote, []string{remoteIssueID})
	assertIDSet(t, "on-both", res.Unrelated.OnBoth, nil)

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

// TestSyncReconcileUnrelatedInventoryPartitionsAllThreeSides drives the ticket's
// acceptance criterion at full strength: two disjoint stores whose issue sets
// overlap in one id must partition into a non-empty only-local, only-remote, AND
// on-both. Independently generated ids never collide, so the shared id is planted
// on B verbatim from A's export — the one way an unrelated pair can genuinely hold
// the same issue (e.g. the same logical ticket filed in both stores). The
// reconcile reads both anchors AS OF and reports the correct three-way partition.
func TestSyncReconcileUnrelatedInventoryPartitionsAllThreeSides(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	// A (the remote side): a remote-only issue plus one it will share with B.
	stA, err := Open(ctx, rootA, "ws")
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	remoteOnly, err := stA.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "remote-only", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(A remote-only): %v", err)
	}
	shared, err := stA.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "shared", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(A shared): %v", err)
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

	// B (the local side, disjoint bootstrap root): a local-only issue, then plant A's
	// shared issue verbatim so the same id lands on BOTH sides. replaceFromExport
	// rewrites the whole issues table, so the combined export keeps B's local-only
	// issue and adds A's shared one.
	stB, err := Open(ctx, rootB, "ws")
	if err != nil {
		t.Fatalf("Open(B): %v", err)
	}
	localOnly, err := stB.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "local-only", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(B local-only): %v", err)
	}
	exportB, err := stB.Export(ctx)
	if err != nil {
		t.Fatalf("Export(B): %v", err)
	}
	combined := exportB
	combined.Issues = append(combined.Issues, issueByID(t, exportA, shared.ID))
	if err := stB.replaceFromExport(ctx, combined, "plant shared issue"); err != nil {
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

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != SyncReconcileUnrelated {
		t.Fatalf("reconcile state = %q, want %q", res.State, SyncReconcileUnrelated)
	}
	if res.Unrelated == nil {
		t.Fatalf("unrelated reconcile carried no both-sides inventory")
	}
	assertIDSet(t, "only-local", res.Unrelated.OnlyLocal, []string{localOnly.ID})
	assertIDSet(t, "only-remote", res.Unrelated.OnlyRemote, []string{remoteOnly.ID})
	assertIDSet(t, "on-both", res.Unrelated.OnBoth, []string{shared.ID})
}

// issueByID returns the issue with id from an export, failing the test if absent —
// a missing planted issue would silently weaken the partition assertion.
func issueByID(t *testing.T, export model.Export, id string) model.Issue {
	t.Helper()
	for _, issue := range export.Issues {
		if issue.ID == id {
			return issue
		}
	}
	t.Fatalf("issue %q not found in export", id)
	return model.Issue{}
}

// assertIDSet fails unless got holds exactly want, order-independent. Both are
// sorted first so the comparison is over set membership, not ordering.
func assertIDSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gotSorted := append([]string(nil), got...)
	wantSorted := append([]string(nil), want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	if len(gotSorted) == 0 && len(wantSorted) == 0 {
		return
	}
	if !reflect.DeepEqual(gotSorted, wantSorted) {
		t.Fatalf("%s partition = %v, want %v", label, gotSorted, wantSorted)
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
