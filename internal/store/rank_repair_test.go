package store

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// seq turns a deliberate order into the rankedIssue sequence the repair reads,
// spacing the ranks so every gap can hold a mover.
//
// The width is derived from the run rather than fixed, because these are rank
// strings — compared lexicographically, never numerically. A literal "%02d"
// stops ascending at the tenth id, where "100" sorts between "10" and "20", and
// hands the repair an order that violates its sorted-by-rank precondition.
// Deriving the width means no future caller can outgrow it.
func seq(ids ...string) []rankedIssue {
	width := len(strconv.Itoa(len(ids) * 10))
	order := make([]rankedIssue, len(ids))
	for i, id := range ids {
		order[i] = rankedIssue{id: id, rank: fmt.Sprintf("%0*d", width, (i+1)*10)}
	}
	return order
}

// applyRewrites returns the ids of order as the rewrites leave them: the new
// ranks written, everything else untouched, re-sorted by rank. It is the
// caller's write loop, so a test can assert on the order the store would end up
// holding rather than on the rewrite list's shape.
func applyRewrites(order []rankedIssue, rewrites []rankRewrite) []string {
	final := make([]rankedIssue, len(order))
	copy(final, order)
	for _, rewrite := range rewrites {
		for i := range final {
			if final[i].id == rewrite.id {
				final[i].rank = rewrite.newRank
			}
		}
	}
	sort.SliceStable(final, func(i, j int) bool { return final[i].rank < final[j].rank })
	ids := make([]string, len(final))
	for i, item := range final {
		ids[i] = item.id
	}
	return ids
}

func rewrittenIDs(rewrites []rankRewrite) []string {
	ids := make([]string, len(rewrites))
	for i, rewrite := range rewrites {
		ids[i] = rewrite.id
	}
	sort.Strings(ids)
	return ids
}

// The defect in one assertion: a band that waits on nothing must survive the
// repair in the order it was ranked, whatever order its ids would sort into. The observed failure was a 28-ticket band
// blocking one gate coming back alphabetical end to end (links-doctor-e91j),
// so the fixture is ranked against its own id order — an implementation that
// re-sorts by id cannot pass it by luck.
func TestRepairRankOrderKeepsBandOrderWhileSinkingTheirDependent(t *testing.T) {
	t.Parallel()
	order := seq("zulu", "yankee", "xray", "gate", "whiskey", "victor")
	edges := []blocksEdge{
		{dependent: "gate", dependency: "zulu"},
		{dependent: "gate", dependency: "yankee"},
		{dependent: "gate", dependency: "xray"},
		{dependent: "gate", dependency: "whiskey"},
		{dependent: "gate", dependency: "victor"},
	}

	rewrites, err := repairRankOrder(order, edges)
	if err != nil {
		t.Fatalf("repairRankOrder() error = %v", err)
	}
	if got := rewrittenIDs(rewrites); len(got) != 1 || got[0] != "gate" {
		t.Fatalf("repairRankOrder() rewrote %v, want only [gate] — the band waits on nothing, so nothing in it had to move", got)
	}
	want := []string{"zulu", "yankee", "xray", "whiskey", "victor", "gate"}
	if got := applyRewrites(order, rewrites); !equalIDs(got, want) {
		t.Fatalf("order after repair = %v, want %v", got, want)
	}
}

// One edge, one mover: b waits on e, so b falls behind everything up to e and
// nothing else changes place.
func TestRepairRankOrderMovesOnlyWhatTheEdgeForces(t *testing.T) {
	t.Parallel()
	order := seq("a", "b", "c", "d", "e", "f")
	// b depends on e, so e must be placed first — the one constraint in the set.
	rewrites, err := repairRankOrder(order, []blocksEdge{{dependent: "b", dependency: "e"}})
	if err != nil {
		t.Fatalf("repairRankOrder() error = %v", err)
	}
	if got := rewrittenIDs(rewrites); len(got) != 1 || got[0] != "b" {
		t.Fatalf("repairRankOrder() rewrote %v, want only [b]", got)
	}
	want := []string{"a", "c", "d", "e", "b", "f"}
	if got := applyRewrites(order, rewrites); !equalIDs(got, want) {
		t.Fatalf("order after repair = %v, want %v", got, want)
	}
}

