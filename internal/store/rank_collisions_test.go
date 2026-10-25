package store

import (
	"cmp"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// listedOrder is the ids of the placed issues in export, in the order every
// listing puts them: key, then id.
func listedOrder(export model.Export) []string {
	placed := placedIssues(export)
	slices.SortFunc(placed, func(a, b rankedIssue) int {
		return cmp.Or(strings.Compare(a.rank, b.rank), strings.Compare(a.id, b.id))
	})
	ids := make([]string, len(placed))
	for i, issue := range placed {
		ids[i] = issue.id
	}
	return ids
}

// The separation's contract, checked over generated exports rather than a
// handful of cases: afterwards no key has two placed holders, every listing
// keeps its order, an unranked or deleted issue keeps the key it had, the
// issues named are exactly the ones whose key changed, with the key each held,
// and an export with no shared key comes back as it was. Where every key is
// well formed and no two keys differ only by trailing zeros, the separation is
// as small as it can be: one holder of each shared key keeps it, and nobody
// else moves.
//
// The key pool is small so collisions are common, adjacent shared keys occur,
// and it holds "V" beside "V0" and "V00" — distinct keys with no room between
// them, the pair the unfiled epic's mint makes — and "a-1", a malformed key.
func TestSeparateRankCollisionsKeepsEveryListingInOrder(t *testing.T) {
	pool := []string{"", "1", "5", "5z", "8", "V", "V0", "V00", "W", "a-1", "h", "z"}
	deletedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewSource(1))
	var sawShared, sawMinimal, sawDeletedShare, sawNoRoom int
	for round := range 2000 {
		n := 1 + rng.Intn(12)
		export := model.Export{}
		for _, id := range rng.Perm(n) {
			issue := model.Issue{ID: fmt.Sprintf("t-%02d", id), Rank: pool[rng.Intn(len(pool))]}
			if rng.Intn(5) == 0 {
				issue.SetRetention(model.Deleted{At: deletedAt})
			}
			export.Issues = append(export.Issues, issue)
		}
		before := slices.Clone(export.Issues)
		placedHolders := map[string]int{}
		for _, issue := range placedIssues(export) {
			placedHolders[issue.rank]++
		}
		var sharedKeys, wantMoves int
		for _, count := range placedHolders {
			if count > 1 {
				sharedKeys++
				wantMoves += count - 1
			}
		}

		got, rekeyed, err := separateRankCollisions(export)
		if err != nil {
			t.Fatalf("round %d: separateRankCollisions(%+v) error = %v", round, before, err)
		}
		if !reflect.DeepEqual(export.Issues, before) {
			t.Fatalf("round %d: the input export was mutated", round)
		}
		if collisions := rankCollisions(placedIssues(got)); len(collisions) != 0 {
			t.Errorf("round %d: %+v still shares keys: %s", round, got.Issues, describeRankCollisions(collisions))
		}
		if want, have := listedOrder(model.Export{Issues: before}), listedOrder(got); !slices.Equal(want, have) {
			t.Errorf("round %d: listed order %v, want %v (from %+v)", round, have, want, before)
		}
		changed := map[string]string{}
		for i, issue := range before {
			_, deleted := issue.Retention().(model.Deleted)
			if (deleted || issue.Rank == "") && got.Issues[i].Rank != issue.Rank {
				t.Errorf("round %d: %s (deleted %t) held %q and now holds %q", round, issue.ID, deleted, issue.Rank, got.Issues[i].Rank)
			}
			if got.Issues[i].Rank != issue.Rank {
				changed[issue.ID] = issue.Rank
			}
		}
		for _, r := range rekeyed {
			from, ok := changed[r.IssueID]
			if !ok || r.From != from {
				t.Errorf("round %d: %s is named as moving from %q; its key changed: %t, from %q", round, r.IssueID, r.From, ok, from)
			}
			if !rank.Valid(r.Rank) {
				t.Errorf("round %d: %s was given %q, not a well-formed rank", round, r.IssueID, r.Rank)
			}
			delete(changed, r.IssueID)
		}
		if len(changed) != 0 {
			t.Errorf("round %d: keys changed without being named: %v", round, changed)
		}
		if sharedKeys == 0 {
			if len(rekeyed) != 0 || !reflect.DeepEqual(got.Issues, before) {
				t.Errorf("round %d: no placed issue shared a key, yet the separation named %+v", round, rekeyed)
			}
			for _, issue := range before {
				if _, deleted := issue.Retention().(model.Deleted); deleted && placedHolders[issue.Rank] > 0 {
					sawDeletedShare++
					break
				}
			}
			continue
		}
		sawShared++
		if placedHolders["V0"] > 1 && placedHolders["V"] > 0 && placedHolders["V00"] > 0 {
			sawNoRoom++
		}
		if !keysSpaceApart(placedHolders) {
			continue
		}
		sawMinimal++
		if len(rekeyed) != wantMoves {
			t.Errorf("round %d: named %d issue(s) %+v, want %d: one holder of each shared key keeps it (from %+v)", round, len(rekeyed), rekeyed, wantMoves, before)
		}
		for _, r := range rekeyed {
			if placedHolders[r.From] < 2 {
				t.Errorf("round %d: %s held %q alone and was moved", round, r.IssueID, r.From)
			}
		}
	}
	// Anti-vacuity: every property above holds trivially of an export that
	// never exercises it.
	for name, saw := range map[string]int{"shared a key": sawShared, "had keys spaced apart": sawMinimal, "held a deleted issue's key beside a placed one": sawDeletedShare, "shared a key with no room beside it": sawNoRoom} {
		if saw < 20 {
			t.Errorf("only %d of 2000 generated exports %s; the generator is not testing it", saw, name)
		}
	}
}

