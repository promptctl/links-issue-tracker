package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestSyncReconcileLinearizesDivergenceAndFastForwardPushes drives the ticket's
// acceptance: a two-clone file-remote scenario where both sides edit DIFFERENT
// code-owned fields, the engine resolves everything, and reconcile produces
// LINEAR history (the reconcile commit has one parent — the remote head) that
// fast-forward pushes. A seeds; B diverges and reconciles.
func TestSyncReconcileLinearizesDivergenceAndFastForwardPushes(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	// A edits LANE and pushes; B edits PRIORITY locally (unpushed) — two different
	// code-owned fields on the same issue, so B is diverged (ahead 1 / behind 1).
	updateAndPush(t, ctx, rootA, id, UpdateIssueInput{Lane: strptr("alpha")})
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Priority: intptr(model.PriorityUrgent)})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q (pending=%v), want %q", res.State, res.Pending, SyncReconcileLinearized)
	}

	// Both edits converged: A's lane AND B's priority survive on the merged row.
	merged := getIssueOrFatal(t, ctx, syncB, id)
	if merged.Lane != "alpha" {
		t.Fatalf("merged lane = %q, want alpha (A's edit lost)", merged.Lane)
	}
	if merged.Priority != model.PriorityUrgent {
		t.Fatalf("merged priority = %d, want urgent (B's edit lost)", merged.Priority)
	}

	// History is linear: the reconcile head commit has exactly one parent (the
	// remote head), not two — no merge commit. The branch is one ahead / zero
	// behind, so the push fast-forwards.
	assertSingleParentHead(t, ctx, syncB, res.RemoteHead)
	assertScratchBranchCleanedUp(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after reconcile: %v", err)
	}
	if fresh.State() != SyncAhead || fresh.Ahead != 1 {
		t.Fatalf("post-reconcile freshness = %q ahead=%d behind=%d, want ahead/1/0", fresh.State(), fresh.Ahead, fresh.Behind)
	}

	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after reconcile: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}

	// A receives the reconciled head by a pure fast-forward and sees both edits.
	syncA := openSyncOrFatal(t, ctx, rootA)
	recv, err := syncA.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != SyncReceiveFastForwarded {
		t.Fatalf("A receive state = %q, want fast_forwarded", recv.State)
	}
	convergedOnA := getIssueOrFatal(t, ctx, syncA, id)
	if convergedOnA.Lane != "alpha" || convergedOnA.Priority != model.PriorityUrgent {
		t.Fatalf("A after receive: lane=%q priority=%d, want alpha/urgent", convergedOnA.Lane, convergedOnA.Priority)
	}
	if err := syncA.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
}

// TestSyncReconcileHoldsProseDivergenceForAgent proves the second half: a
// concurrent free-text rewrite (both sides rewrite the title to different text)
// leaves a prose-pending state — nothing committed, the local branch untouched
// and still diverged — that the agent surface consumes.
func TestSyncReconcileHoldsProseDivergenceForAgent(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// Capture the local head before reconcile: the prose-pending path reads the
	// three-way state on a scratch branch and commits nothing, so the data branch
	// must be byte-for-byte where it started.
	headBefore := headCommit(t, ctx, syncB)
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != SyncReconcileProsePending {
		t.Fatalf("reconcile state = %q, want %q", res.State, SyncReconcileProsePending)
	}
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved during prose-pending reconcile: head %s -> %s (scratch reads leaked onto the live branch)", headBefore, got)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	if len(res.Pending) != 1 {
		t.Fatalf("pending prose count = %d, want 1: %+v", len(res.Pending), res.Pending)
	}
	p := res.Pending[0]
	if p.IssueID != id || p.Field != "title" {
		t.Fatalf("pending = %+v, want issue=%s field=title", p, id)
	}
	if p.Ours != "B's rewritten title" || p.Theirs != "A's rewritten title" {
		t.Fatalf("pending ours=%q theirs=%q, want B's/A's rewritten title", p.Ours, p.Theirs)
	}

	// Nothing committed: B's branch keeps its own title and is still diverged, so
	// the agent surface (ttde.4) has live three-way state to merge.
	local := getIssueOrFatal(t, ctx, syncB, id)
	if local.Title != "B's rewritten title" {
		t.Fatalf("local title after prose-pending reconcile = %q, want B's (untouched)", local.Title)
	}
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != SyncDiverged {
		t.Fatalf("post-prose-pending freshness = %q, want still diverged", fresh.State())
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
}

