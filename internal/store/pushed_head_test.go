package store

import (
	"context"
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

	// The mirror's situation: no engine of its own on the live store.
	if err := RecordPushedHead(ctx, doltRoot, "origin", "master", pushedHead); err != nil {
		t.Fatalf("RecordPushedHead() error = %v", err)
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
	if err := RecordPushedHead(ctx, doltRoot, "origin", "master", "not-a-hash"); err == nil {
		t.Fatal("RecordPushedHead accepted a value that is not a Dolt commit hash")
	}
	// Well-formed, absent: 32 chars of the Dolt hash alphabet naming nothing.
	absent := "00000000000000000000000000000000"
	if err := RecordPushedHead(ctx, doltRoot, "origin", "master", absent); err == nil {
		t.Fatal("RecordPushedHead set a tracking ref to a commit the store does not hold")
	}
}

// TestRecordPushedHeadWaitsOutALiveWriteEngine pins the lock contract: the
// record opens the chunk store under Dolt's LOCK the way a write engine does,
// so against a live write engine it waits — bounded by the write open's own
// retry — and lands once that engine closes, rather than failing at once or
// writing under the holder.
func TestRecordPushedHeadWaitsOutALiveWriteEngine(t *testing.T) {
	// serial: no t.Parallel — engineOpenRetryMaxElapsed governs the wait and
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
	if err := RecordPushedHead(ctx, doltRoot, "origin", "master", head); err != nil {
		t.Fatalf("RecordPushedHead() against a write engine that closes mid-wait error = %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("RecordPushedHead returned while the write engine still held the store; the ref write did not wait for LOCK")
	}
}
