package store

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
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

// A band that waits on nothing must survive the repair in the order it was
// ranked, whatever order its ids would sort into. The fixture is ranked against
// its own id order — an implementation that re-sorts by id cannot pass it by
// luck.
func TestRepairRankOrderKeepsBandOrderWhileSinkingTheirDependent(t *testing.T) {
	t.Parallel()
	order := seq("zulu", "yankee", "xray", "gate", "whiskey", "victor")
	edges := []storage.DependencyEdge{
		{Dependent: "gate", Dependency: "zulu"},
		{Dependent: "gate", Dependency: "yankee"},
		{Dependent: "gate", Dependency: "xray"},
		{Dependent: "gate", Dependency: "whiskey"},
		{Dependent: "gate", Dependency: "victor"},
	}

	rewrites, err := repairRankOrder(order, nil, edges)
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
	rewrites, err := repairRankOrder(order, nil, []storage.DependencyEdge{{Dependent: "b", Dependency: "e"}})
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
	rewrites, err := repairRankOrder(order, nil, []storage.DependencyEdge{{Dependent: "third", Dependency: "first"}})
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
	edge := storage.DependencyEdge{Dependent: "a", Dependency: "b"}
	rewrites, err := repairRankOrder(order, nil, []storage.DependencyEdge{edge, edge})
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
	_, err := repairRankOrder(order, nil, []storage.DependencyEdge{
		{Dependent: "a", Dependency: "b"},
		{Dependent: "b", Dependency: "a"},
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
	rewrites, err := repairRankOrder(order, nil, nil)
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

// A held issue — one no listing reads — keeps its key and is stepped around,
// whatever the sort does to the issues around it. It is written only where its
// key cannot anchor: shared, where one holder keeps it, or all zeros, which
// gets a real key. An unranked one holds no key and is never written.
func TestRepairRankOrderWritesAHeldIssueOnlyWhereItSharesAKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		order   []rankedIssue
		held    []rankedIssue
		edges   []storage.DependencyEdge
		written []string
		listed  []string
	}{
		{
			name:    "the sort overtakes a held key",
			order:   []rankedIssue{{id: "a", rank: "a"}, {id: "c", rank: "c"}, {id: "b", rank: "d"}},
			held:    []rankedIssue{{id: "x", rank: "b"}},
			edges:   []storage.DependencyEdge{{Dependent: "a", Dependency: "b"}, {Dependent: "c", Dependency: "a"}},
			written: []string{"b"},
			listed:  []string{"b", "a", "c"},
		},
		{
			name:    "a held key above a waiting run",
			order:   []rankedIssue{{id: "p", rank: "a"}, {id: "r", rank: "ab"}, {id: "s", rank: "ac"}, {id: "q", rank: "c"}},
			held:    []rankedIssue{{id: "x", rank: "b"}},
			edges:   []storage.DependencyEdge{{Dependent: "p", Dependency: "q"}, {Dependent: "r", Dependency: "q"}, {Dependent: "s", Dependency: "q"}},
			written: []string{"q"},
			listed:  []string{"q", "p", "r", "s"},
		},
		{
			name:    "a held key an ordered issue shares",
			order:   []rankedIssue{{id: "a", rank: "a"}, {id: "b", rank: "b"}},
			held:    []rankedIssue{{id: "x", rank: "a0"}},
			written: []string{"x"},
			listed:  []string{"a", "b"},
		},
		{
			name:    "a key two held issues share",
			order:   []rankedIssue{{id: "a", rank: "a"}},
			held:    []rankedIssue{{id: "x", rank: "b"}, {id: "y", rank: "b"}},
			written: []string{"y"},
			listed:  []string{"a"},
		},
		{
			name:    "an unranked held issue, an all-zero one and a malformed one",
			order:   []rankedIssue{{id: "a", rank: "a"}, {id: "b", rank: "b"}},
			held:    []rankedIssue{{id: "unranked", rank: ""}, {id: "zero", rank: "00"}, {id: "malformed", rank: "c-"}},
			edges:   []storage.DependencyEdge{{Dependent: "a", Dependency: "b"}},
			written: []string{"b", "malformed", "zero"},
			listed:  []string{"b", "a"},
		},
		{
			name:    "an all-zero key two held issues share",
			order:   []rankedIssue{{id: "a", rank: "a"}},
			held:    []rankedIssue{{id: "x", rank: "0"}, {id: "y", rank: "0"}},
			written: []string{"x", "y"},
			listed:  []string{"a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewrites, err := repairRankOrder(tc.order, tc.held, tc.edges)
			if err != nil {
				t.Fatalf("repairRankOrder() error = %v", err)
			}
			if got := rewrittenIDs(rewrites); !equalIDs(got, tc.written) {
				t.Fatalf("repairRankOrder() rewrote %v, want %v", got, tc.written)
			}
			final := applyToOrder(slices.Concat(tc.order, tc.held), rewrites)
			holders := map[string]string{}
			for _, item := range final {
				if !rank.Valid(rank.Significant(item.rank)) {
					continue
				}
				if other, taken := holders[rank.Significant(item.rank)]; taken {
					t.Fatalf("%s and %s share %q after the repair", other, item.id, item.rank)
				}
				holders[rank.Significant(item.rank)] = item.id
			}
			listed := make([]string, 0, len(tc.listed))
			for _, item := range final {
				if slices.ContainsFunc(tc.order, func(o rankedIssue) bool { return o.id == item.id }) {
					listed = append(listed, item.id)
				}
			}
			if !equalIDs(listed, tc.listed) {
				t.Fatalf("ordered issues list as %v after the repair, want %v", listed, tc.listed)
			}
		})
	}
}

// A gap bounded by ranks that pad to the same value holds nothing, so those two
// ranks cannot both anchor a run with a mover between them.
func TestRepairRankOrderNeverAnchorsAGapWithNoRoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		order []rankedIssue
		edge  storage.DependencyEdge
		want  []string
	}{
		{
			name:  "ranks differing only by trailing zeros",
			order: []rankedIssue{{id: "u", rank: "U"}, {id: "v", rank: "V"}, {id: "v0", rank: "V0"}, {id: "w", rank: "W"}},
			edge:  storage.DependencyEdge{Dependent: "v0", Dependency: "w"},
			want:  []string{"u", "v", "w", "v0"},
		},
		{
			name:  "an all-zero rank",
			order: []rankedIssue{{id: "zero", rank: "0"}, {id: "one", rank: "1"}},
			edge:  storage.DependencyEdge{Dependent: "zero", Dependency: "one"},
			want:  []string{"one", "zero"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewrites, err := repairRankOrder(tc.order, nil, []storage.DependencyEdge{tc.edge})
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

// createRankTestEpic creates the epic a test's ranked issues share, so they
// are one frame.
func createRankTestEpic(t *testing.T, ctx context.Context, st *Store) string {
	t.Helper()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Rank epic", Topic: "rank", IssueType: "epic",
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	return epic.ID
}

func createRankTestIssue(t *testing.T, ctx context.Context, st *Store, epic, title string) string {
	t.Helper()
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "rank", IssueType: "task", Priority: 0, Placement: storage.RankBottom, ParentID: epic,
	})
	if err != nil {
		t.Fatalf("CreateIssue(%s) error = %v", title, err)
	}
	return issue.ID
}

// End to end through the store: a backlog in a known deliberate order with
// exactly one inversion comes back with exactly one ticket moved, every other
// ticket holding the byte-identical rank it went in with, and no inversions
// left. The moved ticket is stamped as updated by the repair; no other is.
func TestFixRankInversionsMovesOnlyTheInvertedTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := createRankTestEpic(t, ctx, st)

	ids := make([]string, 0, 6)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot"} {
		ids = append(ids, createRankTestIssue(t, ctx, st, epic, title))
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
	storagetest.StoreBlocksWithinEpic(t, ctx, st, "test", dependent, dependency)
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor(before) error = %v", err)
	}
	if report.RankInversions != 1 {
		t.Fatalf("Doctor(before).RankInversions = %d, want exactly 1", report.RankInversions)
	}

	fixedAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	st.clock = func() time.Time { return fixedAt }
	fixed, err := st.FixRankInversions(ctx)
	if err != nil {
		t.Fatalf("FixRankInversions() error = %v", err)
	}
	if fixed != 1 {
		t.Fatalf("FixRankInversions() = %d, want 1 — only the inverted ticket had to move", fixed)
	}

	after := ranksByID(t, ctx, st, deliberate)
	for _, id := range deliberate {
		issue, err := st.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v", id, err)
		}
		if stamped := issue.UpdatedAt.Equal(fixedAt); stamped != (id == dependent) {
			t.Fatalf("issue %s updated_at = %s; the repair stamps the ticket it moved and no other", id, issue.UpdatedAt)
		}
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

