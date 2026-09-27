package store

import (
	"context"
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
	pushLive := func(label string, setUpstream bool) storage.SyncPushResult {
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
		result, err := live.SyncPush(ctx, "origin", "master", setUpstream, false)
		if err != nil {
			t.Fatalf("%s: SyncPush() error = %v", label, err)
		}
		if err := live.Close(); err != nil {
			t.Fatalf("%s: Close() error = %v", label, err)
		}
		return result
	}

	c1 := commit("c1")
	if got := pushLive("seed", true); got.Head != c1 {
		t.Fatalf("seed push reported head %q, want c1 %s: the push does not report what it sent", got.Head, c1)
	}
	c2 := commit("c2")
	cloneRoot := filepath.Join(base, "clone", "dolt")
	if err := os.MkdirAll(filepath.Dir(cloneRoot), 0o755); err != nil {
		t.Fatalf("mkdir clone parent: %v", err)
	}
	if err := dbsnapshot.CloneTree(ctx, doltRoot, cloneRoot); err != nil {
		t.Fatalf("CloneTree() error = %v", err)
	}
	c3 := commit("c3")
	if got := pushLive("explicit push of c3", false); got.Head != c3 {
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
	if result.Superseded == "" {
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
