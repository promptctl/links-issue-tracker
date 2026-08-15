package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/merge"
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

	localOnly, remoteOnly, shared := seedUnrelatedPairWithShared(t, ctx, rootA, rootB, remoteURL, "")

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
	assertIDSet(t, "only-local", res.Unrelated.OnlyLocal, []string{localOnly})
	assertIDSet(t, "only-remote", res.Unrelated.OnlyRemote, []string{remoteOnly})
	assertIDSet(t, "on-both", res.Unrelated.OnBoth, []string{shared})
}

// ownerApprovedTakeToken exercises the owner-approval gate on the way to a
// permitted take (links-sync-pgct.4): the bare call must refuse with a fresh
// token and the destruction inventory, mutating nothing — the only way any test
// (like any surface) obtains a token is by reading the refusal.
func ownerApprovedTakeToken(t *testing.T, ctx context.Context, st *Store, remote, branch string, choice UnrelatedResolution) string {
	t.Helper()
	_, err := st.SyncResolveUnrelated(ctx, remote, branch, choice, "")
	var approval OwnerApprovalRequiredError
	if !errors.As(err, &approval) {
		t.Fatalf("SyncResolveUnrelated(%s) without approval = %v, want OwnerApprovalRequiredError", choice, err)
	}
	if approval.ApprovalToken == "" || approval.Stale {
		t.Fatalf("bare take refusal carried token=%q stale=%v, want a fresh token", approval.ApprovalToken, approval.Stale)
	}
	if approval.Inventory == nil {
		t.Fatalf("bare take refusal carried no inventory naming what the take would destroy")
	}
	return approval.ApprovalToken
}

// TestSyncResolveUnrelatedTakeRemote drives the ticket's criterion for the remote
// side: from the unrelated-histories state, choosing remote makes local content equal
// the remote and sync report clean, and the discard of the local-only issue is
// reported (not silent) via the result's inventory.
func TestSyncResolveUnrelatedTakeRemote(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeRemote)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote, token)
	if err != nil {
		t.Fatalf("SyncResolveUnrelated(TakeRemote): %v", err)
	}
	if res.State != SyncReconcileTookRemote {
		t.Fatalf("state = %q, want %q", res.State, SyncReconcileTookRemote)
	}
	// The discard is reported: the both-sides partition names the local-only issue that
	// take-remote drops. [LAW:no-silent-failure]
	if res.Unrelated == nil {
		t.Fatalf("take-remote carried no inventory to report the discard")
	}
	assertIDSet(t, "only-local (discarded)", res.Unrelated.OnlyLocal, []string{localIssueID})
	assertIDSet(t, "only-remote (kept)", res.Unrelated.OnlyRemote, []string{remoteIssueID})

	// Local content now equals the remote: the remote issue is present, the local-only
	// issue is gone.
	assertLocalIssueIDs(t, ctx, syncB, []string{remoteIssueID})

	// Sync is clean: local head equals the remote head, so freshness is up-to-date, not
	// diverged, and no push is needed.
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after take-remote: %v", err)
	}
	if fresh.State() != SyncUpToDate {
		t.Fatalf("post-take-remote freshness = %q ahead=%d behind=%d, want up_to_date", fresh.State(), fresh.Ahead, fresh.Behind)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)
}

