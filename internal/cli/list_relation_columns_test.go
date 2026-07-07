package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// lineForID returns the single `lit ls` output line whose first field is id.
func lineForID(t *testing.T, out, id string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, id+" ") || line == id {
			return line
		}
	}
	t.Fatalf("no output line for %q in:\n%s", id, out)
	return ""
}

func fieldsOf(line string) []string {
	parts := strings.Split(line, " | ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// TestListRelationColumns exercises the opt-in parent/blocked projection: parent
// shows the epic id, blocked reflects a *live* blocking dependency only, and the
// default projection is untouched.
func TestListRelationColumns(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Epic", Topic: "rel", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic): %v", err)
	}
	child, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Child", Topic: "rel", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child): %v", err)
	}
	openBlocker, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Open blocker", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(openBlocker): %v", err)
	}
	blockedByOpen, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Blocked by open", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(blockedByOpen): %v", err)
	}
	closedBlocker, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Closed blocker", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(closedBlocker): %v", err)
	}
	blockedByClosed, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "rel", Title: "Blocked by closed", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(blockedByClosed): %v", err)
	}

	// blocks convention: SrcID=dependent, DstID=dependency.
	for _, edge := range []store.AddRelationInput{
		{SrcID: blockedByOpen.ID, DstID: openBlocker.ID, Type: "blocks", CreatedBy: "test"},
		{SrcID: blockedByClosed.ID, DstID: closedBlocker.ID, Type: "blocks", CreatedBy: "test"},
	} {
		if _, err := ap.Store.AddRelation(ctx, edge); err != nil {
			t.Fatalf("AddRelation(%s->%s): %v", edge.SrcID, edge.DstID, err)
		}
	}
	if _, err := ap.Store.Apply(ctx, closedBlocker.ID, store.Change{Action: model.Done{}, Actor: "test", Reason: "done"}); err != nil {
		t.Fatalf("Apply(close): %v", err)
	}

	// --columns id,parent,blocked surfaces the relationship facts.
	var relOut bytes.Buffer
	if err := runList(ctx, &relOut, ap, []string{"--columns", "id,parent,blocked"}); err != nil {
		t.Fatalf("runList(--columns): %v", err)
	}

	cases := []struct {
		id         string
		wantParent string
		wantBlocks string
	}{
		{child.ID, epic.ID, "-"},
		{openBlocker.ID, "-", "-"},
		{blockedByOpen.ID, "-", "blocked"},
		{blockedByClosed.ID, "-", "-"}, // blocker is closed → not live → not blocked
	}
	for _, tc := range cases {
		got := fieldsOf(lineForID(t, relOut.String(), tc.id))
		if len(got) != 3 {
			t.Fatalf("id=%s: want 3 columns, got %d: %v", tc.id, len(got), got)
		}
		if got[1] != tc.wantParent {
			t.Errorf("id=%s parent: got %q, want %q", tc.id, got[1], tc.wantParent)
		}
		if got[2] != tc.wantBlocks {
			t.Errorf("id=%s blocked: got %q, want %q", tc.id, got[2], tc.wantBlocks)
		}
	}

	// Default projection is unchanged: id | state | topic | title, no parent/blocked.
	var defOut bytes.Buffer
	if err := runList(ctx, &defOut, ap, nil); err != nil {
		t.Fatalf("runList(default): %v", err)
	}
	childLine := fieldsOf(lineForID(t, defOut.String(), child.ID))
	want := []string{child.ID, "open", "rel", "Child"}
	if strings.Join(childLine, "|") != strings.Join(want, "|") {
		t.Errorf("default projection: got %v, want %v", childLine, want)
	}
	if strings.Contains(defOut.String(), epic.ID+" | -") || strings.Contains(defOut.String(), "blocked") {
		t.Errorf("default projection leaked relationship columns:\n%s", defOut.String())
	}
}

// TestListRelationColumnsGating proves the relation-graph load is paid only when
// a relationship column is projected — the byte-for-byte/no-extra-query
// guarantee for the default and non-relationship projections.
func TestListRelationColumnsGating(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "gate", Title: "X", Topic: "gate", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	issues := []model.Issue{issue}

	if rels, err := listRelationColumns(ctx, ap.Store, nil, issues); err != nil || rels != nil {
		t.Fatalf("default columns: want nil map, got %v (err %v)", rels, err)
	}
	if rels, err := listRelationColumns(ctx, ap.Store, []string{"id", "title"}, issues); err != nil || rels != nil {
		t.Fatalf("non-relationship columns: want nil map, got %v (err %v)", rels, err)
	}
	rels, err := listRelationColumns(ctx, ap.Store, []string{"id", "parent"}, issues)
	if err != nil {
		t.Fatalf("relationship columns: %v", err)
	}
	if _, ok := rels[issue.ID]; !ok {
		t.Fatalf("relationship columns: want populated map for %s, got %v", issue.ID, rels)
	}
}
