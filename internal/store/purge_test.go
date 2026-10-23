package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/storage/storagetest"
)

// TestRestoreNeverUnpurges pins the restore half of "a purge is permanent": a
// backup taken before the purge still holds the ticket, and restoring it must
// land without the ticket and with the purge record still in place. Without
// that, the restore re-adds the ticket as a local change, the next sync
// pushes it, and every store that dropped it gets it back.
func TestRestoreNeverUnpurges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "purge", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic): %v", err)
	}
	doomed, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Doomed", Topic: "purge", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(doomed): %v", err)
	}
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: doomed.ID, Body: storagetest.CommentBody(t, "secret"), CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	backup, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: doomed.ID, Actor: "tester"}); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if _, err := st.ReplaceFromExport(ctx, backup); err != nil {
		t.Fatalf("ReplaceFromExport(pre-purge backup): %v", err)
	}

	restored, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export after restore: %v", err)
	}
	for _, issue := range restored.Issues {
		if issue.ID == doomed.ID {
			t.Errorf("restore brought back purged issue %s", doomed.ID)
		}
	}
	if len(restored.Comments) != 0 {
		t.Errorf("restore brought back comments %+v", restored.Comments)
	}
	if len(restored.Purges) != 1 || restored.Purges[0].IssueID != doomed.ID {
		t.Errorf("purges after restore = %+v, want the record of %s kept", restored.Purges, doomed.ID)
	}
	if len(restored.Issues) != 1 || restored.Issues[0].ID != epic.ID {
		t.Errorf("issues after restore = %+v, want the epic alone", restored.Issues)
	}
}

// TestPurgingOneOfALeafsTwoParentsKeepsItInside pins the purge's read of the
// parents a child keeps. Restored data can leave a leaf under two epics;
// purging one of them leaves the leaf inside the other, so it goes through,
// and the leaf's last epic is then refused like any occupied container.
func TestPurgingOneOfALeafsTwoParentsKeepsItInside(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first := parentChain(t, ctx, st, "E1")[0]
	second := parentChain(t, ctx, st, "E2")[0]
	child, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "C", Topic: "hier", IssueType: "task", ParentID: first.ID, Placement: storage.RankBottom})
	if err != nil {
		t.Fatalf("CreateIssue(C) error = %v", err)
	}
	seedParentEdge(t, ctx, st, child.ID, second.ID)

	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: second.ID, Actor: "tester"}); err != nil {
		t.Fatalf("Purge(%s) error = %v, want it purged: %s keeps %s", second.ID, err, child.ID, first.ID)
	}
	_, err = st.Purge(ctx, storage.PurgeInput{IssueID: first.ID, Actor: "tester"})
	var refused model.ValidationError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("Purge(%s) error = %v, want a refusal naming %s: it is the leaf's last epic", first.ID, err, child.ID)
	}
}
