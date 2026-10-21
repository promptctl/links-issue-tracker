package store

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The scale the replay has to survive. These numbers are the ticket's floor for
// what two year-old stores meeting for the first time could look like, and they
// are what a shape that projects every folded commit up front and holds all of
// them at once cannot do in bounded memory: its peak grows as chain × backlog.
const (
	scaleBacklogIssues = 1000
	scaleFoldedCommits = 500
	// scaleEditedIssues concentrates the chain's edits onto a SUBSET of the
	// backlog so that every edited issue is touched more than once — here
	// exactly twice. Spreading one edit per issue would leave the fold's
	// last-write-wins ordering untested at this scale: a replay that dropped
	// every edit to a row but the first, or replayed steps out of order, would
	// still produce the expected final lane if no row is ever written twice.
	// The backlog and the chain keep the ticket's floor; only which rows the
	// edits land on changes, so the memory and time this test bounds are
	// unaffected — one row is written per step either way.
	scaleEditedIssues = scaleFoldedCommits / 2
)

// scaleEditTarget is the issue edit i lands on. It exists so the chain that
// PERFORMS the edits and the assertion that PREDICTS their result derive the
// mapping from one place instead of restating it. Written twice, the two agree
// only by coincidence, and a change to the distribution that missed one would
// leave the test asserting its own bug. [LAW:one-source-of-truth]
func scaleEditTarget(i int) string { return scaleIssueID(i % scaleEditedIssues) }

// scaleHeapGrowthBudget bounds how much RETAINED heap the combine may add over
// what the process already held before it started. This is the ticket's real
// acceptance criterion, so it is worth being precise about why it is measured
// this way and why the number is what it is.
//
// It is measured as GROWTH, not as an absolute ceiling, because an embedded
// Dolt engine over a seeded 1000-issue backlog already retains ~180 MiB before
// the replay does anything. Folding that constant into the budget would let it
// swamp the term under test.
//
// It is measured as RETAINED heap — a forced GC before every sample — rather
// than allocated heap, because garbage is exactly what the two designs do NOT
// differ in. The materializing shape's cost is one projected export per folded
// commit, HELD until the spine is built; that is live, uncollectable memory,
// and it is invisible in a measure that garbage dominates.
//
// The number: the streamed replay measures ~145 MiB of growth at this scale.
// It holds a fixed handful of exports whatever the chain's length, but the
// engine still retains ~117 KiB per folded commit (117, 145 and 205 MiB at
// 250, 500 and 1000 commits), so the bound is a growth at THIS scale and not a
// flat line. The materializing shape would additionally retain
// scaleFoldedCommits projected exports at ~450 KiB each — a 1000-issue export
// with its parent edges — some 220 MiB, for growth near 365 MiB. 240 MiB sits
// between the two with margin on both sides: about 1.66x the streamed cost,
// so ordinary variance cannot fail it, and about 0.66x the materializing one,
// so a reintroduced materialization cannot pass it. Both
// ratios are given as numbers rather than as adjectives because this paragraph
// is the arithmetic anyone moving the budget will check it against, and a
// margin described loosely is one they would have to re-derive to trust.
const scaleHeapGrowthBudget = 240 << 20 // 240 MiB

// scaleWallClockBudget bounds how long the combine may take. Unlike the memory
// bound this is NOT a claim that the cost stopped scaling: reading the backlog
// at each folded commit is still one full export per commit, so time remains
// proportional to chain × backlog.
//
// It is a blowup ceiling and deliberately NOT a regression detector. The
// arithmetic says it cannot be both: the materialized-exports-and-wholesale-
// rewrite shape projects to about 5.3 minutes at this scale, comfortably
// inside this budget. So passing here is no evidence that each step writes
// minimally, and the bound is not tightened toward that number to make it so:
// the minimality tests already own that rule, and they assert how LITTLE each
// step writes, which is the thing a wall-clock number can only ever proxy for.
// What this bound catches is a genuine blowup — an unbounded fold, a step that
// wedged. One rule, one enforcer. [LAW:single-enforcer]
const scaleWallClockBudget = 10 * time.Minute