// One gate blocked by a whole band of tickets whose deliberate order is the
// reverse of their id order. The repair must sink the gate below the band and
// leave the band exactly as it found it.
func TestFixRankInversionsPreservesTheBandBlockingOneGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := createRankTestEpic(t, ctx, st)

	band := make([]string, 0, 6)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot"} {
		band = append(band, createRankTestIssue(t, ctx, st, epic, title))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(band)))
	gate := createRankTestIssue(t, ctx, st, epic, "Gate")
	// The gate sits mid-band, so two of its dependencies are ranked below it.
	deliberate := append(append(append([]string(nil), band[:4]...), gate), band[4:]...)
	deliberatelyRank(t, ctx, st, deliberate)
	before := ranksByID(t, ctx, st, deliberate)

	for _, dependency := range band {
		storagetest.StoreBlocksWithinEpic(t, ctx, st, "test", gate, dependency)
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
		var edges []storage.DependencyEdge
		for i := 0; i < size; i++ {
			for j := 0; j < size; j++ {
				if layer[i] < layer[j] && rng.Intn(12) == 0 {
					edges = append(edges, storage.DependencyEdge{Dependency: ids[i], Dependent: ids[j]})
				}
			}
		}

		rewrites, err := repairRankOrder(order, nil, edges)
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
		if left := invertedEdges(repaired, storage.NewDependencyOrder(storage.RankAncestry{}, edges), edges); len(left) != 0 {
			t.Fatalf("trial %d: %d edge(s) still inverted after the repair, want 0", trial, len(left))
		}
		placed := make(map[string]int, size)
		for at, item := range repaired {
			placed[item.id] = at
		}
		dependencies := make(map[string][]string, size)
		for _, e := range edges {
			dependencies[e.Dependent] = append(dependencies[e.Dependent], e.Dependency)
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
		again, err := repairRankOrder(repaired, nil, edges)
		if err != nil {
			t.Fatalf("trial %d: second repairRankOrder() error = %v", trial, err)
		}
		if len(again) != 0 {
			t.Fatalf("trial %d: second repair wrote %d rank(s), want 0 — the repair must be idempotent", trial, len(again))
		}
	}
}

