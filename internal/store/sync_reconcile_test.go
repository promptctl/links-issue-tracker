package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestSyncReconcileLinearizesDivergenceAndFastForwardPushes drives the ticket's
// acceptance: a two-clone file-remote scenario where both sides edit DIFFERENT
// code-owned fields, the engine resolves everything, and reconcile produces
// LINEAR history (the reconcile commit has one parent — the remote head) that
// fast-forward pushes. A seeds; B diverges and reconciles.
func TestSyncReconcileLinearizesDivergenceAndFastForwardPushes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	// A edits LANE and pushes; B edits PRIORITY locally (unpushed) — two different
	// code-owned fields on the same issue, so B is diverged (ahead 1 / behind 1).
	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Lane: strptr("alpha")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Priority: ptr(model.PriorityUrgent)})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// B's folded side is its one "apply update" commit; capture its original
	// timestamp and author so the replay's provenance claim is checked against
	// them.
	originals := originalCommitsByMessage(t, ctx, syncB, "apply update")
	res, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q (pending=%v), want %q", res.State, res.Pending, storage.SyncReconcileLinearized)
	}

	// Both edits converged: A's lane AND B's priority survive on the merged row.
	merged := getIssueOrFatal(t, ctx, syncB, id)
	if merged.Lane != "alpha" {
		t.Fatalf("merged lane = %q, want alpha (A's edit lost)", merged.Lane)
	}
	if merged.Priority != model.PriorityUrgent {
		t.Fatalf("merged priority = %d, want urgent (B's edit lost)", merged.Priority)
	}

	// History is linear on the remote head with no merge commit, and the folded
	// side's provenance survived the fold: B's own commit landed under its
	// original message and timestamp, settled by the reconcile's marker commit.
	assertLinearSpineToRemoteHead(t, ctx, syncB, res.RemoteHead)
	if res.Replayed != 1 {
		t.Fatalf("replayed provenance commits = %d, want 1 (B's update)", res.Replayed)
	}
	spine := spineSince(t, ctx, syncB, res.RemoteHead)
	if len(spine) != 2 || spine[0].message != "apply update" || spine[1].message != reconcileCommitMessage {
		t.Fatalf("replayed spine = %+v, want [apply update, %q]", spine, reconcileCommitMessage)
	}
	original := originals["apply update"]
	if !spine[0].date.UTC().Equal(original.date.UTC().Truncate(time.Second)) {
		t.Fatalf("replayed commit date = %s, want the original %s (to the second)", spine[0].date.UTC(), original.date.UTC())
	}
	if spine[0].committer != original.committer || spine[0].email != original.email {
		t.Fatalf("replayed commit author = %s <%s>, want the original %s <%s>", spine[0].committer, spine[0].email, original.committer, original.email)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	// Property: the linearized outcome — the one that DOES mutate the data branch —
	// leaves a clean working set (no staged/unstaged residue, no held conflicts).
	assertWorkingSetClean(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after reconcile: %v", err)
	}
	if fresh.State() != storage.SyncAhead || fresh.Ahead != 2 {
		t.Fatalf("post-reconcile freshness = %q ahead=%d behind=%d, want ahead/2/0 (provenance commit + marker)", fresh.State(), fresh.Ahead, fresh.Behind)
	}

	if _, err := syncB.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after reconcile: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}

	// A receives the reconciled head by a pure fast-forward and sees both edits.
	syncA := openSyncOrFatal(t, ctx, rootA)
	recv, err := syncA.SyncReceive(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != storage.SyncReceiveFastForwarded {
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
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// Capture the local head before reconcile: the prose-pending path reads the
	// three-way state on a scratch branch and commits nothing, so the data branch
	// must be byte-for-byte where it started.
	headBefore := headCommit(t, ctx, syncB)
	res, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileProsePending {
		t.Fatalf("reconcile state = %q, want %q", res.State, storage.SyncReconcileProsePending)
	}
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved during prose-pending reconcile: head %s -> %s (scratch reads leaked onto the live branch)", headBefore, got)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	// Property: a prose-pending reconcile (the non-mutating outcome) also leaves a
	// clean working set — the scratch reads never leak staged/unstaged residue.
	assertWorkingSetClean(t, ctx, syncB)
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
	fresh, err := syncB.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != storage.SyncDiverged {
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
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	// Read the live conflict to obtain the fingerprint the agent would copy from the
	// surface, then finalize against it.
	pendingRes, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if len(pendingRes.Pending) != 1 {
		t.Fatalf("expected one pending field, got %+v", pendingRes.Pending)
	}
	res, err := syncB.SyncReconcileResolved(ctx, remoteName(t, "origin"), "master", []merge.ProseResolution{
		{Fingerprint: pendingRes.Pending[0].Fingerprint(), Text: "both A's and B's intent merged"},
	}, "judge")
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("resolved state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	got := getIssueOrFatal(t, ctx, syncB, id)
	if got.Title != "both A's and B's intent merged" {
		t.Fatalf("title after finalize = %q, want the agent's merged text", got.Title)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B): %v", err)
	}
	if fresh.State() != storage.SyncAhead {
		t.Fatalf("post-finalize freshness = %q, want ahead (linear, fast-forward-pushable)", fresh.State())
	}
}

// TestSyncReconcileResolvedRecordsTheMergedText drives a real reconcile in which
// all three prose fields diverged, and proves the agent's merged text reaches
// history the way an edit does: one event, by the resolving actor, with one
// change row per field from the provisional value (ours) to the merged text,
// stamped at the issue's new updated_at.
func TestSyncReconcileResolvedRecordsTheMergedText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Title: strptr("A title"), Description: strptr("A desc"), Prompt: strptr("A prompt")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Title: strptr("B title"), Description: strptr("B desc"), Prompt: strptr("B prompt")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	pendingRes, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if len(pendingRes.Pending) != 3 {
		t.Fatalf("pending = %+v, want title, description and prompt", pendingRes.Pending)
	}
	merged := map[merge.ProseField]string{
		merge.ProseTitle:       "A and B title",
		merge.ProseDescription: "A and B desc",
		merge.ProsePrompt:      "A and B prompt",
	}
	resolutions := make([]merge.ProseResolution, 0, len(pendingRes.Pending))
	for _, p := range pendingRes.Pending {
		resolutions = append(resolutions, merge.ProseResolution{Fingerprint: p.Fingerprint(), Text: merged[p.Field]})
	}
	res, err := syncB.SyncReconcileResolved(ctx, remoteName(t, "origin"), "master", resolutions, "judge")
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("resolved state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	if res.ProseRecorded != 3 {
		t.Errorf("ProseRecorded = %d, want 3", res.ProseRecorded)
	}

	events, err := syncB.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var recorded []model.IssueEvent
	for _, event := range events {
		if event.Reason == proseResolvedReason {
			recorded = append(recorded, event)
		}
	}
	if len(recorded) != 1 {
		t.Fatalf("events recording the merged text = %d, want 1: %+v", len(recorded), events)
	}
	event := recorded[0]
	if event.Actor != "judge" {
		t.Errorf("event actor = %q, want the resolving actor %q", event.Actor, "judge")
	}
	want := map[string]model.FieldChange{
		"title":       {Field: "title", From: "B title", To: "A and B title"},
		"description": {Field: "description", From: "B desc", To: "A and B desc"},
		"prompt":      {Field: "prompt", From: "B prompt", To: "A and B prompt"},
	}
	if len(event.Changes) != len(want) {
		t.Fatalf("event changes = %+v, want one row per resolved field: %+v", event.Changes, want)
	}
	for _, change := range event.Changes {
		if change != want[change.Field] {
			t.Errorf("change %q = %+v, want %+v", change.Field, change, want[change.Field])
		}
	}
	got := getIssueOrFatal(t, ctx, syncB, id)
	if got.Title != "A and B title" || got.Description != "A and B desc" || got.Prompt != "A and B prompt" {
		t.Fatalf("issue after finalize = %q / %q / %q, want the merged texts", got.Title, got.Description, got.Prompt)
	}
	if !got.UpdatedAt.Equal(event.CreatedAt) {
		t.Errorf("updated_at = %s, want the event's time %s", got.UpdatedAt, event.CreatedAt)
	}
}

// TestSyncReconcileResolvedRecordsNothingForTheProvisionalText is the other side
// of the record: an agent whose merge is the provisional text (ours) moved no
// field, so history gains no event, exactly as for an edit restating a value.
func TestSyncReconcileResolvedRecordsNothingForTheProvisionalText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Title: strptr("A title")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Title: strptr("B title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	pendingRes, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if len(pendingRes.Pending) != 1 {
		t.Fatalf("expected one pending field, got %+v", pendingRes.Pending)
	}
	res, err := syncB.SyncReconcileResolved(ctx, remoteName(t, "origin"), "master", []merge.ProseResolution{
		{Fingerprint: pendingRes.Pending[0].Fingerprint(), Text: "B title"},
	}, "judge")
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("resolved state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	if res.ProseRecorded != 0 {
		t.Errorf("ProseRecorded = %d, want 0", res.ProseRecorded)
	}
	events, err := syncB.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, event := range events {
		if event.Reason == proseResolvedReason {
			t.Fatalf("an unchanged merged text recorded an event: %+v", event)
		}
	}
}