// TestSyncReconcileCombineIsBoundedOnALargeFoldedChain is the acceptance test
// for links-sync-pgct.13: a combine folding scaleFoldedCommits commits over a
// scaleBacklogIssues-issue backlog completes inside a stated time and memory
// budget, with peak memory not scaling with chain × backlog — and every
// provenance guarantee the replay existed to provide still holding.
//
// It runs the UNRELATED-histories combine on purpose: its folded side is the
// whole local chain rather than just the ahead commits, so it is the harshest
// version of the replay.
func TestSyncReconcileCombineIsBoundedOnALargeFoldedChain(t *testing.T) {
	// serial: no t.Parallel — a throughput benchmark asserting an elapsed bound;
	// CPU contention from parallel tests would fail it for reasons unrelated to
	// the code.
	// [LAW:comments-carry-meaning] the other side of this gate: the inner loop
	// (ci.yml, `just test-short`) skips here; the nightly full lane
	// (.github/workflows/nightly.yml) runs without -short, so this test cannot
	// skip there.
	if testing.Short() {
		t.Skip("scale test: folds 500 commits over a 1000-issue backlog")
	}
	ctx := context.Background()
	base := t.TempDir()
	rootA := filepath.Join(base, "a")
	rootB := filepath.Join(base, "b")
	remoteURL := "file://" + filepath.Join(base, "remote")

	seedUnrelatedBacklogPair(t, ctx, rootA, rootB, remoteURL, scaleBacklogIssues, scaleFoldedCommits)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(B): %v", err)
	}
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}

	peak := watchPeakHeap(t)
	start := time.Now()
	res, err := syncB.SyncReconcileCombine(ctx, remoteName(t, "origin"), "master")
	elapsed := time.Since(start)
	peakHeap := peak.stop()
	if err != nil {
		t.Fatalf("SyncReconcileCombine: %v", err)
	}
	t.Logf("combine folded %d commits over %d issues in %s; retained heap: baseline %d MiB, peak %d MiB, growth %d MiB",
		res.Replayed, scaleBacklogIssues, elapsed.Round(time.Millisecond), peak.baseline>>20, peakHeap>>20, (peakHeap-peak.baseline)>>20)

	if res.State != storage.SyncReconcileCombined {
		t.Fatalf("combine state = %q (pending=%d), want %q", res.State, len(res.Pending), storage.SyncReconcileCombined)
	}
	// Every planted ticket is already inside an epic, so a landing that moves
	// any of them has stopped seeing the planted epic; each moved ticket becomes
	// a lane of one and its lane edit stops counting.
	if len(res.Rehomed) != 0 {
		t.Fatalf("combine rehomed %d planted tickets (first %+v), want none: every planted ticket lives in %s", len(res.Rehomed), res.Rehomed[0], scaleEpicID)
	}
	if growth := peakHeap - peak.baseline; growth > scaleHeapGrowthBudget {
		t.Errorf("combine grew retained heap by %d MiB, over the %d MiB budget: the replay is holding memory proportional to the folded chain again (a projected export per commit, alive at once) instead of streaming one step at a time",
			growth>>20, scaleHeapGrowthBudget>>20)
	}
	if elapsed > scaleWallClockBudget {
		t.Errorf("combine took %s, over the %s budget: this bound is loose enough that a return of the per-step rewrite alone would not reach it, so the fold is not merely slower than it should be — look for an unbounded replay or a step that wedged",
			elapsed.Round(time.Second), scaleWallClockBudget)
	}

	// Bounded is worthless if it is also wrong. Every guarantee the provenance
	// replay exists to provide must survive the streamed, incremental shape.
	if res.Replayed != scaleFoldedCommits+scaleFoldedChainOverhead {
		t.Errorf("replayed provenance commits = %d, want %d: the folded side's per-commit history must land commit-for-commit",
			res.Replayed, scaleFoldedCommits+scaleFoldedChainOverhead)
	}
	assertLinearSpineToRemoteHead(t, ctx, syncB, res.RemoteHead)
	assertWorkingSetClean(t, ctx, syncB)
	assertScratchBranchCleanedUp(t, ctx, syncB)
	assertCombinedBacklogContents(t, ctx, syncB, scaleBacklogIssues, scaleFoldedCommits)

	// The whole point of replaying onto the remote head is that the result is a
	// descendant the remote takes without a merge.
	if _, err := syncB.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("fast-forward SyncPush(B) after combine: %v", err)
	}
}