// A backlog that already satisfies every edge is not a backlog to rewrite.
func TestRepairRankOrderWritesNothingWhenAlreadyOrdered(t *testing.T) {
	t.Parallel()
	order := seq("first", "second", "third")
	rewrites, err := repairRankOrder(order, []blocksEdge{{dependent: "third", dependency: "first"}})
	if err != nil {
		t.Fatalf("repairRankOrder() error = %v", err)
	}
	if len(rewrites) != 0 {
		t.Fatalf("repairRankOrder() = %v, want no writes for an order that already satisfies its edges", rewrites)
	}
}

// A duplicated edge row must not leave its dependent permanently blocked: the
// same constraint stated twice is the same constraint.
func TestRepairRankOrderToleratesDuplicateEdges(t *testing.T) {
	t.Parallel()
	order := seq("a", "b")
	edge := blocksEdge{dependent: "a", dependency: "b"}
	rewrites, err := repairRankOrder(order, []blocksEdge{edge, edge})
	if err != nil {
		t.Fatalf("repairRankOrder() error = %v", err)
	}
	if got := applyRewrites(order, rewrites); !equalIDs(got, []string{"b", "a"}) {
		t.Fatalf("order after repair = %v, want [b a]", got)
	}
}

// No rank order satisfies a cycle, so the repair says so and names it rather
// than returning a sequence that quietly drops the unplaceable work.
func TestRepairRankOrderRefusesCycle(t *testing.T) {
	t.Parallel()
	order := seq("a", "b")
	_, err := repairRankOrder(order, []blocksEdge{
		{dependent: "a", dependency: "b"},
		{dependent: "b", dependency: "a"},
	})
	if err == nil {
		t.Fatal("repairRankOrder() = nil error, want a cycle refusal")
	}
	if !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a") {
		t.Fatalf("repairRankOrder() error = %v, want it to name the cycle members", err)
	}
}

// An unranked row cannot bound a gap — nothing sorts below the empty rank — so
// it is always a mover, which hands it a real rank on the way past.
func TestRepairRankOrderGivesUnrankedIssuesARank(t *testing.T) {
	t.Parallel()
	order := []rankedIssue{{id: "unranked", rank: ""}, {id: "ranked", rank: "10"}}
	rewrites, err := repairRankOrder(order, nil)
	if err != nil {
		t.Fatalf("repairRankOrder() error = %v", err)
	}
	if got := rewrittenIDs(rewrites); len(got) != 1 || got[0] != "unranked" {
		t.Fatalf("repairRankOrder() rewrote %v, want only [unranked]", got)
	}
	if rewrites[0].newRank >= "10" {
		t.Fatalf("unranked issue placed at %q, want a rank above %q that keeps it first", rewrites[0].newRank, "10")
	}
}