// TestSyncResolveUnrelatedTakeLocal drives the ticket's criterion for the local side:
// choosing local makes the remote-tracking side converge to local (local becomes a
// fast-forwardable descendant carrying its own backlog; a push then converges the
// remote), and the discard of the remote-only issue is reported, not silent.
func TestSyncResolveUnrelatedTakeLocal(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeLocal)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal, token)
	if err != nil {
		t.Fatalf("SyncResolveUnrelated(TakeLocal): %v", err)
	}
	if res.State != SyncReconcileTookLocal {
		t.Fatalf("state = %q, want %q", res.State, SyncReconcileTookLocal)
	}
	if res.Unrelated == nil {
		t.Fatalf("take-local carried no inventory to report the discard")
	}
	assertIDSet(t, "only-remote (discarded)", res.Unrelated.OnlyRemote, []string{remoteIssueID})
	assertIDSet(t, "only-local (kept)", res.Unrelated.OnlyLocal, []string{localIssueID})

	// Local content is the local backlog wholesale: the local issue survives, the
	// remote-only issue is dropped.
	assertLocalIssueIDs(t, ctx, syncB, []string{localIssueID})

	// Local is now a fast-forwardable descendant of the remote head: not diverged,
	// strictly ahead, so the push fast-forwards. [LAW:one-source-of-truth] the head is
	// the durable state; freshness reads it, not a stored flag.
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after take-local: %v", err)
	}
	if fresh.State() != SyncAhead {
		t.Fatalf("post-take-local freshness = %q ahead=%d behind=%d, want ahead", fresh.State(), fresh.Ahead, fresh.Behind)
	}

	// The remote-tracking side converges to local: push fast-forwards, and A receiving
	// it sees exactly the local backlog (the remote-only issue discarded on both ends).
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after take-local: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	recv, err := syncA.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != SyncReceiveFastForwarded {
		t.Fatalf("A receive state = %q, want fast_forwarded", recv.State)
	}
	assertLocalIssueIDs(t, ctx, syncA, []string{localIssueID})
}

// TestSyncResolveUnrelatedOwnerApprovalBindsForkAndSide pins the gate's binding
// (links-sync-pgct.4): a token authorizes destroying one exact fork on one exact
// side, so presenting it for the other side is refused as stale, and a local
// commit landing after issuance voids it — the refusal re-mints a token for the
// moved fork, and only that fresh token runs. No refused path mutates.
func TestSyncResolveUnrelatedOwnerApprovalBindsForkAndSide(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	localToken := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeLocal)

	// Side binding: a take-local approval does not authorize take-remote.
	_, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote, localToken)
	var wrongSide OwnerApprovalRequiredError
	if !errors.As(err, &wrongSide) || !wrongSide.Stale {
		t.Fatalf("take-remote with a take-local token = %v, want a stale-approval refusal", err)
	}

	// Fork binding: a local commit after issuance voids the token. updateLocal
	// opens its own store, so the handle closes around it (one writer per path).
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B) before local mutation: %v", err)
	}
	updateLocal(t, ctx, rootB, localIssueID, UpdateIssueInput{Lane: strptr("moved")})
	syncB = openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	headAfterMove := headCommit(t, ctx, syncB)

	_, err = syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal, localToken)
	var stale OwnerApprovalRequiredError
	if !errors.As(err, &stale) || !stale.Stale {
		t.Fatalf("take-local with a pre-move token = %v, want a stale-approval refusal", err)
	}
	if stale.ApprovalToken == localToken {
		t.Fatalf("the moved fork re-minted the same token, want a fresh one bound to the new head")
	}
	if got := headCommit(t, ctx, syncB); got != headAfterMove {
		t.Fatalf("a refused take moved the data branch: %s -> %s", headAfterMove, got)
	}

	// The re-minted token authorizes the take against the fork as it now stands.
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal, stale.ApprovalToken)
	if err != nil {
		t.Fatalf("take-local with the re-minted token: %v", err)
	}
	if res.State != SyncReconcileTookLocal {
		t.Fatalf("state = %q, want %q", res.State, SyncReconcileTookLocal)
	}
}