// scaleFoldedChainOverhead is the three commits B's chain carries besides its
// per-edit commits: the epic and the issue inside it that bootstrap the
// workspace, and the one that plants the backlog. All three are part of the
// folded side and are replayed like any other.
const scaleFoldedChainOverhead = 3

// scalePlantedBirth is the one creation instant every planted row carries on both
// sides of the pair. A ticket's created_at is what identifies it across stores, so
// a fixed value is what makes the two backlogs the same tickets rather than two
// sets that merely agree on ids.
var scalePlantedBirth = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// assertCombinedBacklogContents checks the union actually landed: every planted
// issue is present, and the lane edits the folded chain carried survived onto
// the merged rows. A replay that lost a step, or landed steps in the wrong
// order, shows up here as a wrong lane rather than as a count that happens to
// match.
func assertCombinedBacklogContents(t *testing.T, ctx context.Context, st *Store, issues, commits int) {
	t.Helper()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("export after combine: %v", err)
	}
	byID := make(map[string]model.Issue, len(export.Issues))
	for _, issue := range export.Issues {
		byID[issue.ID] = issue
	}
	if len(byID) != issues+1 {
		t.Errorf("combined backlog holds %d issues, want %d, the planted issues and their epic (both sides planted the same ids, so the union is one backlog)", len(byID), issues+1)
	}
	// Each edit i set lane-i on scaleEditTarget(i), and the edits deliberately
	// overlap, so every edited issue was written more than once and the LAST
	// edit to touch it is the lane that must survive the fold. Building the
	// expectation by replaying the same mapping in the same order is what makes
	// this an ordering assertion rather than a set-membership one.
	wantLane := make(map[string]string, scaleEditedIssues)
	for i := 0; i < commits; i++ {
		wantLane[scaleEditTarget(i)] = fmt.Sprintf("lane-%d", i)
	}
	for id, lane := range wantLane {
		issue, ok := byID[id]
		if !ok {
			t.Errorf("issue %s missing from the combined backlog", id)
			continue
		}
		if issue.Lane != lane {
			t.Errorf("issue %s lane = %q, want %q: the folded chain's edit did not survive the replay", id, issue.Lane, lane)
		}
	}
}

func scaleIssueID(i int) string { return fmt.Sprintf("bench-%05d", i) }

// scaleEpicID is the epic every planted issue lives in, fixed so both stores
// plant the same epic rather than each minting its own.
const scaleEpicID = "bench-epic"

// seedUnrelatedBacklogPair builds two workspaces that initialised
// INDEPENDENTLY against one remote, so their histories share no commit. A holds
// the backlog and has pushed it; B holds the same backlog (same ids, so the
// union is one backlog and the merge does real per-issue field resolution)
// plus a long chain of local single-field edits that have never been pushed.
// B's whole chain is therefore the folded side.
//
// The backlog is planted in ONE commit through rebuildFromExport rather than
// created issue by issue: the chain length under test is the EDIT chain, and
// paying a thousand commits to set up a five-hundred-commit fold would make the
// test's cost setup rather than the thing being measured.
func seedUnrelatedBacklogPair(t *testing.T, ctx context.Context, rootA, rootB, remoteURL string, issues, commits int) {
	t.Helper()

	stA, err := Open(ctx, testEnv(), rootA, "wsA")
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	plantScaleBacklog(t, ctx, stA, issues)
	if err := stA.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
	syncA := openSyncOrFatal(t, ctx, rootA)
	if err := syncA.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(A): %v", err)
	}
	if _, err := syncA.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(A): %v", err)
	}
	if err := syncA.Close(); err != nil {
		t.Fatalf("Close(A sync): %v", err)
	}

	stB, err := Open(ctx, testEnv(), rootB, "wsB")
	if err != nil {
		t.Fatalf("Open(B): %v", err)
	}
	defer func() {
		if err := stB.Close(); err != nil {
			t.Fatalf("Close(B): %v", err)
		}
	}()
	plantScaleBacklog(t, ctx, stB, issues)
	for i := 0; i < commits; i++ {
		if _, err := stB.Apply(ctx, scaleEditTarget(i), storage.Change{Fields: storage.UpdateIssueInput{Lane: strptr(fmt.Sprintf("lane-%d", i))}}); err != nil {
			t.Fatalf("Apply(B edit %d): %v", i, err)
		}
	}
}