// A gap bounded by ranks that pad to the same value holds nothing, so those two
// ranks cannot both anchor a run with a mover between them.
func TestRepairRankOrderNeverAnchorsAGapWithNoRoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		order []rankedIssue
		edge  blocksEdge
		want  []string
	}{
		{
			name:  "ranks differing only by trailing zeros",
			order: []rankedIssue{{id: "u", rank: "U"}, {id: "v", rank: "V"}, {id: "v0", rank: "V0"}, {id: "w", rank: "W"}},
			edge:  blocksEdge{dependent: "v0", dependency: "w"},
			want:  []string{"u", "v", "w", "v0"},
		},
		{
			name:  "an all-zero rank",
			order: []rankedIssue{{id: "zero", rank: "0"}, {id: "one", rank: "1"}},
			edge:  blocksEdge{dependent: "zero", dependency: "one"},
			want:  []string{"one", "zero"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewrites, err := repairRankOrder(tc.order, []blocksEdge{tc.edge})
			if err != nil {
				t.Fatalf("repairRankOrder() error = %v", err)
			}
			if len(rewrites) != 1 {
				t.Fatalf("repairRankOrder() rewrote %v, want exactly one issue", rewrittenIDs(rewrites))
			}
			if got := applyRewrites(tc.order, rewrites); !equalIDs(got, tc.want) {
				t.Fatalf("order after repair = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// deliberatelyRank ranks ids into exactly the given order at the top of the
// backlog and fails the test if that order happens to be the ids' own sort
// order — a fixture that is already alphabetical cannot witness alphabetizing.
func deliberatelyRank(t *testing.T, ctx context.Context, st *Store, ids []string) {
	t.Helper()
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	if equalIDs(ids, sorted) {
		t.Fatalf("fixture order %v is already the ids' sort order; it cannot detect a repair that sorts by id", ids)
	}
	if _, err := st.RankSet(ctx, ids); err != nil {
		t.Fatalf("RankSet(%v) error = %v", ids, err)
	}
}

// ranksByID reads the stored rank of every named issue.
func ranksByID(t *testing.T, ctx context.Context, st *Store, ids []string) map[string]string {
	t.Helper()
	ranks := make(map[string]string, len(ids))
	for _, id := range ids {
		issue, err := st.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", id, err)
		}
		ranks[id] = issue.Rank
	}
	return ranks
}

// orderByRank returns ids sorted the way the backlog sorts them.
func orderByRank(ranks map[string]string) []string {
	ids := make([]string, 0, len(ranks))
	for id := range ranks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ranks[ids[i]] < ranks[ids[j]] })
	return ids
}

func createRankTestIssue(t *testing.T, ctx context.Context, st *Store, title string) string {
	t.Helper()
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "rank", IssueType: "task", Priority: 0, Placement: storage.RankBottom,
	})
	if err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", title, err)
	}
	return issue.ID
}

// Acceptance for links-doctor-e91j, end to end through the store: a backlog in
// a known deliberate order with exactly one inversion comes back with exactly
// one ticket moved, every other ticket holding the byte-identical rank it went
// in with, and no inversions left.
func TestFixRankInversionsMovesOnlyTheInvertedTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := make([]string, 0, 6)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot"} {
		ids = append(ids, createRankTestIssue(t, ctx, st, title))
	}
	// Rank them in reverse id order, so the deliberate order is exactly the one
	// an id-sorting repair would destroy.
	deliberate := append([]string(nil), ids...)
	sort.Sort(sort.Reverse(sort.StringSlice(deliberate)))
	deliberatelyRank(t, ctx, st, deliberate)
	before := ranksByID(t, ctx, st, deliberate)

	// One inversion: the ticket at position 1 depends on the ticket at
	// position 4, which is ranked below it.
	dependent, dependency := deliberate[1], deliberate[4]
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: dependent, DstID: dependency, Type: "blocks", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation(%s blocked by %s) error = %v", dependent, dependency, err)
	}
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if report.RankInversions != 1 {
		t.Fatalf("Doctor(before).RankInversions = %d, want exactly 1", report.RankInversions)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 1 {
		t.Fatalf("FixRankInversions() = %d, want 1 — only the inverted ticket had to move", fixed)
	}

	after := ranksByID(t, ctx, st, deliberate)
	for _, id := range deliberate {
		if id == dependent {
			continue
		}
		if after[id] != before[id] {
			t.Fatalf("issue %s rank %q -> %q; it was not inverted, so the repair must not have touched it", id, before[id], after[id])
		}
	}
	want := []string{deliberate[0], deliberate[2], deliberate[3], deliberate[4], deliberate[1], deliberate[5]}
	if got := orderByRank(after); !equalIDs(got, want) {
		t.Fatalf("order after fix = %v, want %v", got, want)
	}

	report, err = st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if report.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", report.RankInversions)
	}
}