// keysSpaceApart reports whether every key is well formed and no two keys
// differ only by trailing zeros — the exports in which a gap opens between any
// two keys.
func keysSpaceApart(holders map[string]int) bool {
	significant := map[string]bool{}
	for key := range holders {
		if !rank.Valid(rank.Significant(key)) || significant[rank.Significant(key)] {
			return false
		}
		significant[rank.Significant(key)] = true
	}
	return true
}

// A shared key whose neighbours pad to the same value leaves nothing to space
// its holders into. The separation moves the neighbours too, so the landing
// still keeps every listing's order rather than failing on data the rank
// repair could mend.
func TestSeparateRankCollisionsLandsAKeyWithNoRoomBesideIt(t *testing.T) {
	export := model.Export{Issues: []model.Issue{
		{ID: "t-1", Rank: "V"},
		{ID: "t-2", Rank: "V0"},
		{ID: "t-3", Rank: "V0"},
		{ID: "t-4", Rank: "V00"},
	}}
	got, _, err := separateRankCollisions(export)
	if err != nil {
		t.Fatalf("separateRankCollisions error = %v", err)
	}
	if collisions := rankCollisions(placedIssues(got)); len(collisions) != 0 {
		t.Errorf("%+v still shares keys: %s", got.Issues, describeRankCollisions(collisions))
	}
	if want, have := []string{"t-1", "t-2", "t-3", "t-4"}, listedOrder(got); !slices.Equal(have, want) {
		t.Errorf("listed order %v, want %v", have, want)
	}
}

// plantRank writes key onto id behind every rank verb's back: the state a
// merge or an old restore leaves.
func plantRank(t *testing.T, ctx context.Context, st *Store, id, key string) {
	t.Helper()
	if err := st.ExecRawForTest(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, key, id); err != nil {
		t.Fatalf("plant rank %q on %s: %v", key, id, err)
	}
}

func listedIDs(t *testing.T, ctx context.Context, st *Store) []string {
	t.Helper()
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

// Doctor counts each key two placed issues hold — a closed one too, since the
// next landing would separate it — and names the holders. The rank repair then
// gives every holder its own key, and the listing it leaves is the listing it
// found.
func TestDoctorCountsSharedRankKeysAndTheRepairSeparatesThemInOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	var epics []model.Issue
	for _, title := range []string{"one", "two", "three", "four"} {
		epics = append(epics, createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "epic"}))
	}
	// Filed under the epic outside the pair, since an epic whose only child is
	// closed is closed with it.
	done := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "done", Topic: "topic", IssueType: "task", ParentID: epics[3].ID})
	if _, err := st.Apply(ctx, done.ID, storage.Change{Action: model.Close{Outcome: model.Obsolete{}}, Actor: "tester"}); err != nil {
		t.Fatalf("close %s: %v", done.ID, err)
	}
	plantRank(t, ctx, st, epics[2].ID, epics[0].Rank)
	plantRank(t, ctx, st, done.ID, epics[1].Rank)
	before := listedIDs(t, ctx, st)

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.DuplicateRanks != 2 {
		t.Errorf("DuplicateRanks = %d, want 2: each planted key, the closed holder's included", report.DuplicateRanks)
	}
	for _, pair := range [][2]model.Issue{{epics[0], epics[2]}, {epics[1], done}} {
		holders := []string{pair[0].ID, pair[1].ID}
		slices.Sort(holders)
		want := fmt.Sprintf("%q shared by %s", pair[0].Rank, strings.Join(holders, ", "))
		if !slices.ContainsFunc(report.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
			t.Errorf("Warnings = %v, want one naming %s", report.Warnings, want)
		}
	}

	if _, err := st.FixRankInversions(ctx); err != nil {
		t.Fatalf("FixRankInversions: %v", err)
	}
	report, err = st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor after repair: %v", err)
	}
	if report.DuplicateRanks != 0 || len(report.Warnings) != 0 {
		t.Errorf("after repair DuplicateRanks = %d, Warnings = %v; want neither", report.DuplicateRanks, report.Warnings)
	}
	if after := listedIDs(t, ctx, st); !slices.Equal(after, before) {
		t.Errorf("listing after repair = %v, want the listing it found %v", after, before)
	}
}