// TestSyncReconcileCombineUnionsBothSides drives the ticket's core acceptance criterion:
// two disjoint stores with some unique ids and one shared id, after combine, hold EVERY
// unique issue plus the field-merged shared one — nothing dropped. The shared id is planted
// verbatim on B so both sides carry identical content, so it merges cleanly (no prose held)
// and combine settles as SyncReconcileCombined. The union then fast-forward-pushes and the
// remote side converges onto it, proving no side's issues were lost. [LAW:no-silent-failure]
func TestSyncReconcileCombineUnionsBothSides(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	localOnly, remoteOnly, shared := seedUnrelatedPairWithShared(t, ctx, rootA, rootB, remoteURL, "")

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	res, err := syncB.SyncReconcileCombine(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcileCombine(B): %v", err)
	}
	if res.State != SyncReconcileCombined {
		t.Fatalf("combine state = %q, want %q (pending=%+v)", res.State, SyncReconcileCombined, res.Pending)
	}
	// The union reports what it kept from each side — the partition read off the two anchors.
	if res.Unrelated == nil {
		t.Fatalf("combine carried no both-sides inventory to report the union")
	}
	assertIDSet(t, "kept only-local", res.Unrelated.OnlyLocal, []string{localOnly})
	assertIDSet(t, "kept only-remote", res.Unrelated.OnlyRemote, []string{remoteOnly})
	assertIDSet(t, "field-merged on-both", res.Unrelated.OnBoth, []string{shared})

	// Nothing dropped: local now holds the UNION of all three ids.
	assertLocalIssueIDs(t, ctx, syncB, []string{localOnly, remoteOnly, shared})

	// The union is a fast-forwardable descendant of the remote head, so the push converges
	// the remote onto it (the mirror of take-local, but keeping BOTH sides).
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness(B) after combine: %v", err)
	}
	if fresh.State() != SyncAhead {
		t.Fatalf("post-combine freshness = %q ahead=%d behind=%d, want ahead", fresh.State(), fresh.Ahead, fresh.Behind)
	}
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after combine: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	recv, err := syncA.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != SyncReceiveFastForwarded {
		t.Fatalf("A receive state = %q, want fast_forwarded", recv.State)
	}
	assertLocalIssueIDs(t, ctx, syncA, []string{localOnly, remoteOnly, shared})
}

// TestSyncReconcileCombineHoldsAndFinalizesProse drives the ticket's per-issue-overlap
// criterion: when the shared id's free text diverged on both sides, combine holds it as
// prose-pending (never auto-picking a side) rather than dropping or guessing, and the SAME
// `lit sync reconcile resolve` finalize path splices the agent's merged text and commits the
// union. It proves the two-way (no base) resolution surfaces prose exactly as the shared-history
// three-way does. [LAW:no-silent-failure]
func TestSyncReconcileCombineHoldsAndFinalizesProse(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	// Plant the shared id on B with a DIFFERENT title than A's, so its free text diverges
	// with no base — the one field the engine must hold for the agent.
	localOnly, remoteOnly, shared := seedUnrelatedPairWithShared(t, ctx, rootA, rootB, remoteURL, "shared-on-B")

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)

	held, err := syncB.SyncReconcileCombine(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcileCombine(B): %v", err)
	}
	if held.State != SyncReconcileProsePending {
		t.Fatalf("combine state = %q, want %q", held.State, SyncReconcileProsePending)
	}
	if len(held.Pending) != 1 {
		t.Fatalf("pending count = %d, want 1 (the shared id's title): %+v", len(held.Pending), held.Pending)
	}
	p := held.Pending[0]
	if p.IssueID != shared || p.Field != "title" {
		t.Fatalf("pending = %+v, want issue=%s field=title", p, shared)
	}
	// No base, so the held conflict's Base is empty; ours is B's planted title, theirs is A's.
	if p.Base != "" || p.Ours != "shared-on-B" || p.Theirs != "shared" {
		t.Fatalf("pending base=%q ours=%q theirs=%q, want empty/shared-on-B/shared", p.Base, p.Ours, p.Theirs)
	}
	// Nothing committed while prose is held: the data branch never moved.
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved during a prose-held combine: %s -> %s", headBefore, got)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)

	// Finalize through the SAME resolve path the three-way reconcile uses: the divergence is
	// re-derived (no base), the merged text spliced, and the union committed as one commit.
	res, err := syncB.SyncReconcileResolved(ctx, "origin", "master", []merge.ProseResolution{
		{IssueID: shared, Field: merge.ProseTitle, Fingerprint: p.Fingerprint(), Text: "merged A and B shared title"},
	})
	if err != nil {
		t.Fatalf("SyncReconcileResolved(B) finalizing combine: %v", err)
	}
	if res.State != SyncReconcileCombined {
		t.Fatalf("finalized combine state = %q, want %q", res.State, SyncReconcileCombined)
	}
	// The union is present AND the shared id carries the agent's merged title — nothing dropped.
	assertLocalIssueIDs(t, ctx, syncB, []string{localOnly, remoteOnly, shared})
	got := getIssueOrFatal(t, ctx, syncB, shared)
	if got.Title != "merged A and B shared title" {
		t.Fatalf("shared title after combine finalize = %q, want the agent's merged text", got.Title)
	}
}