// plantScaleBacklog fills a store with issues issues inside one epic in a
// single commit, by cloning real, fully-hydrated rows: the epic from a real
// epic, each issue from one real issue, and each issue's parent edge from that
// issue's own edge. Cloning rows the store itself produced (rather than
// assembling model literals) keeps every derived and unexported field
// consistent, so the export is one the store could have produced on its own.
func plantScaleBacklog(t *testing.T, ctx context.Context, st *Store, issues int) {
	t.Helper()
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "scale epic", Topic: "scale", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic): %v", err)
	}
	seedIssue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "seed", Topic: "scale", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export(seed): %v", err)
	}
	seed := issueByID(t, export, seedIssue.ID)
	var seedEdge model.Relation
	for _, rel := range export.Relations {
		if rel.SrcID == seed.ID && rel.Type == model.RelParentChild {
			seedEdge = rel
		}
	}
	if seedEdge.SrcID == "" {
		t.Fatalf("seed %s has no parent edge in the export", seed.ID)
	}
	// The planted rows replace the seed and its epic alike, planted below the
	// write boundary — so they must already hold the shape that boundary
	// enforces: every ticket inside an epic. A backlog with no epic is one every
	// sync landing rehomes (storage.HomeStranded), and a stranded ticket is a
	// lane of one whatever its Lane says (model.LaneOf), so the chain's lane
	// edits would be edits to a field nothing reads.
	planted := issueByID(t, export, epic.ID)
	planted.ID = scaleEpicID
	planted.CreatedAt = scalePlantedBirth
	export.Issues = make([]model.Issue, 0, issues+1)
	export.Issues = append(export.Issues, planted)
	export.Relations = make([]model.Relation, 0, issues)
	for i := 0; i < issues; i++ {
		issue := seed
		issue.ID = scaleIssueID(i)
		issue.Title = fmt.Sprintf("scale issue %05d", i)
		issue.Rank = fmt.Sprintf("r%05d", i)
		// Both stores plant this same backlog, and the pair is meant to hold ONE
		// backlog reached by two histories — the shape a clone or a seeded init
		// produces. Left as each store's own seed time, the two sides would be
		// 1000 ids each naming a ticket minted independently on both machines,
		// which is an id collision (merge.Classify) and not a divergence any
		// combine may merge. Pinning the birth certificate is what makes the
		// fixture mean what its name says. [LAW:one-source-of-truth]
		issue.CreatedAt = scalePlantedBirth
		export.Issues = append(export.Issues, issue)
		edge := seedEdge
		edge.SrcID, edge.DstID, edge.CreatedAt = issue.ID, scaleEpicID, scalePlantedBirth
		export.Relations = append(export.Relations, edge)
	}
	// The seed issue's own events reference the id being replaced, so the
	// planted backlog starts with issue rows and their parent edges only.
	export.Events, export.Comments, export.Labels = nil, nil, nil
	if err := st.rebuildFromExport(ctx, export, commitStamp{Message: "plant backlog"}); err != nil {
		t.Fatalf("rebuildFromExport(plant %d issues): %v", issues, err)
	}
}

// heapWatch samples live heap while an operation runs, because the peak that
// matters happens DURING the replay and is gone by the time it returns — a
// materializing shape releases its steps the moment the spine is built, so a
// measurement taken afterwards would report both designs as equal.
type heapWatch struct {
	done     chan struct{}
	wg       sync.WaitGroup
	peak     uint64
	baseline uint64
}

func watchPeakHeap(t *testing.T) *heapWatch {
	t.Helper()
	runtime.GC()
	var at runtime.MemStats
	runtime.ReadMemStats(&at)
	w := &heapWatch{done: make(chan struct{}), baseline: at.HeapAlloc, peak: at.HeapAlloc}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var stats runtime.MemStats
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				runtime.GC()
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > w.peak {
					w.peak = stats.HeapAlloc
				}
			}
		}
	}()
	return w
}

func (w *heapWatch) stop() uint64 {
	close(w.done)
	w.wg.Wait()
	return w.peak
}