// A restore lands a backup's shared key as two keys, in the order the backup
// listed them, and says which ticket it gave a new key.
func TestRestoreSeparatesASharedRankKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	for _, title := range []string{"one", "two", "three"} {
		createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "epic"})
	}
	backup, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	shared := backup.Issues[0].Rank
	backup.Issues[2].Rank = shared
	holders := []string{backup.Issues[0].ID, backup.Issues[2].ID}
	slices.Sort(holders)
	wantOrder := listedOrder(backup)

	replacement, err := st.ReplaceFromExport(ctx, backup)
	if err != nil {
		t.Fatalf("ReplaceFromExport: %v", err)
	}
	landing := replacement.Landing
	if len(landing.Rekeyed) != 1 || landing.Rekeyed[0].IssueID != holders[0] || landing.Rekeyed[0].From != shared {
		t.Errorf("Rekeyed = %+v, want the holder listed first, %s, moved from %q", landing.Rekeyed, holders[0], shared)
	}
	if got := listedIDs(t, ctx, st); !slices.Equal(got, wantOrder) {
		t.Errorf("listing after restore = %v, want the backup's order %v", got, wantOrder)
	}
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.DuplicateRanks != 0 {
		t.Errorf("DuplicateRanks after restore = %d, want 0", report.DuplicateRanks)
	}
}

// Two replicas that each place a different epic between the same two
// neighbours mint the same key, and the merge resolves each row's rank on its
// own, so both land on it. The reconcile separates them, names the one it
// moved, and the push carries the separated keys to the other replica.
func TestSyncReconcileSeparatesTheKeyTwoReplicasMintedAlike(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")

	first := seedReconcileRemote(t, ctx, rootA, remoteURL)
	for _, title := range []string{"second", "third", "fourth"} {
		createEpicAndPush(t, ctx, rootA, title)
	}
	adoptRemote(t, ctx, rootB, remoteURL)
	seeded := openOrFatal(t, ctx, rootA)
	epics := listedIDs(t, ctx, seeded)
	if err := seeded.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
	if len(epics) != 4 || epics[0] != first {
		t.Fatalf("seeded listing %v, want four epics led by %s", epics, first)
	}
	second, third, fourth := epics[1], epics[2], epics[3]

	placeAboveSecond := func(root, id string) {
		t.Helper()
		st := openOrFatal(t, ctx, root)
		if _, err := st.RankAbove(ctx, id, second); err != nil {
			t.Fatalf("RankAbove(%s, %s): %v", id, second, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%s): %v", root, err)
		}
	}
	placeAboveSecond(rootA, third)
	sync := openSyncOrFatal(t, ctx, rootA)
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(A): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(A sync): %v", err)
	}
	placeAboveSecond(rootB, fourth)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	holders := []string{third, fourth}
	slices.Sort(holders)
	if len(res.Rekeyed) != 1 || res.Rekeyed[0].IssueID != holders[0] {
		t.Fatalf("Rekeyed = %+v, want %s, the first-listed of the two epics placed between the same neighbours %v", res.Rekeyed, holders[0], holders)
	}
	wantOrder := []string{first, holders[0], holders[1], second}
	if got := listedIDs(t, ctx, syncB); !slices.Equal(got, wantOrder) {
		t.Errorf("B lists %v, want %v", got, wantOrder)
	}
	assertNoDuplicateRanks(t, ctx, syncB)
	if _, err := syncB.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(B): %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B sync): %v", err)
	}

	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	if _, err := syncA.SyncReceive(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	if got := listedIDs(t, ctx, syncA); !slices.Equal(got, wantOrder) {
		t.Errorf("A lists %v after receiving, want %v", got, wantOrder)
	}
	assertNoDuplicateRanks(t, ctx, syncA)
}

func assertNoDuplicateRanks(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.DuplicateRanks != 0 {
		t.Errorf("DuplicateRanks = %d, want 0 (warnings %v)", report.DuplicateRanks, report.Warnings)
	}
}