// seedUnrelatedPairWithShared builds two disjoint stores that share a remote and hold one
// common issue id: A (the remote side) gets a remote-only issue plus a shared one it pushes;
// B (the local side, its own bootstrap root) gets a local-only issue, then A's shared issue
// is planted verbatim — the one way independently-generated ids can genuinely collide (the
// same logical ticket filed in both). sharedTitleOnB, when non-empty, overrides the planted
// title so the shared id's free text diverges with no base. Returns (onlyLocalID,
// onlyRemoteID, sharedID).
func seedUnrelatedPairWithShared(t *testing.T, ctx context.Context, rootA, rootB, remoteURL, sharedTitleOnB string) (string, string, string) {
	t.Helper()
	stA, err := Open(ctx, rootA, "wsA")
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

	stB, err := Open(ctx, rootB, "wsB")
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
	sharedIssue := issueByID(t, exportA, shared.ID)
	if sharedTitleOnB != "" {
		sharedIssue.Title = sharedTitleOnB
	}
	combined := exportB
	combined.Issues = append(combined.Issues, sharedIssue)
	if err := stB.replaceFromExport(ctx, combined, commitStamp{Message: "plant shared issue"}); err != nil {
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
	return localOnly.ID, remoteOnly.ID, shared.ID
}

// TestSyncResolveUnrelatedRefusesSharedHistory proves take-one is scoped to unrelated
// histories: a divergence WITH a common base is mergeable, so taking one side
// wholesale would silently drop the other side's non-conflicting work. The resolver
// refuses loudly and mutates nothing. [LAW:no-silent-failure]
func TestSyncResolveUnrelatedRefusesSharedHistory(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	updateAndPush(t, ctx, rootA, id, UpdateIssueInput{Lane: strptr("alpha")})
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Priority: ptr(model.PriorityUrgent)})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)

	// The shared-history refusal precedes the owner gate: a mergeable divergence is
	// never takeable, approved or not, so no token is minted for it.
	_, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote, "")
	if err == nil {
		t.Fatalf("SyncResolveUnrelated on a shared-history divergence returned nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "shares history") {
		t.Fatalf("refusal error = %q, want it to name the shared history", err.Error())
	}
	var approval OwnerApprovalRequiredError
	if errors.As(err, &approval) {
		t.Fatalf("shared-history refusal minted an approval token, want the merge redirect instead")
	}
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("data branch moved during a refused take: %s -> %s", headBefore, got)
	}
	assertWorkingSetClean(t, ctx, syncB)
}

// TestSyncResolveUnrelatedTakeLocalRefusesSchemaAheadRemote proves take-local shares
// the three-way path's schema-ahead refusal: because it authors a replay commit ON the
// remote head, a remote at a schema this binary cannot produce would make it author a
// commit BELOW that schema (dropping newer fields) and regress the shared remote on
// push. It must refuse and mutate nothing — while take-remote, which adopts the remote
// head wholesale and authors no replay commit, is exempt and proceeds. [LAW:single-enforcer]
func TestSyncResolveUnrelatedTakeLocalRefusesSchemaAheadRemote(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	// A seeds the remote at the current schema, then advances its head to a future
	// schema and pushes; B forks an UNRELATED clone (own root, own issue) and fetches,
	// so B is unrelated-diverged from a schema-ahead remote head.
	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	advanceRemoteToFutureSchema(t, ctx, rootA, remoteIssueID, "v9.9.0", UpdateIssueInput{Lane: strptr("from-a")})
	forkUnrelatedClone(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	headBefore := headCommit(t, ctx, syncB)

	// take-local: owner-approved, then refused with the schema-ahead contract, no
	// write — the approval gate governs WHO may run the destruction; the schema
	// guard still governs whether the store may author the commit at all.
	takeLocalToken := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeLocal)
	_, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal, takeLocalToken)
	var ahead *RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("take-local onto schema-ahead remote = %v, want *RemoteSchemaAheadError", err)
	}
	if got := headCommit(t, ctx, syncB); got != headBefore {
		t.Fatalf("refused take-local still moved the local head: %s -> %s", headBefore, got)
	}
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)

	// take-remote: exempt — it adopts the schema-ahead head wholesale (the recovery that
	// gets a stale binary the newer data), so it proceeds rather than refusing.
	takeRemoteToken := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeRemote)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote, takeRemoteToken)
	if err != nil {
		t.Fatalf("take-remote onto schema-ahead remote errored, want it exempt: %v", err)
	}
	if res.State != SyncReconcileTookRemote {
		t.Fatalf("take-remote state = %q, want %q", res.State, SyncReconcileTookRemote)
	}
	if got := headCommit(t, ctx, syncB); got == headBefore {
		t.Fatalf("take-remote did not adopt the remote head (local head unchanged at %s)", got)
	}
}

