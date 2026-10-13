package store

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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
	if _, _, err := st.AddComment(ctx, storage.AddCommentInput{IssueID: doomed.ID, Body: "secret", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	backup, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: doomed.ID, Actor: "tester"}); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	if err := st.ReplaceFromExport(ctx, backup); err != nil {
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