// The reported failure, reproduced at scale-in-miniature: one gate blocked by a
// whole band of tickets whose deliberate order is the reverse of their id
// order. The repair must sink the gate below the band and leave the band
// exactly as it found it — the observed bug hoisted every dependency above the
// gate in scan order and returned the band alphabetized (links-doctor-e91j).
func TestFixRankInversionsPreservesTheBandBlockingOneGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	band := make([]string, 0, 6)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot"} {
		band = append(band, createRankTestIssue(t, ctx, st, title))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(band)))
	gate := createRankTestIssue(t, ctx, st, "Gate")
	// The gate sits mid-band, so two of its dependencies are ranked below it.
	deliberate := append(append(append([]string(nil), band[:4]...), gate), band[4:]...)
	deliberatelyRank(t, ctx, st, deliberate)
	before := ranksByID(t, ctx, st, deliberate)

	for _, dependency := range band {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: gate, DstID: dependency, Type: "blocks", CreatedBy: "tester"}); err != nil {
			t.Fatalf("AddRelation(gate blocked by %s) error = %v", dependency, err)
		}
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 1 {
		t.Fatalf("FixRankInversions() = %d, want 1 — sinking the gate satisfies every edge, so no member of the band had to move", fixed)
	}

	after := ranksByID(t, ctx, st, deliberate)
	for _, id := range band {
		if after[id] != before[id] {
			t.Fatalf("band member %s rank %q -> %q; the band waits on nothing and must come out as it went in", id, before[id], after[id])
		}
	}
	if got := orderByRank(after); !equalIDs(got, append(append([]string(nil), band...), gate)) {
		t.Fatalf("order after fix = %v, want the band in its deliberate order followed by the gate %v", got, band)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if report.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", report.RankInversions)
	}
}

// applyToOrder returns the sequence the store would hold after the rewrites:
// the new ranks written, everything else untouched, re-sorted by rank.
func applyToOrder(order []rankedIssue, rewrites []rankRewrite) []rankedIssue {
	final := make([]rankedIssue, len(order))
	copy(final, order)
	for _, rewrite := range rewrites {
		for i := range final {
			if final[i].id == rewrite.id {
				final[i].rank = rewrite.newRank
			}
		}
	}
	sort.SliceStable(final, func(i, j int) bool { return final[i].rank < final[j].rank })
	return final
}

// The repair's contract stated as properties and checked over shapes no
// hand-written fixture would cover: whatever the graph, the result must be a
// permutation of the input that satisfies every edge, an issue may fall behind
// one that stood after it only if a dependency of it is placed no earlier than
// that one, and running the repair again must write nothing. Idempotence is what
// makes the single-pass properties trustworthy — a repair that converges only
// sometimes would still pass a single-pass assertion.
func TestRepairRankOrderProperties(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 300; trial++ {
		size := 1 + rng.Intn(40)
		ids := make([]string, size)
		for i := range ids {
			ids[i] = fmt.Sprintf("t%03d", i)
		}
		order := seq(ids...)

		// Draw edges against a random topological layering, so the constraint
		// set is satisfiable by construction and any refusal is a real defect
		// rather than a fixture that asked for the impossible.
		layer := rng.Perm(size)
		var edges []blocksEdge
		for i := 0; i < size; i++ {
			for j := 0; j < size; j++ {
				if layer[i] < layer[j] && rng.Intn(12) == 0 {
					edges = append(edges, blocksEdge{dependency: ids[i], dependent: ids[j]})
				}
			}
		}

		rewrites, err := repairRankOrder(order, edges)
		if err != nil {
			t.Fatalf("trial %d (size %d, %d edges): repairRankOrder() error = %v, want none for an acyclic graph", trial, size, len(edges), err)
		}
		repaired := applyToOrder(order, rewrites)

		if len(repaired) != size {
			t.Fatalf("trial %d: repaired order has %d issues, want %d — the repair must permute the backlog, not resize it", trial, len(repaired), size)
		}
		seen := make(map[string]struct{}, size)
		for _, item := range repaired {
			seen[item.id] = struct{}{}
		}
		if len(seen) != size {
			t.Fatalf("trial %d: repaired order holds %d distinct ids, want %d", trial, len(seen), size)
		}
		if left := invertedEdges(repaired, edges); len(left) != 0 {
			t.Fatalf("trial %d: %d edge(s) still inverted after the repair, want 0", trial, len(left))
		}
		placed := make(map[string]int, size)
		for at, item := range repaired {
			placed[item.id] = at
		}
		dependencies := make(map[string][]string, size)
		for _, e := range edges {
			dependencies[e.dependent] = append(dependencies[e.dependent], e.dependency)
		}
		// seq ranks ids in slice order, so q ranging past p means q stood after p.
		for i, p := range ids {
			for _, q := range ids[i+1:] {
				overtaken := placed[q] < placed[p]
				// q may itself be the dependency p was waiting on.
				waiting := slices.ContainsFunc(dependencies[p], func(d string) bool { return placed[d] >= placed[q] })
				if overtaken && !waiting {
					t.Fatalf("trial %d: %s now stands ahead of %s, yet no dependency of %s is placed at or after %s — an issue may fall behind only while it waits on a dependency", trial, q, p, p, q)
				}
			}
		}
		again, err := repairRankOrder(repaired, edges)
		if err != nil {
			t.Fatalf("trial %d: second repairRankOrder() error = %v", trial, err)
		}
		if len(again) != 0 {
			t.Fatalf("trial %d: second repair wrote %d rank(s), want 0 — the repair must be idempotent", trial, len(again))
		}
	}
}