// The repair's contract over a hierarchy: keys are read only among
// frame-mates, so the properties are stated in tree order, with every
// container's key read back from the repaired store. The edges are drawn
// against a hidden order of the issues, so no issue waits on itself, as the
// write boundary guarantees, while their lifted edges can run both ways
// between frame-mates; and the starting keys are shuffled, so no frame starts
// out agreeing with any order. Some edges join an issue to its own container
// or descendant, which tree order decides and the repair must leave alone.
// About a quarter of the issues are closed, so only the live ones' edges count
// and the rest are held, written only where they share a key, which none here
// does. Whatever interleaves, the repair runs, leaves no inversion for the
// queue to advertise it for, and writes nothing on a second run.
func TestRepairRankOrderPropertiesAcrossFrames(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2))
	keyOf := func(n, i int) string {
		return fmt.Sprintf("%0*d", len(strconv.Itoa(n*10)), (i+1)*10)
	}
	ancestryOver := func(parent map[string]string, key map[string]string) storage.RankAncestry {
		links := make([]storage.ParentLink, 0, len(parent))
		for child, p := range parent {
			links = append(links, storage.ParentLink{ChildID: child, ParentID: p, ParentRank: key[p]})
		}
		return storage.NewRankAncestry(links)
	}
	interleavedTrials := 0
	for trial := 0; trial < 300; trial++ {
		size := 1 + rng.Intn(40)
		ids := make([]string, size)
		parent := map[string]string{}
		for i := range ids {
			ids[i] = fmt.Sprintf("t%03d", i)
			if i > 0 && rng.Intn(3) > 0 {
				parent[ids[i]] = ids[rng.Intn(i)]
			}
		}
		key := map[string]string{}
		for i, at := range rng.Perm(size) {
			key[ids[i]] = keyOf(size, at)
		}
		hiddenAt := map[string]int{}
		for i, at := range rng.Perm(size) {
			hiddenAt[ids[i]] = at
		}
		live := map[string]struct{}{}
		for _, id := range ids {
			if rng.Intn(4) > 0 {
				live[id] = struct{}{}
			}
		}
		var drawn []storage.DependencyEdge
		for _, dependency := range ids {
			for _, dependent := range ids {
				if hiddenAt[dependency] < hiddenAt[dependent] && rng.Intn(8) == 0 {
					drawn = append(drawn, storage.DependencyEdge{Dependent: dependent, Dependency: dependency})
				}
			}
		}
		edges := liveEdges(drawn, live)

		order := make([]rankedIssue, size)
		for i, id := range ids {
			order[i] = rankedIssue{id: id, rank: key[id]}
		}
		slices.SortFunc(order, compareRankedIssues)
		ancestry := ancestryOver(parent, key)
		dependencies := storage.NewDependencyOrder(ancestry, edges)
		if len(dependencies.Interleaved()) > 0 {
			interleavedTrials++
		}
		constraints := dependencies.Constraints()
		ordered, held := splitRankOrder(order, live, constraints)
		rewrites, err := repairRankOrder(ordered, held, constraints)
		if err != nil {
			t.Fatalf("trial %d (size %d, %d edges): repairRankOrder() error = %v, want none: no issue waits on itself", trial, size, len(edges), err)
		}

		repairedKey := maps.Clone(key)
		for _, rewrite := range rewrites {
			if slices.ContainsFunc(held, func(h rankedIssue) bool { return h.id == rewrite.id }) {
				t.Fatalf("trial %d: the repair wrote held issue %s, whose key no other issue shares", trial, rewrite.id)
			}
			repairedKey[rewrite.id] = rewrite.newRank
		}
		holders := map[string]string{}
		for id, k := range repairedKey {
			if other, taken := holders[k]; taken {
				t.Fatalf("trial %d: %s and %s both hold %q after the repair", trial, other, id, k)
			}
			holders[k] = id
		}
		repaired := applyToOrder(order, rewrites)
		repairedAncestry := ancestryOver(parent, repairedKey)
		if left := invertedEdges(repaired, storage.NewDependencyOrder(repairedAncestry, edges), edges); len(left) != 0 {
			t.Fatalf("trial %d: %v still inverted in tree order after the repair, want none", trial, left)
		}
		// Within a frame, an issue falls behind a frame-mate that stood after
		// it only while it waits on a dependency placed no earlier than that one.
		waitsOn := map[string][]string{}
		for _, c := range constraints {
			waitsOn[c.Dependent] = append(waitsOn[c.Dependent], c.Dependency)
		}
		for _, p := range ordered {
			for _, q := range ordered {
				p, q := p.id, q.id
				pRep, qRep, comparable := ancestry.Representatives(p, q)
				if !comparable || pRep != p || qRep != q || key[q] < key[p] {
					continue
				}
				overtaken := repairedKey[q] < repairedKey[p]
				waiting := slices.ContainsFunc(waitsOn[p], func(d string) bool { return repairedKey[d] >= repairedKey[q] })
				if overtaken && !waiting {
					t.Fatalf("trial %d: %s now stands ahead of its frame-mate %s, yet %s waits on nothing placed at or after it", trial, q, p, p)
				}
			}
		}
		repairedConstraints := storage.NewDependencyOrder(repairedAncestry, edges).Constraints()
		orderedAgain, heldAgain := splitRankOrder(repaired, live, repairedConstraints)
		again, err := repairRankOrder(orderedAgain, heldAgain, repairedConstraints)
		if err != nil {
			t.Fatalf("trial %d: second repairRankOrder() error = %v", trial, err)
		}
		if len(again) != 0 {
			t.Fatalf("trial %d: second repair wrote %d rank(s), want 0 — the repair must be idempotent", trial, len(again))
		}
	}
	// Both shapes must occur, or a property above held over only one of them.
	if interleavedTrials == 0 || interleavedTrials == 300 {
		t.Fatalf("%d of 300 trials interleaved, want some and not all", interleavedTrials)
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
	epic := createRankTestEpic(t, ctx, st)

	ids := make([]string, 0, 8)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel"} {
		ids = append(ids, createRankTestIssue(t, ctx, st, epic, title))
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
		storagetest.StoreBlocksWithinEpic(t, ctx, st, "test", edge.dependent, edge.dependency)
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

// Stored ranks that are zero-extensions of one another — "V", "V0", "V00" —
// share one significant part, and no rank longer than two of them sorts between
// them. A smoothing window planted inside such a run has neighbours of that
// kind on both sides, so re-spacing between them is impossible; smoothing must
// widen the window past the run instead of failing the rank write that
// triggered it, and must stop at the first rank outside the run.
func TestSmoothingWidensPastRanksThatLeaveNoRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := createRankTestEpic(t, ctx, st)

	// The run is long enough that a full window around its middle leaves a
	// member of the run on each side: SmoothingWindow/2 at or below the trigger,
	// as many above, and one more past each end.
	run := rank.SmoothingWindow + 2
	ids := make([]string, 0, run+2)
	for i := 0; i < run+2; i++ {
		ids = append(ids, createRankTestIssue(t, ctx, st, epic, fmt.Sprintf("Issue %d", i)))
	}
	planted := make(map[string]string, len(ids))
	planted[ids[0]] = "U"
	for i := 1; i <= run; i++ {
		planted[ids[i]] = "V" + strings.Repeat("0", i-1)
	}
	planted[ids[run+1]] = "W"
	for id, stored := range planted {
		if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", stored, id); err != nil {
			t.Fatalf("plant rank for %s: %v", id, err)
		}
	}
	trigger := planted[ids[1+rank.SmoothingWindow/2]]
	if len(trigger) < rank.SmoothingThreshold {
		t.Fatalf("trigger rank %q is shorter than rank.SmoothingThreshold; the fixture would not smooth at all", trigger)
	}

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := smoothRanksIfNeededTx(ctx, tx, trigger); err != nil {
		_ = tx.Rollback()
		t.Fatalf("smoothRanksIfNeededTx(%q) error = %v, want the window widened past the zero-extension run", trigger, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	after := ranksByID(t, ctx, st, ids)
	if got := orderByRank(after); !equalIDs(got, ids) {
		t.Fatalf("order after smoothing = %v, want %v — smoothing must never reorder", got, ids)
	}
	for _, outside := range []string{ids[0], ids[run+1]} {
		if after[outside] != planted[outside] {
			t.Fatalf("smoothing rewrote %s from %q to %q; the window must stop at the first rank outside the run", outside, planted[outside], after[outside])
		}
	}
	for _, member := range ids[1 : run+1] {
		if after[member] == planted[member] {
			t.Fatalf("%s kept its planted rank %q; every member of the run lies in the widened window and is re-spaced", member, planted[member])
		}
	}
}

// An all-zero run is the same shape with nothing below it: every member's
// significant part is the empty string, which is also the sentinel for the open
// end of the keyspace. The bounds are chosen from those significant parts, so
// this is the case where the lower bound and the run's floor coincide, and
// smoothing must still widen rather than ask for room that does not exist.
func TestSmoothingWidensPastAnAllZeroRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := createRankTestEpic(t, ctx, st)

	run := rank.SmoothingWindow + 2
	ids := make([]string, 0, run+1)
	for i := 0; i < run+1; i++ {
		ids = append(ids, createRankTestIssue(t, ctx, st, epic, fmt.Sprintf("Issue %d", i)))
	}
	planted := make(map[string]string, len(ids))
	for i := 0; i < run; i++ {
		planted[ids[i]] = strings.Repeat("0", rank.SmoothingThreshold+i)
	}
	planted[ids[run]] = "W"
	for id, stored := range planted {
		if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", stored, id); err != nil {
			t.Fatalf("plant rank for %s: %v", id, err)
		}
	}
	trigger := planted[ids[rank.SmoothingWindow/2]]

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := smoothRanksIfNeededTx(ctx, tx, trigger); err != nil {
		_ = tx.Rollback()
		t.Fatalf("smoothRanksIfNeededTx(%q) error = %v, want the window widened past the all-zero run", trigger, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	after := ranksByID(t, ctx, st, ids)
	if got := orderByRank(after); !equalIDs(got, ids) {
		t.Fatalf("order after smoothing = %v, want %v — smoothing must never reorder", got, ids)
	}
	if after[ids[run]] != planted[ids[run]] {
		t.Fatalf("smoothing rewrote %s from %q to %q; the window must stop at the first rank outside the run",
			ids[run], planted[ids[run]], after[ids[run]])
	}
}

// An unranked row's empty key sorts below every other, so a window reaching
// the bottom of the keyspace would sweep it up and hand it a key. A respace
// only re-spaces keys that exist; it never ranks an issue.
func TestSmoothingLeavesUnrankedIssuesUnranked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic := createRankTestEpic(t, ctx, st)

	unranked := createRankTestIssue(t, ctx, st, epic, "Unranked")
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		ids = append(ids, createRankTestIssue(t, ctx, st, epic, fmt.Sprintf("Issue %d", i)))
	}
	planted := map[string]string{unranked: "", ids[0]: "V", ids[1]: "V" + strings.Repeat("0", rank.SmoothingThreshold), ids[2]: "W"}
	for id, stored := range planted {
		if err := st.ExecRawForTest(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", stored, id); err != nil {
			t.Fatalf("plant rank for %s: %v", id, err)
		}
	}

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := smoothRanksIfNeededTx(ctx, tx, planted[ids[1]]); err != nil {
		_ = tx.Rollback()
		t.Fatalf("smoothRanksIfNeededTx error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	after := ranksByID(t, ctx, st, append([]string{unranked}, ids...))
	if after[unranked] != "" {
		t.Fatalf("smoothing ranked %s at %q; an unranked issue is not in any window", unranked, after[unranked])
	}
	if got := orderByRank(map[string]string{ids[0]: after[ids[0]], ids[1]: after[ids[1]], ids[2]: after[ids[2]]}); !equalIDs(got, ids) {
		t.Fatalf("order after smoothing = %v, want %v", got, ids)
	}
	if after[ids[1]] == planted[ids[1]] {
		t.Fatalf("%s kept %q; the fixture did not smooth", ids[1], planted[ids[1]])
	}
}
