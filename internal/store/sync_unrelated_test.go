package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

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

	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote)
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

	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal)
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

	_, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote)
	if err == nil {
		t.Fatalf("SyncResolveUnrelated on a shared-history divergence returned nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "shares history") {
		t.Fatalf("refusal error = %q, want it to name the shared history", err.Error())
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

	// take-local: refused with the schema-ahead contract, no write.
	_, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeLocal)
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
	res, err := syncB.SyncResolveUnrelated(ctx, "origin", "master", TakeRemote)
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