// TestSyncReconcileResolvedRejectsStaleResolutions proves the safety gate: a
// resolution that does not match the live divergence (here, for a field that is
// not pending) commits nothing and re-surfaces the CURRENT prose conflicts.
// [LAW:no-silent-failure] the agent can never overwrite a field whose divergence
// changed underneath it.
func TestSyncReconcileResolvedRejectsStaleResolutions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	updateAndPush(t, ctx, rootA, id, storage.UpdateIssueInput{Title: strptr("A's rewritten title")})
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Title: strptr("B's rewritten title")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)
	// Only the title diverged; a resolution addressed to a description conflict names
	// no live conflict, so the set is stale.
	res, err := syncB.SyncReconcileResolved(ctx, remoteName(t, "origin"), "master", []merge.ProseResolution{
		{Fingerprint: merge.ProsePending{IssueID: id, Field: merge.ProseDescription}.Fingerprint(), Text: "wrong field"},
	}, "judge")
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B): %v", err)
	}
	if res.State != storage.SyncReconcileProsePending {
		t.Fatalf("stale-resolution state = %q, want %q (re-surfaced)", res.State, storage.SyncReconcileProsePending)
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

// seedReconcileRemote creates one epic at root, adds the remote, pushes it, and
// returns the epic's id. An epic, because the collision tests file children
// under it and only an epic contains tickets; every caller edits fields an
// epic carries (title, lane, priority, labels).
func seedReconcileRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("Open(seed %s): %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed): %v", err)
	}
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
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
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(adopt): %v", err)
	}
	if err := sync.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(adopt): %v", err)
	}
	if err := sync.SyncResetToRemoteHead(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncResetToRemoteHead(adopt): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(adopt): %v", err)
	}
}

