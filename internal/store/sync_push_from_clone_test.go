package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestSyncPushFromCloneReportsARaceItLostAsSuperseded builds the race the
// on-change mirror can lose: a clone is taken at c2, the live store commits
// c3 and pushes it first, and only then does the clone push. The remote
// rejects c2 as behind c3 — and the remote carries c2 all the same, because
// c3 descends from it. The clone push reports that as superseded, with the
// rejection kept in the result and the clone's head reported, rather than as
// a failed push over a remote that is fully up to date.
func TestSyncPushFromCloneReportsARaceItLostAsSuperseded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	doltRoot := migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	epicID := seedSyncEpic(t, ctx, doltRoot, "ws")

	c1 := commitTitled(t, ctx, doltRoot, epicID, "c1")
	if got := pushSyncFrom(t, ctx, doltRoot, remoteURL, "seed"); got.Head != c1 {
		t.Fatalf("seed push reported head %q, want c1 %s: the push does not report what it sent", got.Head, c1)
	}
	c2 := commitTitled(t, ctx, doltRoot, epicID, "c2")
	cloneRoot := cloneStoreTree(t, ctx, doltRoot, base)
	c3 := commitTitled(t, ctx, doltRoot, epicID, "c3")
	if got := pushSyncFrom(t, ctx, doltRoot, "", "explicit push of c3"); got.Head != c3 {
		t.Fatalf("explicit push reported head %q, want c3 %s", got.Head, c3)
	}

	clone, err := OpenSync(ctx, cloneRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync(clone) error = %v", err)
	}
	defer clone.Close()
	result, err := clone.SyncPushFromClone(ctx, "origin", "master", false, false)
	if err != nil {
		t.Fatalf("SyncPushFromClone() after the live store outran it error = %v; want superseded, the remote carries c2 inside c3", err)
	}
	if !errors.As(result.Superseded, new(pushRefusedError)) {
		t.Fatalf("SyncPushFromClone() reported a landed push (%+v); the remote rejected c2 as behind c3 and that rejection must stay in the record", result)
	}
	if result.Head != c2 {
		t.Fatalf("SyncPushFromClone() head = %q, want the clone's own head c2 %s", result.Head, c2)
	}
	// A plain push from the same clone is the rejection, unexplained: the
	// clone variant is what adds the judgment, not the push.
	if _, err := clone.SyncPush(ctx, "origin", "master", false, false); err == nil {
		t.Fatal("SyncPush() from the outrun clone succeeded; the race this test builds did not happen")
	}
}

// TestAPushRefusedOverAPeersCommitsIsDiverged builds the race two checkouts
// lose when both write: a clone is taken at c1, the live store commits c2
// and pushes it, and the clone commits c3. The remote refuses the clone's
// push, and the clone and the remote each hold a commit the other lacks, so
// every push step reports the refusal as storage.ErrPushDiverged.
func TestAPushRefusedOverAPeersCommitsIsDiverged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	race := buildDivergedClone(t, ctx)
	clone, err := OpenSync(ctx, race.cloneRoot, "ws")
	if err != nil {
		t.Fatalf("OpenSync(clone) error = %v", err)
	}
	defer clone.Close()
	steps := []struct {
		name string
		push func(context.Context, string, string, bool, bool) (storage.SyncPushResult, error)
	}{
		{"SyncPushFromClone", clone.SyncPushFromClone},
		{"SyncCompactAndPush", clone.SyncCompactAndPush},
		{"SyncPush", clone.SyncPush},
	}
	for _, step := range steps {
		if _, err := step.push(ctx, "origin", "master", false, false); !errors.Is(err, storage.ErrPushDiverged) {
			t.Fatalf("%s() over the peer's c2 error = %v; want storage.ErrPushDiverged", step.name, err)
		}
	}
}

// divergedClone is the race TestAPushRefusedOverAPeersCommitsIsDiverged
// builds: the live store at liveRoot pushed c2 to the remote at remoteDir,
// and the clone at cloneRoot, taken at c1, holds c3.
type divergedClone struct {
	liveRoot, cloneRoot, remoteDir, epicID string
}

func buildDivergedClone(t *testing.T, ctx context.Context) divergedClone {
	t.Helper()
	base := t.TempDir()
	race := divergedClone{liveRoot: migratedDoltDir(t), remoteDir: filepath.Join(base, "remote")}
	race.epicID = seedSyncEpic(t, ctx, race.liveRoot, "ws")
	commitTitled(t, ctx, race.liveRoot, race.epicID, "c1")
	pushSyncFrom(t, ctx, race.liveRoot, "file://"+race.remoteDir, "seed")
	race.cloneRoot = cloneStoreTree(t, ctx, race.liveRoot, base)
	commitTitled(t, ctx, race.liveRoot, race.epicID, "c2")
	pushSyncFrom(t, ctx, race.liveRoot, "", "the peer's push of c2")
	commitTitled(t, ctx, race.cloneRoot, race.epicID, "c3")
	return race
}

// commitTitled creates an issue titled title in the store at root and
// returns the head commit it left.
func commitTitled(t *testing.T, ctx context.Context, root, epicID, title string) string {
	t.Helper()
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(%q) error = %v", title, err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "task", Priority: 0, ParentID: epicID}); err != nil {
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

// pushSyncFrom pushes the store at root to origin's master. A non-empty
// addRemote adds origin at that URL first and sets the upstream.
func pushSyncFrom(t *testing.T, ctx context.Context, root, addRemote, label string) storage.SyncPushResult {
	t.Helper()
	live, err := OpenSync(ctx, root, "ws")
	if err != nil {
		t.Fatalf("%s: OpenSync() error = %v", label, err)
	}
	if addRemote != "" {
		if err := live.SyncAddRemote(ctx, "origin", addRemote); err != nil {
			t.Fatalf("%s: SyncAddRemote() error = %v", label, err)
		}
	}
	result, err := live.SyncPush(ctx, "origin", "master", addRemote != "", false)
	if err != nil {
		t.Fatalf("%s: SyncPush() error = %v", label, err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("%s: Close() error = %v", label, err)
	}
	return result
}

// cloneStoreTree copies the store at root to a clone under base, the way the
// on-change mirror takes its clone, and returns the clone's root.
func cloneStoreTree(t *testing.T, ctx context.Context, root, base string) string {
	t.Helper()
	cloneRoot := filepath.Join(base, "clone", "dolt")
	if err := os.MkdirAll(filepath.Dir(cloneRoot), 0o755); err != nil {
		t.Fatalf("mkdir clone parent: %v", err)
	}
	if err := dbsnapshot.CloneTree(ctx, root, cloneRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	return cloneRoot
}