// TestUnrelatedResolutionValid pins the boundary guard: only the two real sides are
// valid, so an unknown value is rejected at SyncResolveUnrelated's door rather than
// reaching the dispatch and silently no-op'ing. [LAW:no-silent-failure]
func TestUnrelatedResolutionValid(t *testing.T) {
	for _, valid := range []UnrelatedResolution{TakeLocal, TakeRemote} {
		if !valid.valid() {
			t.Errorf("%q reported invalid, want valid", valid)
		}
	}
	for _, invalid := range []UnrelatedResolution{"", "combine", "mine", "REMOTE"} {
		if UnrelatedResolution(invalid).valid() {
			t.Errorf("%q reported valid, want invalid", invalid)
		}
	}
}

// assertLocalIssueIDs fails unless the store's data branch holds exactly want issue
// ids — the wholesale-take assertion that the chosen side survived and the other's
// unique issues were dropped.
func assertLocalIssueIDs(t *testing.T, ctx context.Context, st *Store, want []string) {
	t.Helper()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export for local issue ids: %v", err)
	}
	got := make([]string, 0, len(export.Issues))
	for _, issue := range export.Issues {
		got = append(got, issue.ID)
	}
	assertIDSet(t, "local issue ids", got, want)
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

// TestSyncReconcileCombinePreservesFoldedProvenance drives the links-sync-pgct.6
// acceptance: reconcile two workspaces with unrelated histories where the local
// side holds N data commits; after combine, those changes appear as individually
// attributable commits on the new spine — original message and timestamp (to the
// second, Dolt's --date granularity) preserved, in their original order, each
// mid-chain state a whole union backlog — settled by the combine's marker
// commit; the contents equal the union and the push fast-forwards. The squash
// this replaces is the 2026-08-08 field-incident cost: the data survived but its
// provenance did not.
func TestSyncReconcileCombinePreservesFoldedProvenance(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	// B's folded side: three data commits with distinct messages — create,
	// update, add label — so each one is individually identifiable on the spine.
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, localIssueID, UpdateIssueInput{Lane: strptr("fold-lane")})
	addLabelLocal(t, ctx, rootB, localIssueID, "fold-label")

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	foldedMessages := []string{"create issue", "apply update", "add label"}
	originals := originalCommitsByMessage(t, ctx, syncB, foldedMessages...)

	res, err := syncB.SyncReconcileCombine(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcileCombine(B): %v", err)
	}
	if res.State != SyncReconcileCombined {
		t.Fatalf("combine state = %q (pending=%+v), want %q", res.State, res.Pending, SyncReconcileCombined)
	}
	if res.Replayed != len(foldedMessages) {
		t.Fatalf("replayed provenance commits = %d, want %d", res.Replayed, len(foldedMessages))
	}

	// The spine: B's three data commits in original order under original
	// messages and timestamps, then the combine marker. B's bootstrap commits
	// (init, migrations, workspace meta) project no backlog change onto the
	// spine, so they land nothing.
	assertLinearSpineToRemoteHead(t, ctx, syncB, res.RemoteHead)
	spine := spineSince(t, ctx, syncB, res.RemoteHead)
	assertFoldedSpine(t, spine, foldedMessages, originals, combineCommitMessage)

	// Mid-chain states are whole backlogs: the union projection carries the
	// remote-only issue from the FIRST replayed commit, not just from the marker.
	assertIssuePresentAsOf(t, ctx, syncB, spine[0].hash, remoteIssueID, true)

	// Contents equal the union, and the spine fast-forward-pushes; the remote
	// side converges onto it.
	assertLocalIssueIDs(t, ctx, syncB, []string{localIssueID, remoteIssueID})
	assertWorkingSetClean(t, ctx, syncB)
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after combine: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	recv, err := syncA.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != SyncReceiveFastForwarded {
		t.Fatalf("A receive state = %q, want fast_forwarded", recv.State)
	}
	assertLocalIssueIDs(t, ctx, syncA, []string{localIssueID, remoteIssueID})
}

