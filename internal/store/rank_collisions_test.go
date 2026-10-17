package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// listedOrder is the ids of the ranked issues in export, in the order every
// listing puts them: key, then id.
func listedOrder(export model.Export) []string {
	ranked := slices.DeleteFunc(slices.Clone(export.Issues), func(issue model.Issue) bool { return issue.Rank == "" })
	slices.SortFunc(ranked, func(a, b model.Issue) int {
		return cmp.Or(strings.Compare(a.Rank, b.Rank), strings.Compare(a.ID, b.ID))
	})
	ids := make([]string, len(ranked))
	for i, issue := range ranked {
		ids[i] = issue.ID
	}
	return ids
}

// The separation's contract, checked over generated exports rather than a
// handful of cases: afterwards no key has two holders, every listing keeps its
// order, an issue whose key was its own keeps it, an unranked issue stays
// unranked, and the issues named are exactly the holders of a shared key. The
// key pool is small so collisions are common, adjacent shared keys occur, and
// it holds "V" beside "V0" — distinct keys with no room between them, the pair
// the unfiled epic's mint makes.
func TestSeparateRankCollisionsKeepsEveryListingInOrder(t *testing.T) {
	pool := []string{"", "1", "5", "5z", "8", "V", "V0", "W", "h", "z"}
	rng := rand.New(rand.NewSource(1))
	sawShared := 0
	for round := range 500 {
		n := 1 + rng.Intn(12)
		export := model.Export{}
		for _, id := range rng.Perm(n) {
			export.Issues = append(export.Issues, model.Issue{ID: fmt.Sprintf("t-%02d", id), Rank: pool[rng.Intn(len(pool))]})
		}
		before := slices.Clone(export.Issues)
		holders := map[string]int{}
		for _, issue := range before {
			holders[issue.Rank]++
		}

		got, rekeyed, err := separateRankCollisions(export)
		if err != nil {
			t.Fatalf("round %d: separateRankCollisions(%+v) error = %v", round, before, err)
		}
		if !reflect.DeepEqual(export.Issues, before) {
			t.Fatalf("round %d: the input export was mutated", round)
		}
		if collisions := rankCollisions(rankedIssues(got)); len(collisions) != 0 {
			t.Errorf("round %d: %+v still shares keys: %s", round, got.Issues, describeRankCollisions(collisions))
		}
		if want, have := listedOrder(model.Export{Issues: before}), listedOrder(got); !slices.Equal(want, have) {
			t.Errorf("round %d: listed order %v, want %v (from %+v)", round, have, want, before)
		}
		var wantNamed []string
		for i, issue := range before {
			shared := issue.Rank != "" && holders[issue.Rank] > 1
			if shared {
				wantNamed = append(wantNamed, issue.ID)
				continue
			}
			if got.Issues[i].Rank != issue.Rank {
				t.Errorf("round %d: %s held %q alone and now holds %q", round, issue.ID, issue.Rank, got.Issues[i].Rank)
			}
		}
		var named []string
		for _, r := range rekeyed {
			named = append(named, r.IssueID)
			if holders[r.Shared] < 2 {
				t.Errorf("round %d: %s is named as sharing %q, which only it held", round, r.IssueID, r.Shared)
			}
			if !rank.Valid(r.Rank) {
				t.Errorf("round %d: %s was given %q, not a well-formed rank", round, r.IssueID, r.Rank)
			}
		}
		slices.Sort(named)
		slices.Sort(wantNamed)
		if !slices.Equal(named, wantNamed) {
			t.Errorf("round %d: named %v, want the holders of a shared key %v", round, named, wantNamed)
		}
		if len(wantNamed) > 0 {
			sawShared++
		}
	}
	// Anti-vacuity: the properties above hold trivially of an export with no
	// shared key.
	if sawShared < 100 {
		t.Fatalf("only %d of 500 generated exports shared a key; the generator is not testing separation", sawShared)
	}
}

func rankedIssues(export model.Export) []rankedIssue {
	out := make([]rankedIssue, len(export.Issues))
	for i, issue := range export.Issues {
		out[i] = rankedIssue{id: issue.ID, rank: issue.Rank}
	}
	return out
}

// A shared key whose neighbours pad to the same value leaves nothing to space
// its holders into. The separation refuses, naming the key and its holders,
// rather than landing the tie.
func TestSeparateRankCollisionsRefusesWhenNoKeyFitsBetweenTheNeighbours(t *testing.T) {
	export := model.Export{Issues: []model.Issue{
		{ID: "t-1", Rank: "V"},
		{ID: "t-2", Rank: "V0"},
		{ID: "t-3", Rank: "V0"},
		{ID: "t-4", Rank: "V00"},
	}}
	_, _, err := separateRankCollisions(export)
	if !errors.Is(err, rank.ErrNoRoom) {
		t.Fatalf("separateRankCollisions error = %v, want rank.ErrNoRoom", err)
	}
	if !strings.Contains(err.Error(), `"V0" shared by t-2, t-3`) {
		t.Errorf("error %q does not name the shared key and its holders", err)
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

// Doctor counts each key two live issues hold, and names the holders; a closed
// issue holding a live issue's key orders nothing and is not counted. The rank
// repair then gives every live holder its own key, and the listing it leaves is
// the listing it found.
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
	if report.DuplicateRanks != 1 {
		t.Errorf("DuplicateRanks = %d, want 1: one key two live issues hold, the closed holder not counted", report.DuplicateRanks)
	}
	holders := []string{epics[0].ID, epics[2].ID}
	slices.Sort(holders)
	want := fmt.Sprintf("%q shared by %s", epics[0].Rank, strings.Join(holders, ", "))
	if !slices.ContainsFunc(report.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
		t.Errorf("Warnings = %v, want one naming %s", report.Warnings, want)
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
// listed them, and says which tickets it gave a key of their own.
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

	landing, err := st.ReplaceFromExport(ctx, backup)
	if err != nil {
		t.Fatalf("ReplaceFromExport: %v", err)
	}
	var named []string
	for _, r := range landing.Rekeyed {
		named = append(named, r.IssueID)
		if r.Shared != shared {
			t.Errorf("%s named as sharing %q, want %q", r.IssueID, r.Shared, shared)
		}
	}
	if !slices.Equal(named, holders) {
		t.Errorf("Rekeyed names %v, want the two holders %v", named, holders)
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
// own, so both land on it. The reconcile separates them, names them, and the
// push carries the separated keys to the other replica.
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
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(A): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(A sync): %v", err)
	}
	placeAboveSecond(rootB, fourth)

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	var named []string
	for _, r := range res.Rekeyed {
		named = append(named, r.IssueID)
	}
	holders := []string{third, fourth}
	slices.Sort(holders)
	if !slices.Equal(named, holders) {
		t.Fatalf("Rekeyed = %+v, want the two epics placed between the same neighbours %v", res.Rekeyed, holders)
	}
	wantOrder := []string{first, holders[0], holders[1], second}
	if got := listedIDs(t, ctx, syncB); !slices.Equal(got, wantOrder) {
		t.Errorf("B lists %v, want %v", got, wantOrder)
	}
	assertNoDuplicateRanks(t, ctx, syncB)
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(B): %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B sync): %v", err)
	}

	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	if _, err := syncA.SyncReceive(ctx, "origin", "master"); err != nil {
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
