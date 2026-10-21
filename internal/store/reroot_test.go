package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// TestReRootGivesTheSameHashesInEveryStore re-roots one history in two
// separate stores at the same cut. Both arrive at byte-equal hashes for the
// root and for every kept commit, every kept commit carries the root value
// and metadata it had, and the store reads back the same data through its SQL
// engine. The kept and dropped counts are checked against dolt_log, read
// before the re-root, and the event carries the same facts.
func TestReRootGivesTheSameHashesInEveryStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	epic := seedCommitEpic(t, ctx, doltRoot)
	for i := range 6 {
		st, err := Open(ctx, testEnv(), doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%d) error = %v", i, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("t%d", i), Topic: "topic", IssueType: "task", ParentID: epic}); err != nil {
			t.Fatalf("CreateIssue(%d) error = %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%d) error = %v", i, err)
		}
	}
	stores := [2]string{}
	for i := range stores {
		stores[i] = filepath.Join(base, fmt.Sprintf("store%d", i), "dolt")
		if err := os.MkdirAll(filepath.Dir(stores[i]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := dbsnapshot.CloneTree(ctx, doltRoot, stores[i]); err != nil {
			t.Fatalf("CloneTree(%d) error = %v", i, err)
		}
	}

	var cut hash.Hash
	var before map[hash.Hash]commitFacts
	withChunkStore(t, stores[0], func(ddb *doltdb.DoltDB) {
		cut = firstParentBelowHead(t, ctx, ddb, 3)
		before = commitsReachedFromHead(t, ctx, ddb)
	})
	exportBefore := exportOf(t, ctx, stores[0])
	total := logLength(t, ctx, stores[0], "HEAD")
	wantDropped := logLength(t, ctx, stores[0], cut.String()) - 1
	wantKept := total - wantDropped

	var results [2]storage.ReRooted
	for i, root := range stores {
		got, err := ReRoot(ctx, root, "master", cut.String())
		if err != nil {
			t.Fatalf("ReRoot(store %d) error = %v", i, err)
		}
		results[i] = got
	}
	a, b := results[0], results[1]
	if a.Root != b.Root || a.Head != b.Head {
		t.Fatalf("root, head = %s, %s in one store and %s, %s in the other", a.Root, a.Head, b.Root, b.Head)
	}
	if !maps.Equal(a.Rewritten, b.Rewritten) {
		t.Fatalf("the kept commits were rewritten to different hashes:\n%v\n%v", a.Rewritten, b.Rewritten)
	}
	if a.Kept() != wantKept || a.Dropped != wantDropped {
		t.Fatalf("kept, dropped = %d, %d; dolt_log says %d, %d", a.Kept(), a.Dropped, wantKept, wantDropped)
	}
	if a.Root == cut.String() {
		t.Fatalf("the root kept the cut's hash %s, so the cut's parents were not dropped", cut)
	}

	withChunkStore(t, stores[0], func(ddb *doltdb.DoltDB) {
		head := branchHead(t, ctx, ddb, "master")
		if head.String() != a.Head {
			t.Fatalf("master = %s, want the rewritten head %s", head, a.Head)
		}
		after := commitsReachedFromHead(t, ctx, ddb)
		if len(after) != wantKept {
			t.Fatalf("master reaches %d commits after the re-root, want %d", len(after), wantKept)
		}
		for old, rewritten := range a.Rewritten {
			was, now := before[hash.Parse(old)], after[hash.Parse(rewritten)]
			if now.value != was.value {
				t.Errorf("%s -> %s: root value %s, want %s: the tables changed", old, rewritten, now.value, was.value)
			}
			if !reflect.DeepEqual(now.meta, was.meta) {
				t.Errorf("%s -> %s: metadata %+v, want %+v", old, rewritten, now.meta, was.meta)
			}
			var wantParents []hash.Hash
			for _, p := range was.parents {
				if np, kept := a.Rewritten[p.String()]; kept {
					wantParents = append(wantParents, hash.Parse(np))
				}
			}
			if !slices.Equal(now.parents, wantParents) {
				t.Errorf("%s -> %s: parents %v, want %v", old, rewritten, now.parents, wantParents)
			}
		}
	})
	if got := logLength(t, ctx, stores[0], "HEAD"); got != wantKept {
		t.Fatalf("dolt_log after the re-root has %d commits, want %d", got, wantKept)
	}
	if exportAfter := exportOf(t, ctx, stores[0]); !reflect.DeepEqual(exportAfter, exportBefore) {
		t.Fatalf("the store reads different data after the re-root:\n%+v\nwant\n%+v", exportAfter, exportBefore)
	}

	// Re-rooting at the root rewrites every commit to itself: what a checkout
	// that already holds the re-rooted history computes when it follows.
	again, err := ReRoot(ctx, stores[0], "master", a.Root)
	if err != nil {
		t.Fatalf("ReRoot(at the root) error = %v", err)
	}
	if again.Head != a.Head || again.Dropped != 0 || again.Kept() != wantKept {
		t.Fatalf("re-root at the root: head %s kept %d dropped %d; want %s, %d, 0", again.Head, again.Kept(), again.Dropped, a.Head, wantKept)
	}
	for old, rewritten := range again.Rewritten {
		if old != rewritten {
			t.Fatalf("re-root at the root rewrote %s to %s", old, rewritten)
		}
	}

	recorder := telemetry.Begin(telemetry.Event{Event: "lit sync push"})
	facts, counts := a.EventRecord()
	telemetry.Record(telemetry.With(ctx, recorder), facts, counts)
	ev := recorder.Finish(telemetry.End{})
	if ev.Facts["reroot.cut"] != cut.String() {
		t.Errorf("reroot.cut = %v, want %s", ev.Facts["reroot.cut"], cut)
	}
	if ev.Facts["reroot.root"] != a.Root || ev.Facts["reroot.head"] != a.Head {
		t.Errorf("reroot.root, reroot.head = %v, %v; want %s, %s", ev.Facts["reroot.root"], ev.Facts["reroot.head"], a.Root, a.Head)
	}
	if ev.Counts["reroot.kept"] != wantKept || ev.Counts["reroot.dropped"] != wantDropped {
		t.Errorf("reroot.kept, reroot.dropped = %d, %d; want %d, %d", ev.Counts["reroot.kept"], ev.Counts["reroot.dropped"], wantKept, wantDropped)
	}
	if a.Elapsed <= 0 || ev.Facts["reroot.elapsed_ms"] != a.Elapsed.Milliseconds() {
		t.Errorf("reroot.elapsed_ms = %v for an elapsed %s", ev.Facts["reroot.elapsed_ms"], a.Elapsed)
	}
}

// TestReRootKeepsEveryMergeAboveTheCutAndRefusesALineAroundIt drives the
// history shapes a sync produces. Above the cut C:
//
//	H0 - a1 - C - a2 - x - m - m2
//	      \    \      /   /   /
//	       \    `- s1 ---'   /
//	        `-------------- t1
//
// x merges a1, an ancestor of C, and drops it; m merges s1, a line that forked
// at C, and keeps both parents. t1 forks at a1, below C, so with m2 as the
// head the cut is refused and master is left where it was. The re-root at C
// runs over the ref a re-root that died part-way leaves, and still writes a
// parentless root.
func TestReRootKeepsEveryMergeAboveTheCutAndRefusesALineAroundIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)

	var c map[string]hash.Hash
	withChunkStore(t, doltRoot, func(ddb *doltdb.DoltDB) {
		h0 := branchHead(t, ctx, ddb, "master")
		c = map[string]hash.Hash{"H0": h0}
		for i, spec := range []struct{ name, parents string }{
			{"a1", "H0"}, {"C", "a1"}, {"a2", "C"}, {"x", "a2 a1"}, {"s1", "C"},
			{"m", "x s1"}, {"t1", "a1"}, {"m2", "m t1"},
		} {
			var parents []*doltdb.Commit
			for _, p := range strings.Fields(spec.parents) {
				parents = append(parents, readCommit(t, ctx, ddb, c[p]))
			}
			value, _, err := commitContent(ctx, readCommit(t, ctx, ddb, h0))
			if err != nil {
				t.Fatal(err)
			}
			meta := &datas.CommitMeta{Name: "t", Email: "t@links.local", Description: spec.name, Timestamp: uint64(1_800_000_000_000 + i), UserTimestamp: int64(1_800_000_000_000 + i)}
			nc, err := ddb.CommitDanglingWithParentCommits(ctx, value, parents, meta)
			if err != nil {
				t.Fatalf("commit %s: %v", spec.name, err)
			}
			if c[spec.name], err = nc.HashOf(); err != nil {
				t.Fatal(err)
			}
		}
		if err := ddb.SetHead(ctx, ref.NewBranchRef("master"), c["m2"]); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := ReRoot(ctx, doltRoot, "master", c["C"].String()); !errors.Is(err, ErrSideLineAcrossCut) || !strings.Contains(err.Error(), c["t1"].String()) {
		t.Fatalf("ReRoot(C) with m2 at the head error = %v, want %v naming t1 %s", err, ErrSideLineAcrossCut, c["t1"])
	}
	for cut, refusal := range map[string]string{
		"s9fpkfkdq4pb0nk2nq8r6n0e4u2hb4ue": "read commit s9fpkfkdq4pb0nk2nq8r6n0e4u2hb4ue: ",
		"not-a-hash":                       `"not-a-hash" is not a Dolt commit hash`,
	} {
		if _, err := ReRoot(ctx, doltRoot, "master", cut); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("ReRoot(%s) error = %v, want the refusal %q", cut, err, refusal)
		}
	}
	withChunkStore(t, doltRoot, func(ddb *doltdb.DoltDB) {
		if head := branchHead(t, ctx, ddb, "master"); head != c["m2"] {
			t.Fatalf("a refused re-root moved master to %s, want m2 %s", head, c["m2"])
		}
		if err := ddb.SetHead(ctx, ref.NewBranchRef("master"), c["m"]); err != nil {
			t.Fatal(err)
		}
		// What a re-root that died between writing the root and deleting its
		// ref leaves behind; committing onto it would give the root a parent.
		if err := ddb.SetHead(ctx, reRootRef, c["H0"]); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := ReRoot(ctx, doltRoot, "master", c["t1"].String()); err == nil || !strings.Contains(err.Error(), "the cut is not an ancestor of the branch head "+c["m"].String()) {
		t.Fatalf("ReRoot(t1) with m at the head error = %v, want the cut named as off the branch head m %s", err, c["m"])
	}

	wantDropped := logLength(t, ctx, doltRoot, c["C"].String()) - 1
	got, err := ReRoot(ctx, doltRoot, "master", c["C"].String())
	if err != nil {
		t.Fatalf("ReRoot(C) with m at the head error = %v", err)
	}
	if got.Kept() != 5 || got.Dropped != wantDropped {
		t.Fatalf("kept, dropped = %d, %d; want 5 (C a2 x s1 m), %d", got.Kept(), got.Dropped, wantDropped)
	}
	rewritten := func(name string) hash.Hash { return hash.Parse(got.Rewritten[c[name].String()]) }
	withChunkStore(t, doltRoot, func(ddb *doltdb.DoltDB) {
		if leftover, err := ddb.HasRef(ctx, reRootRef); err != nil || leftover {
			t.Fatalf("the re-root left %s: %v, %v", reRootRef, leftover, err)
		}
		for name, want := range map[string][]hash.Hash{
			"C":  nil,
			"a2": {rewritten("C")},
			"x":  {rewritten("a2")},
			"s1": {rewritten("C")},
			"m":  {rewritten("x"), rewritten("s1")},
		} {
			parents, err := readCommit(t, ctx, ddb, rewritten(name)).ParentHashes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(parents, want) {
				t.Errorf("%s's parents after the re-root = %v, want %v", name, parents, want)
			}
		}
	})
}

// commitFacts is what a re-root must carry over for one commit.
type commitFacts struct {
	value   hash.Hash
	meta    *datas.CommitMeta
	parents []hash.Hash
}

// withChunkStore opens the store at root below its SQL engine for fn. No
// engine is open on it while a test reads or writes this way.
func withChunkStore(t *testing.T, root string, fn func(ddb *doltdb.DoltDB)) {
	t.Helper()
	ddb, err := openCloneChunkStore(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ddb.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	fn(ddb)
}

func readCommit(t *testing.T, ctx context.Context, ddb *doltdb.DoltDB, addr hash.Hash) *doltdb.Commit {
	t.Helper()
	commit, err := readFullCommit(ctx, ddb, addr)
	if err != nil {
		t.Fatalf("read commit %s: %v", addr, err)
	}
	return commit
}

func branchHead(t *testing.T, ctx context.Context, ddb *doltdb.DoltDB, branch string) hash.Hash {
	t.Helper()
	commit, err := ddb.ResolveCommitRef(ctx, ref.NewBranchRef(branch))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := commit.HashOf()
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func firstParentBelowHead(t *testing.T, ctx context.Context, ddb *doltdb.DoltDB, steps int) hash.Hash {
	t.Helper()
	addr := branchHead(t, ctx, ddb, "master")
	for range steps {
		parents, err := readCommit(t, ctx, ddb, addr).ParentHashes(ctx)
		if err != nil || len(parents) == 0 {
			t.Fatalf("history ends before %d first-parent steps: %v", steps, err)
		}
		addr = parents[0]
	}
	return addr
}

func commitsReachedFromHead(t *testing.T, ctx context.Context, ddb *doltdb.DoltDB) map[hash.Hash]commitFacts {
	t.Helper()
	reached := map[hash.Hash]commitFacts{}
	pending := []hash.Hash{branchHead(t, ctx, ddb, "master")}
	for len(pending) > 0 {
		addr := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, seen := reached[addr]; seen {
			continue
		}
		commit := readCommit(t, ctx, ddb, addr)
		value, meta, err := commitContent(ctx, commit)
		if err != nil {
			t.Fatal(err)
		}
		parents, err := commit.ParentHashes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		reached[addr] = commitFacts{value: value, meta: meta, parents: parents}
		pending = append(pending, parents...)
	}
	return reached
}

// logLength is how many commits dolt_log lists from rev: the SQL engine's
// count, not the re-root's.
func logLength(t *testing.T, ctx context.Context, root, rev string) int {
	t.Helper()
	st, err := Open(ctx, testEnv(), root, "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log(?)`, rev).Scan(&n); err != nil {
		t.Fatalf("count dolt_log(%s): %v", rev, err)
	}
	return n
}

func exportOf(t *testing.T, ctx context.Context, root string) model.Export {
	t.Helper()
	st, err := Open(ctx, testEnv(), root, "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	export.ExportedAt = time.Time{}
	return export
}

// TestSettleReRootKeepsAMoveThatLandedBeforeAFailedRelease pins what a
// re-root's error leaves standing: a move that landed is described even when
// a hold's release failed after it, and a refusal carries only its hold time.
func TestSettleReRootKeepsAMoveThatLandedBeforeAFailedRelease(t *testing.T) {
	released := errors.New("release the commit lock: lock file gone")
	moved := storage.ReRooted{Cut: "c", Root: "r", Head: "h", Rewritten: map[string]string{"c": "r"}, Dropped: 2}

	got, err := settleReRoot(moved, released, "master", "c")
	if !errors.Is(err, ErrReRootLandedThenFailed) || !errors.Is(err, released) || !strings.Contains(err.Error(), "moved it to h") {
		t.Fatalf("a landed move with a failed release: error = %v, want %v naming head h and wrapping %v", err, ErrReRootLandedThenFailed, released)
	}
	if got.Head != "h" || got.Root != "r" || got.Dropped != 2 || got.Kept() != 1 {
		t.Fatalf("a landed move with a failed release: result = %+v, want %+v", got, moved)
	}

	refused := fmt.Errorf("%w: t1 reaches the history below the cut", ErrSideLineAcrossCut)
	got, err = settleReRoot(storage.ReRooted{Elapsed: time.Millisecond}, refused, "master", "c")
	if !errors.Is(err, ErrSideLineAcrossCut) || errors.Is(err, ErrReRootLandedThenFailed) {
		t.Fatalf("a refusal: error = %v, want %v and not %v", err, ErrSideLineAcrossCut, ErrReRootLandedThenFailed)
	}
	if want := (storage.ReRooted{Elapsed: time.Millisecond}); got.Head != "" || got.Kept() != 0 || got.Elapsed != want.Elapsed {
		t.Fatalf("a refusal: result = %+v, want %+v", got, want)
	}

	if got, err = settleReRoot(moved, nil, "master", "c"); err != nil || got.Head != "h" {
		t.Fatalf("a clean re-root: result, error = %+v, %v; want %+v, nil", got, err, moved)
	}
}

// TestWhereTheMoveLeftAsksTheBranch pins that a branch move which returned an
// error is judged by where the branch is, not by the error: Dolt reads the
// moved dataset back after its manifest lands, so an error can follow a move
// that landed.
func TestWhereTheMoveLeftAsksTheBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	moveErr := errors.New("read the dataset back: context deadline exceeded")
	withChunkStore(t, doltRoot, func(ddb *doltdb.DoltDB) {
		at := branchHead(t, ctx, ddb, "master").String()

		landed := storage.ReRooted{Cut: "c", Root: "r", Head: at, Dropped: 1}
		got, err := whereTheMoveLeft(ctx, ddb, ref.NewBranchRef("master"), landed, moveErr)
		if !errors.Is(err, moveErr) || got.Head != at || got.Root != "r" {
			t.Fatalf("master on the rewritten head: result, error = %+v, %v; want %+v wrapping %v", got, err, landed, moveErr)
		}

		elsewhere := storage.ReRooted{Cut: "c", Root: "r", Head: "s9fpkfkdq4pb0nk2nq8r6n0e4u2hb4ue"}
		got, err = whereTheMoveLeft(ctx, ddb, ref.NewBranchRef("master"), elsewhere, moveErr)
		if !errors.Is(err, moveErr) || got.Head != "" {
			t.Fatalf("master off the rewritten head: result, error = %+v, %v; want none, wrapping %v", got, err, moveErr)
		}
	})
}
