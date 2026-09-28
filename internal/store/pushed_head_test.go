package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRecordPushedHeadMovesTheTrackingRefWithoutTheNetwork drives the
// on-change mirror's shape at the store level: a clone of the live store
// pushes, and the live store — which ran no push and holds no engine — has the
// result recorded on it. The assertion is on the one ref every freshness read
// hangs off: after the record the live store is up to date with the remote
// exactly as if it had pushed itself, and a commit landing afterwards reads as
// ahead by one, so the ref moved to the pushed head and not past it.
func TestRecordPushedHeadMovesTheTrackingRefWithoutTheNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	commit := func(title string) {
		st, err := Open(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
	}
	freshness := func(label string, st *Store) storage.SyncFreshness {
		t.Helper()
		got, err := st.SyncFreshness(ctx, "origin", "master")
		if err != nil {
			t.Fatalf("%s: SyncFreshness() error = %v", label, err)
		}
		return got
	}

	commit("c1")
	live, err := OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := live.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}
	if _, err := live.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("seed push error = %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close() after seed push error = %v", err)
	}

	// The commit the mirror is spawned for: unpushed on the live store.
	commit("c2")

	// The mirror's clone, taken with no engine live on the path, then pushed
	// from its own engine on its own path.
	cloneRoot := filepath.Join(base, "clone", "dolt")
	if err := os.MkdirAll(filepath.Dir(cloneRoot), 0o755); err != nil {
		t.Fatalf("mkdir clone parent: %v", err)
	}
	if err := dbsnapshot.CloneTree(ctx, doltRoot, cloneRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	clone, err := OpenSync(ctx, cloneRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync(clone) error = %v", err)
	}
	if _, err := clone.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(clone) error = %v", err)
	}
	if got := freshness("clone after push", clone); got.State() != storage.SyncUpToDate {
		t.Fatalf("clone after push: state = %q (%+v), want up-to-date", got.State(), got)
	}
	status, err := clone.SyncStatus(ctx)
	if err != nil {
		t.Fatalf("SyncStatus(clone) error = %v", err)
	}
	pushedHead := status.HeadCommit
	if err := clone.Close(); err != nil {
		t.Fatalf("Close(clone) error = %v", err)
	}

	live, err = OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after clone push error = %v", err)
	}
	// Before the record the live store's ref still names the seed push, so c2
	// reads as unpushed — the false "not pushed" every banner would print.
	if got := freshness("live before record", live); got.State() != storage.SyncAhead || got.Ahead != 1 {
		t.Fatalf("live before record: state = %q ahead=%d, want ahead by 1", got.State(), got.Ahead)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close() before record error = %v", err)
	}

	// The mirror's situation: no engine of its own on the live store. The
	// received-refs record the push proved rides along and lands with the ref.
	proven := []byte("origin " + remoteURL + "\nproven-head\trefs/dolt/data\n")
	record, err := RecordPushedHead(ctx, doltRoot, "origin", "master", pushedHead, proven)
	if err != nil {
		t.Fatalf("RecordPushedHead() error = %v", err)
	}
	if record != PushedHeadMoved {
		t.Fatalf("RecordPushedHead() = %s, want moved: the ref named the seed push and the clone pushed past it", record)
	}
	if got, err := ReadReceivedRefs(doltRoot); err != nil || string(got) != string(proven) {
		t.Fatalf("received-refs record after the push was recorded = %q, %v; want %q", got, err, proven)
	}

	live, err = OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after record error = %v", err)
	}
	if got := freshness("live after record", live); got.State() != storage.SyncUpToDate {
		t.Fatalf("live after record: state = %q (%+v), want up-to-date", got.State(), got)
	}
	var strayRefs int64
	if err := live.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_remote_branches WHERE name <> 'remotes/origin/master'`).Scan(&strayRefs); err != nil {
		t.Fatalf("count tracking refs: %v", err)
	}
	if strayRefs != 0 {
		t.Fatalf("the record minted %d tracking ref(s) beside remotes/origin/master", strayRefs)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close() after record error = %v", err)
	}

	// The clone is discarded the way the mirror discards it, and the live
	// store keeps working with nothing pointing at the removed tree.
	if err := os.RemoveAll(filepath.Dir(cloneRoot)); err != nil {
		t.Fatalf("remove clone: %v", err)
	}
	commit("c3")
	live, err = OpenSync(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync() after clone removal error = %v", err)
	}
	defer live.Close()
	if got := freshness("live after c3", live); got.State() != storage.SyncAhead || got.Ahead != 1 {
		t.Fatalf("live after c3: state = %q ahead=%d, want ahead by exactly the one commit after the recorded head", got.State(), got.Ahead)
	}
}

// TestRecordPushedHeadRefusesAHeadTheStoreDoesNotHold pins the loud arm: a
// hash that names no commit in this store is not a head a clone of it could
// have pushed, so recording it is refused rather than minting a tracking ref
// that points at nothing. [LAW:no-silent-failure]
func TestRecordPushedHeadRefusesAHeadTheStoreDoesNotHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	// A refused ref write takes the proven record down with it: the record
	// would stop the receive whose fetch is what repairs the ref.
	standing := []byte("standing\n")
	if err := WriteReceivedRefs(doltRoot, standing); err != nil {
		t.Fatalf("WriteReceivedRefs() error = %v", err)
	}
	proven := []byte("proven\n")
	if _, err := RecordPushedHead(ctx, doltRoot, "origin", "master", "not-a-hash", proven); err == nil {
		t.Fatal("RecordPushedHead accepted a value that is not a Dolt commit hash")
	}
	// Well-formed, absent: 32 chars of the Dolt hash alphabet naming nothing.
	absent := "00000000000000000000000000000000"
	if _, err := RecordPushedHead(ctx, doltRoot, "origin", "master", absent, proven); err == nil {
		t.Fatal("RecordPushedHead set a tracking ref to a commit the store does not hold")
	}
	if got, err := ReadReceivedRefs(doltRoot); err != nil || string(got) != string(standing) {
		t.Fatalf("received-refs record after refused records = %q, %v; want the standing %q", got, err, standing)
	}
}

// TestRecordPushedHeadWaitsOutALiveWriteEngine pins the lock contract: the
// record opens the chunk store under Dolt's LOCK the way a write engine does,
// so against a live write engine it waits — bounded by the write open's own
// retry — and lands once that engine closes, rather than failing at once or
// writing under the holder.
func TestRecordPushedHeadWaitsOutALiveWriteEngine(t *testing.T) {
	// serial: no t.Parallel — coResidentHolderWait governs the wait and
	// a sibling test rewrites it.
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	var head string
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	closed := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = st.Close()
		close(closed)
	}()
	if _, err := RecordPushedHead(ctx, doltRoot, "origin", "master", head, nil); err != nil {
		t.Fatalf("RecordPushedHead() against a write engine that closes mid-wait error = %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("RecordPushedHead returned while the write engine still held the store; the ref write did not wait for LOCK")
	}
}

// TestRecordPushedHeadNeverMovesTheTrackingRefBackwards pins the forward-only
// contract. The record is a push's bookkeeping arriving after the push, and
// nothing serializes the clone's push against the live store's own traffic:
// by the time the record runs, an explicit push from the live store may have
// landed a later commit and moved the ref past the clone's head. A record
// that set the ref back would make every freshness read report that later
// commit as unpushed — the false report the record exists to remove. So a
// ref already at the head, or past it, is left where it is and reported as
// carried; only a ref behind the head moves.
func TestRecordPushedHeadNeverMovesTheTrackingRefBackwards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	commit := func(title string) string {
		t.Helper()
		st, err := Open(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("Open(%q) error = %v", title, err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0}); err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		var head string
		if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
			t.Fatalf("read head after %q: %v", title, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close(%q) error = %v", title, err)
		}
		return head
	}
	pushLive := func(label string, setUpstream bool) {
		t.Helper()
		live, err := OpenSync(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", label, err)
		}
		if setUpstream {
			if err := live.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
				t.Fatalf("%s: SyncAddRemote() error = %v", label, err)
			}
		}
		if _, err := live.SyncPush(ctx, "origin", "master", setUpstream, false); err != nil {
			t.Fatalf("%s: SyncPush() error = %v", label, err)
		}
		if err := live.Close(); err != nil {
			t.Fatalf("%s: Close() error = %v", label, err)
		}
	}
	trackingRef := func(label string) string {
		t.Helper()
		live, err := OpenSync(ctx, doltRoot, "ws")
		if err != nil {
			t.Fatalf("%s: OpenSync() error = %v", label, err)
		}
		defer live.Close()
		var hash string
		if err := live.db.QueryRowContext(ctx, `SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/master'`).Scan(&hash); err != nil {
			t.Fatalf("%s: read tracking ref: %v", label, err)
		}
		return hash
	}

	c1 := commit("c1")
	pushLive("seed", true)
	c2 := commit("c2")
	c3 := commit("c3")
	// The explicit push that outran the clone: the live ref now names c3.
	pushLive("explicit push of c3", false)
	if got := trackingRef("after explicit push"); got != c3 {
		t.Fatalf("tracking ref after the explicit push = %s, want c3 %s", got, c3)
	}

	// The clone's record arrives late, for c2 — an ancestor of where the ref is.
	// Its proven advertisement is older than whatever carried the ref past
	// it, so the record on disk stands.
	newer := []byte("newer\n")
	if err := WriteReceivedRefs(doltRoot, newer); err != nil {
		t.Fatalf("WriteReceivedRefs() error = %v", err)
	}
	record, err := RecordPushedHead(ctx, doltRoot, "origin", "master", c2, []byte("late-c2\n"))
	if err != nil {
		t.Fatalf("RecordPushedHead(c2) error = %v", err)
	}
	if record != PushedHeadCarried {
		t.Fatalf("RecordPushedHead(c2) = %s, want carried: the ref was already past c2", record)
	}
	if got := trackingRef("after late record of c2"); got != c3 {
		t.Fatalf("the late record moved the tracking ref backwards to %s; want it left at c3 %s", got, c3)
	}
	if got, err := ReadReceivedRefs(doltRoot); err != nil || string(got) != string(newer) {
		t.Fatalf("received-refs record after a carried-past record = %q, %v; want the newer %q standing", got, err, newer)
	}
	// And for exactly where the ref is: nothing to do to the ref, still not an
	// error, and the ref ends at the pushed head, so the proof is recorded.
	atC3 := []byte("at-c3\n")
	record, err = RecordPushedHead(ctx, doltRoot, "origin", "master", c3, atC3)
	if err != nil {
		t.Fatalf("RecordPushedHead(c3) error = %v", err)
	}
	if record != PushedHeadCarried {
		t.Fatalf("RecordPushedHead(c3) = %s, want carried: the ref already names c3", record)
	}
	if got, err := ReadReceivedRefs(doltRoot); err != nil || string(got) != string(atC3) {
		t.Fatalf("received-refs record after a record at the ref = %q, %v; want %q", got, err, atC3)
	}
	// The forward move still works from this state: a later head moves it.
	c4 := commit("c4")
	record, err = RecordPushedHead(ctx, doltRoot, "origin", "master", c4, nil)
	if err != nil {
		t.Fatalf("RecordPushedHead(c4) error = %v", err)
	}
	if record != PushedHeadMoved {
		t.Fatalf("RecordPushedHead(c4) = %s, want moved", record)
	}
	if got := trackingRef("after record of c4"); got != c4 {
		t.Fatalf("tracking ref after the record of c4 = %s, want %s", got, c4)
	}
	_ = c1
}

// TestRecordPushedHeadCutsAtTheHoldBudget pins that the record's hold on the
// live store runs under MirrorHoldBudget and that a cut is stamped with
// ErrMirrorHoldCut — the discriminator the mirror's trail reads. The budget is
// shrunk to nothing so the hold is cut the instant it starts; the lock waits
// before it are not under the budget, so the open itself still succeeds.
func TestRecordPushedHeadCutsAtTheHoldBudget(t *testing.T) {
	// serial: no t.Parallel — MirrorHoldBudget is a package variable.
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	st, err := Open(ctx, doltRoot, "ws")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	var head string
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	previous := MirrorHoldBudget
	MirrorHoldBudget = time.Nanosecond
	t.Cleanup(func() { MirrorHoldBudget = previous })
	_, err = RecordPushedHead(ctx, doltRoot, "origin", "master", head, nil)
	if !errors.Is(err, ErrMirrorHoldCut) {
		t.Fatalf("RecordPushedHead() under a zero hold budget error = %v, want ErrMirrorHoldCut", err)
	}
}