// updateLocal applies a field update to an issue and leaves it local (unpushed).
func updateLocal(t *testing.T, ctx context.Context, root, id string, in storage.UpdateIssueInput) {
	t.Helper()
	st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("Open(update %s): %v", root, err)
	}
	if _, err := st.Apply(ctx, id, storage.Change{Fields: in}); err != nil {
		t.Fatalf("Apply(%s): %v", id, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(update): %v", err)
	}
}

// updateAndPush applies a field update and pushes it to the remote.
func updateAndPush(t *testing.T, ctx context.Context, root, id string, in storage.UpdateIssueInput) {
	t.Helper()
	updateLocal(t, ctx, root, id, in)
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(update): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(update push): %v", err)
	}
}

func openSyncOrFatal(t *testing.T, ctx context.Context, root string) *Store {
	t.Helper()
	sync, err := OpenSync(ctx, testEnv(), testStderr(), root, "ws")
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

// assertLinearSpineToRemoteHead fails unless the current HEAD descends from the
// remote head through single-parent commits only — the reconcile's history
// contract: the fold lands as a linear forward sequence on the remote head (the
// folded side's provenance commits, then the marker), never a merge commit and
// never a rewritten spine. The walk terminates on any history: a merge commit
// or the parentless root fails it before it can loop.
func assertLinearSpineToRemoteHead(t *testing.T, ctx context.Context, st *Store, remoteHead string) {
	t.Helper()
	current, err := readDoltHead(ctx, st.db)
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	for current != remoteHead {
		rows, err := st.db.QueryContext(ctx, `SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ?`, current)
		if err != nil {
			t.Fatalf("read commit ancestors: %v", err)
		}
		var parents []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				t.Fatalf("scan parent: %v", err)
			}
			parents = append(parents, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate parents: %v", err)
		}
		rows.Close()
		if len(parents) != 1 {
			t.Fatalf("commit %s on the replayed spine has %d parents %v, want 1 (linear descent from remote head %s)", current, len(parents), parents, remoteHead)
		}
		current = parents[0]
	}
}

// spineEntry is one commit of the replayed spine as the TESTS read it — via a
// direct dolt_log query, deliberately independent of the production
// readFoldedChain reader so a bug there cannot vouch for itself. Committer and
// email are kept as raw components (never re-joined through the production
// "name <email>" formatting) so author-preservation is asserted independently
// of how production spells the --author string.
type spineEntry struct {
	hash      string
	message   string
	committer string
	email     string
	date      time.Time
}

// spineSince lists the commits reachable from HEAD but not from exclusiveBase,
// oldest-first — the segment a reconcile's replay built on the remote head.
func spineSince(t *testing.T, ctx context.Context, st *Store, exclusiveBase string) []spineEntry {
	t.Helper()
	head := headCommit(t, ctx, st)
	spine := scanSpineEntries(t, ctx, st, exclusiveBase+".."+head)
	for i, j := 0, len(spine)-1; i < j; i, j = i+1, j-1 {
		spine[i], spine[j] = spine[j], spine[i]
	}
	return spine
}