// TestSyncReconcileResolvedFinalizesWithAgentText proves the finalize boundary:
// after a prose divergence the agent supplies merged text, and SyncReconcileResolved
// re-derives the same divergence, splices the text in, and replays it as ONE
// forward commit on the remote head — linear history the next push fast-forwards.
func TestSyncReconcileResolvedFinalizesWithAgentText(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// Read the live conflict to obtain the fingerprint the agent would copy from the
	// surface, then finalize against it.
	pendingRes, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if len(pendingRes.Pending) != 1 {
		t.Fatalf("expected one pending field, got %+v", pendingRes.Pending)
	}
	res, err := syncB.SyncReconcileResolved(ctx, "origin", "master", []merge.ProseResolution{
		{IssueID: id, Field: merge.ProseTitle, Fingerprint: pendingRes.Pending[0].Fingerprint(), Text: "both A's and B's intent merged"},
	})
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != SyncReconcileLinearized {
		t.Fatalf("resolved state = %q, want %q", res.State, SyncReconcileLinearized)
	}
	got := getIssueOrFatal(t, ctx, syncB, id)
	if got.Title != "both A's and B's intent merged" {
		t.Fatalf("title after finalize = %q, want the agent's merged text", got.Title)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != SyncAhead {
		t.Fatalf("post-finalize freshness = %q, want ahead (linear, fast-forward-pushable)", fresh.State())
	}
}

// TestSyncReconcileResolvedRejectsStaleResolutions proves the safety gate: a
// resolution that does not match the live divergence (here, for a field that is
// not pending) commits nothing and re-surfaces the CURRENT prose conflicts.
// [LAW:no-silent-failure] the agent can never overwrite a field whose divergence
// changed underneath it.
func TestSyncReconcileResolvedRejectsStaleResolutions(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)
	// Only the title diverged; resolving the description is a stale/mismatched set.
	res, err := syncB.SyncReconcileResolved(ctx, "origin", "master", []merge.ProseResolution{
		{IssueID: id, Field: merge.ProseDescription, Text: "wrong field"},
	})
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != SyncReconcileProsePending {
		t.Fatalf("stale-resolution state = %q, want %q (re-surfaced)", res.State, SyncReconcileProsePending)
	}
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved on a rejected finalize: %s -> %s", headBefore, got)
	}
	if len(res.Pending) != 1 || res.Pending[0].Field != "title" {
		t.Fatalf("re-surfaced pending = %+v, want the live title conflict", res.Pending)
	}
	got := getIssueOrFatal(t, ctx, syncB, id)
	if got.Title != "B's rewritten title" {
		t.Fatalf("local title after rejected finalize = %q, want B's (untouched)", got.Title)
	}
}

// --- helpers ---

func strptr(s string) *string { return &s }
func intptr(i int) *int       { return &i }

// seedReconcileRemote creates one issue at root, adds the remote, pushes it, and
// returns the issue id.
func seedReconcileRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(seed %s): %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
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

// adoptRemote points a fresh clone at the remote and resets to its head.
func adoptRemote(t *testing.T, ctx context.Context, root, remoteURL string) {
	t.Helper()
	sync := openSyncOrFatal(t, ctx, root)
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(adopt): %v", err)
	}
	if err := sync.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(adopt): %v", err)
	}
	if err := sync.SyncResetToRemoteHead(ctx, "origin", "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(adopt): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(adopt): %v", err)
	}
}

// updateLocal applies a field update to an issue and leaves it local (unpushed).
func updateLocal(t *testing.T, ctx context.Context, root, id string, in UpdateIssueInput) {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(update %s): %v", root, err)
	}
	if _, err := st.Apply(ctx, id, Change{Fields: in}); err != nil {
		t.Fatalf("Apply(%s): %v", id, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(update): %v", err)
	}
}

// updateAndPush applies a field update and pushes it to the remote.
func updateAndPush(t *testing.T, ctx context.Context, root, id string, in UpdateIssueInput) {
	t.Helper()
	updateLocal(t, ctx, root, id, in)
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(update): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(update push): %v", err)
	}
}

func openSyncOrFatal(t *testing.T, ctx context.Context, root string) *Store {
	t.Helper()
	sync, err := OpenSync(ctx, root, "ws")
	if err != nil {
		t.Fatalf("OpenSync(%s): %v", root, err)
	}
	return sync
}

func headCommit(t *testing.T, ctx context.Context, st *Store) string {
	t.Helper()
	head, err := readDoltHead(ctx, st.db)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	return head
}

// assertScratchBranchCleanedUp fails if the reconcile left any throwaway scratch
// branch behind, and confirms the session is back on the data branch.
func assertScratchBranchCleanedUp(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_branches WHERE name LIKE ?`, reconcileScratchPrefix+"-%").Scan(&count); err != nil {
		t.Fatalf("count scratch branches: %v", err)
	}
	if count != 0 {
		t.Fatalf("reconcile left %d scratch branch(es) under %q behind", count, reconcileScratchPrefix)
	}
	branch, err := activeBranch(ctx, st.db)
	if err != nil {
		t.Fatalf("read active branch: %v", err)
	}
	if strings.HasPrefix(branch, reconcileScratchPrefix) {
		t.Fatalf("session left on scratch branch %q after reconcile", branch)
	}
}

func getIssueOrFatal(t *testing.T, ctx context.Context, st *Store, id string) model.Issue {
	t.Helper()
	issue, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", id, err)
	}
	return issue
}

// assertSingleParentHead fails unless the current HEAD commit has exactly one
// parent, and that parent is the remote head — i.e. the reconcile replayed onto
// the remote head linearly with no merge commit.
func assertSingleParentHead(t *testing.T, ctx context.Context, st *Store, remoteHead string) {
	t.Helper()
	head, err := readDoltHead(ctx, st.db)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	rows, err := st.db.QueryContext(ctx, `SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ?`, head)
	if err != nil {
		t.Fatalf("read commit ancestors: %v", err)
	}
	defer rows.Close()
	var parents []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan parent: %v", err)
		}
		parents = append(parents, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate parents: %v", err)
	}
	if len(parents) != 1 {
		t.Fatalf("reconcile head %s has %d parents %v, want 1 (no merge commit)", head, len(parents), parents)
	}
	if parents[0] != remoteHead {
		t.Fatalf("reconcile head parent = %s, want remote head %s", parents[0], remoteHead)
	}
}