// TestSyncResolveUnrelatedTakeLocalPreservesFoldedProvenance is the same
// acceptance for the take-local resolution (which the engine permits only on
// unrelated histories — a shared-history divergence is mergeable and refused):
// after the owner-approved take, the local side's commits appear individually
// on the spine with message and timestamp preserved, and the DISCARD of the
// remote-only issues is the marker commit's own diff — present through the last
// provenance commit, gone at the marker — attributing the destruction to the
// take itself rather than to any of local's commits.
func TestSyncResolveUnrelatedTakeLocalPreservesFoldedProvenance(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, localIssueID, UpdateIssueInput{Lane: strptr("fold-lane")})
	addLabelLocal(t, ctx, rootB, localIssueID, "fold-label")

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	foldedMessages := []string{"create issue", "apply update", "add label"}
	originals := originalCommitsByMessage(t, ctx, syncB, foldedMessages...)

	token := ownerApprovedTakeToken(t, ctx, syncB, "origin", "master", TakeLocal)
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal, token)
	if err != nil {
		t.Fatalf("SyncResolveUnrelated(TakeLocal): %v", err)
	}
	if res.State != SyncReconcileTookLocal {
		t.Fatalf("state = %q, want %q", res.State, SyncReconcileTookLocal)
	}
	if res.Replayed != len(foldedMessages) {
		t.Fatalf("replayed provenance commits = %d, want %d", res.Replayed, len(foldedMessages))
	}

	assertLinearSpineToRemoteHead(t, ctx, syncB, res.RemoteHead)
	spine := spineSince(t, ctx, syncB, res.RemoteHead)
	assertFoldedSpine(t, spine, foldedMessages, originals, takeLocalCommitMessage)

	// The discard is the take's own act: the remote-only issue survives through
	// every provenance commit (union projections) and disappears exactly at the
	// marker — so history attributes the owner-approved destruction to the take,
	// not to any of local's replayed commits.
	assertIssuePresentAsOf(t, ctx, syncB, spine[len(spine)-2].hash, remoteIssueID, true)
	assertIssuePresentAsOf(t, ctx, syncB, spine[len(spine)-1].hash, remoteIssueID, false)

	// Contents are local's backlog wholesale, and the spine fast-forward-pushes;
	// the remote side converges onto it.
	assertLocalIssueIDs(t, ctx, syncB, []string{localIssueID})
	assertWorkingSetClean(t, ctx, syncB)
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after take-local: %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	recv, err := syncA.SyncReceive(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if recv.State != SyncReceiveFastForwarded {
		t.Fatalf("A receive state = %q, want fast_forwarded", recv.State)
	}
	assertLocalIssueIDs(t, ctx, syncA, []string{localIssueID})
}