// Idempotence through the store, with smoothing in the path. The property run
// above never reaches smoothRanksIfNeededTx, so here every planted rank is
// already longer than rank.SmoothingThreshold, and so is each mover's: three
// movers in one call, two sharing a gap, all re-spacing overlapping windows.
// Smoothing may rewrite any rank in the store, but it must leave the repaired
// order standing, and a second call over that store must write nothing at all.
func TestFixRankInversionsTwiceWritesNothingWithSmoothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	ids := make([]string, 0, 8)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel"} {
		ids = append(ids, createRankTestIssue(t, ctx, st, title))
	}
	prefix := strings.Repeat("V", rank.SmoothingThreshold)
	for _, item := range seq(ids...) {
		if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", prefix+item.rank, item.id); err != nil {
			t.Fatalf("plant rank for %s: %v", item.id, err)
		}
	}
	for _, edge := range []struct{ dependent, dependency string }{
		{ids[0], ids[3]},
		{ids[1], ids[3]},
		{ids[5], ids[7]},
	} {
		if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: edge.dependent, DstID: edge.dependency, Type: "blocks", CreatedBy: "tester"}); err != nil {
			t.Fatalf("AddRelation(%s blocked by %s) error = %v", edge.dependent, edge.dependency, err)
		}
	}
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if report.RankInversions != 3 {
		t.Fatalf("Doctor(before).RankInversions = %d, want 3", report.RankInversions)
	}

	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 3 {
		t.Fatalf("FixRankInversions() = %d, want 3 — five of the eight already ascend, so three move", fixed)
	}
	repaired := ranksByID(t, ctx, st, ids)
	want := []string{ids[2], ids[3], ids[0], ids[1], ids[4], ids[6], ids[7], ids[5]}
	if got := orderByRank(repaired); !equalIDs(got, want) {
		t.Fatalf("order after fix = %v, want %v", got, want)
	}

	again, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("second FixRankInversions() error = %v", err)
	}
	if again != 0 {
		t.Fatalf("second FixRankInversions() = %d, want 0 — the store was already repaired", again)
	}
	for id, after := range ranksByID(t, ctx, st, ids) {
		if after != repaired[id] {
			t.Fatalf("second FixRankInversions() moved %s from %q to %q; a repaired store must come back byte-identical", id, repaired[id], after)
		}
	}
	report, err = st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(after) error = %v", err)
	}
	if report.RankInversions != 0 {
		t.Fatalf("Doctor(after).RankInversions = %d, want 0", report.RankInversions)
	}
}
