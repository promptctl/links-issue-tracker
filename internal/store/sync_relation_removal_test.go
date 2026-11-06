package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A dependency removed on one clone stays removed when that clone reconciles with
// a remote that moved on without touching the edge. The reconcile reads the
// merge base, so the other side merely keeping the edge is not a vote to restore
// it. Pure merge tests pin the rule; this one pins that the real reconcile hands
// the merge the base that makes it decidable.
func TestSyncReconcileKeepsADependencyThisCloneRemoved(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	epicID := seedReconcileRemote(t, ctx, rootA, remoteURL)

	st, err := Open(ctx, testEnv(), testStderr(), rootA, "ws")
	if err != nil {
		t.Fatalf("Open(A): %v", err)
	}
	waiter := mustCreateTask(t, ctx, st, epicID, "waiter")
	otherEpic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("other"), Lane: storage.SharedLane, Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(other epic): %v", err)
	}
	blocker := mustCreateTask(t, ctx, st, otherEpic.ID, "blocker")
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: waiter.ID, DstID: blocker.ID, Type: model.RelBlockedBy, CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddRelation: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(A): %v", err)
	}
	push(t, ctx, rootA)
	adoptRemote(t, ctx, rootB, remoteURL)

	// B drops the dependency; A moves on with an edit that leaves it alone.
	stB, err := Open(ctx, testEnv(), testStderr(), rootB, "ws")
	if err != nil {
		t.Fatalf("Open(B): %v", err)
	}
	if err := stB.RemoveRelation(ctx, waiter.ID, blocker.ID, model.RelBlockedBy); err != nil {
		t.Fatalf("RemoveRelation(B): %v", err)
	}
	if err := stB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	updateAndPush(t, ctx, rootA, blocker.ID, storage.UpdateIssueInput{Lane: strptr("alpha")})

	_, syncB := reconcileB(t, ctx, rootB)
	edges, err := syncB.ListRelationsForIssue(ctx, waiter.ID, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue(B): %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("after the reconcile B holds %+v, want the dependency B removed to stay removed", edges)
	}
	if got := getIssueOrFatal(t, ctx, syncB, blocker.ID); got.Lane != "alpha" {
		t.Fatalf("blocker lane = %q, want A's edit (alpha) merged in", got.Lane)
	}
}

func mustCreateTask(t *testing.T, ctx context.Context, st *Store, epicID model.ID, title string) model.Issue {
	t.Helper()
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle(title), Lane: storage.SharedLane, Topic: "topic", IssueType: "task", ParentID: epicID})
	if err != nil {
		t.Fatalf("CreateIssue(%s): %v", title, err)
	}
	return issue
}