// assertFoldedSpine fails unless the replayed spine is exactly the folded
// side's data commits — original messages in original order, each timestamp
// equal to the original to the second (Dolt's --date granularity truncates
// sub-second precision), each author equal to the original committer/email —
// settled by the marker commit as its final entry.
func assertFoldedSpine(t *testing.T, spine []spineEntry, foldedMessages []string, originals map[string]spineEntry, markerMessage string) {
	t.Helper()
	if len(spine) != len(foldedMessages)+1 {
		t.Fatalf("replayed spine holds %d commits %+v, want %d folded + 1 marker", len(spine), spine, len(foldedMessages))
	}
	for i, message := range foldedMessages {
		if spine[i].message != message {
			t.Fatalf("spine[%d] message = %q, want %q (original order preserved)", i, spine[i].message, message)
		}
		original := originals[message]
		if want := original.date.UTC().Truncate(time.Second); !spine[i].date.UTC().Equal(want) {
			t.Fatalf("spine[%d] (%q) date = %s, want original %s", i, message, spine[i].date.UTC(), want)
		}
		if spine[i].committer != original.committer || spine[i].email != original.email {
			t.Fatalf("spine[%d] (%q) author = %s <%s>, want original %s <%s>", i, message, spine[i].committer, spine[i].email, original.committer, original.email)
		}
	}
	if last := spine[len(spine)-1].message; last != markerMessage {
		t.Fatalf("spine marker message = %q, want %q", last, markerMessage)
	}
}

// addLabelLocal adds a label to an issue and leaves it local (unpushed) — a
// third distinct data-commit message for provenance fixtures.
func addLabelLocal(t *testing.T, ctx context.Context, root, id, label string) {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(label %s): %v", root, err)
	}
	if _, err := st.AddLabel(ctx, AddLabelInput{IssueID: id, Name: label, CreatedBy: "test"}); err != nil {
		t.Fatalf("AddLabel(%s): %v", id, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(label): %v", err)
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

// TestSyncReconcileCombineRecoversFromTransientFailureMidReplay pins the
// scratch replay's recovery contract: scratch-branch commits take a SINGLE
// attempt, so a transient online-GC failure mid-replay must bubble to
// replayUnderGuard's outer retry, whose next attempt re-enters
// runOnReconcileScratch on the rotated (default-branch) connection and
// re-checkouts the scratch branch before rebuilding the whole spine. An
// inline self-rotating retry here would resume committing on the data branch
// — the corruption this test exists to catch; had the retried attempt run
// anywhere but the rebuilt scratch, the spine, contents, and clean-state
// assertions below could not all hold. The injected failure hits the FIRST
// commit of the replay (the lift), the earliest point a rotation could
// strand the session off the scratch branch.
func TestSyncReconcileCombineRecoversFromTransientFailureMidReplay(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	remoteIssueID := seedReconcileRemote(t, ctx, rootA, remoteURL)
	localIssueID := forkUnrelatedClone(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, localIssueID, UpdateIssueInput{Lane: strptr("fold-lane")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer func() { _ = syncB.Close() }()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	foldedMessages := []string{"create issue", "apply update"}
	originals := originalCommitsByMessage(t, ctx, syncB, foldedMessages...)

	fires := 0
	syncB.commitWorkingSetHookForTest = func() error {
		fires++
		if fires == 1 {
			return transientGCContentionError{err: errors.New("injected transient online-gc contention mid-replay")}
		}
		return nil
	}

	res, err := syncB.SyncReconcileCombine(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcileCombine(B) did not recover from the mid-replay transient failure: %v", err)
	}
	syncB.commitWorkingSetHookForTest = nil
	if fires < 2 {
		t.Fatalf("commit hook fired %d time(s); want the injected failure to have forced an outer retry", fires)
	}
	if res.State != SyncReconcileCombined {
		t.Fatalf("combine state = %q, want %q", res.State, SyncReconcileCombined)
	}
	if res.Replayed != len(foldedMessages) {
		t.Fatalf("replayed provenance commits = %d, want %d", res.Replayed, len(foldedMessages))
	}

	// The rebuilt spine is intact — provenance, marker, linearity — and the
	// store is clean: the failed first attempt leaked nothing onto the data
	// branch and left no scratch residue.
	assertLinearSpineToRemoteHead(t, ctx, syncB, res.RemoteHead)
	spine := spineSince(t, ctx, syncB, res.RemoteHead)
	assertFoldedSpine(t, spine, foldedMessages, originals, combineCommitMessage)
	assertLocalIssueIDs(t, ctx, syncB, []string{localIssueID, remoteIssueID})
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertWorkingSetClean(t, ctx, syncB)
}