// originalCommitsByMessage reads the commit (timestamp, committer, email) of
// each named message from the store's CURRENT branch history, requiring exactly
// one commit per message — an absent or duplicated message would silently
// weaken a provenance assertion built on it.
func originalCommitsByMessage(t *testing.T, ctx context.Context, st *Store, messages ...string) map[string]spineEntry {
	t.Helper()
	found := make(map[string][]spineEntry)
	for _, e := range scanSpineEntries(t, ctx, st, "HEAD") {
		found[e.message] = append(found[e.message], e)
	}
	originals := make(map[string]spineEntry, len(messages))
	for _, message := range messages {
		if len(found[message]) != 1 {
			t.Fatalf("history holds %d commits with message %q, want exactly 1", len(found[message]), message)
		}
		originals[message] = found[message][0]
	}
	return originals
}

// scanSpineEntries reads one dolt_log revision expression into spineEntry rows
// in dolt_log's own (newest-first) order.
func scanSpineEntries(t *testing.T, ctx context.Context, st *Store, revision string) []spineEntry {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, `SELECT commit_hash, committer, email, date, message FROM dolt_log(?)`, revision)
	if err != nil {
		t.Fatalf("read commit log %q: %v", revision, err)
	}
	defer rows.Close()
	var entries []spineEntry
	for rows.Next() {
		var e spineEntry
		if err := rows.Scan(&e.hash, &e.committer, &e.email, &e.date, &e.message); err != nil {
			t.Fatalf("scan log commit: %v", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate log %q: %v", revision, err)
	}
	return entries
}

// assertIssuePresentAsOf reads whether an issue row exists at a specific spine
// commit — how the tests see a mid-chain state without moving any branch. The
// commit hash comes from dolt_log (base32, trusted alphabet) and is
// interpolated because the engine does not accept a bound placeholder for AS OF.
func assertIssuePresentAsOf(t *testing.T, ctx context.Context, st *Store, commit, issueID string, want bool) {
	t.Helper()
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM issues AS OF '%s' WHERE id = ?", commit)
	if err := st.db.QueryRowContext(ctx, query, issueID).Scan(&count); err != nil {
		t.Fatalf("read issue %s AS OF %s: %v", issueID, commit, err)
	}
	if got := count == 1; got != want {
		t.Fatalf("issue %s present at %s = %v, want %v", issueID, commit, got, want)
	}
}

// TestReconcileScratchCleanupSurvivesCallerCancel pins that the scratch
// cleanup runs to the end when the deadline bounding a reconcile cuts its body
// short: the body cancels the context the reconcile runs on and fails with
// that cancellation, and the session still returns to the data branch with
// both scratch branches dropped, the body's error reported.
func TestReconcileScratchCleanupSurvivesCallerCancel(t *testing.T) {
	t.Parallel()
	bg := context.Background()
	st, err := Open(bg, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	dataBranch, err := activeBranch(bg, st.db)
	if err != nil {
		t.Fatalf("read active branch: %v", err)
	}

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	head := headCommit(t, bg, st)
	// A scratch run's caller holds the commit lock, as every reconcile does.
	err = st.withCommitLock(ctx, func(ctx context.Context) error {
		return st.runOnScratch(ctx, dataBranch, []string{newReconcileScratch().spine}, head, attemptOnce, func() error {
			cancel()
			return ctx.Err()
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runOnScratch() error = %v; want the body's cancellation", err)
	}
	assertScratchBranchCleanedUp(t, bg, st)
	if branch, err := activeBranch(bg, st.db); err != nil || branch != dataBranch {
		t.Fatalf("active branch = %q (%v); want the data branch %q", branch, err, dataBranch)
	}
}

// mergeBase reads unrelated histories off doltdb.ErrNoCommonAncestor alone, so
// any other failure of DOLT_MERGE_BASE is an error and never "unrelated": a
// ref that does not exist must not let two refs pass as sharing no history.
func TestAMergeBaseThatFailsIsAnErrorNotUnrelated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	same, err := mergeBase(ctx, st.db, "master", "master")
	if err != nil {
		t.Fatalf("mergeBase(master, master) error = %v", err)
	}
	if _, ok := same.shared(); !ok {
		t.Fatalf("mergeBase(master, master) = %+v, want a shared commit", same)
	}
	missing, err := mergeBase(ctx, st.db, "master", "no-such-ref")
	if err == nil {
		t.Fatalf("mergeBase(master, no-such-ref) = %+v, want an error", missing)
	}
	if errors.Is(err, doltdb.ErrNoCommonAncestor) {
		t.Fatalf("mergeBase(master, no-such-ref) error = %v, which reads as unrelated histories", err)
	}
}
