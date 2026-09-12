package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Epic", Topic: "rel", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic): %v", err)
	}
	child, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Child", Topic: "rel", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child): %v", err)
	}
	openBlocker, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Open blocker", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(openBlocker): %v", err)
	}
	blockedByOpen, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Blocked by open", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(blockedByOpen): %v", err)
	}
	closedBlocker, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Closed blocker", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(closedBlocker): %v", err)
	}
	blockedByClosed, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rel", Title: "Blocked by closed", Topic: "rel", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(blockedByClosed): %v", err)
	}

	// blocks convention: SrcID=dependent, DstID=dependency.
	for _, edge := range []storage.AddRelationInput{
		{SrcID: blockedByOpen.ID, DstID: openBlocker.ID, Type: "blocks", CreatedBy: "test"},
		{SrcID: blockedByClosed.ID, DstID: closedBlocker.ID, Type: "blocks", CreatedBy: "test"},
	} {
		if _, err := ap.Store.AddRelation(ctx, edge); err != nil {
			t.Fatalf("AddRelation(%s->%s): %v", edge.SrcID, edge.DstID, err)
		}
	}
	if _, err := ap.Store.Apply(ctx, closedBlocker.ID, storage.Change{Action: model.Done{}, Actor: "test", Reason: "done"}); err != nil {
		t.Fatalf("Apply(close): %v", err)
	}

	// --columns id,parent,blocked surfaces the relationship facts.
	var relOut bytes.Buffer
	if err := runListWithStore(ctx, &relOut, ap.Store, nil, []string{"--columns", "id,parent,blocked"}); err != nil {
		t.Fatalf("runListWithStore(--columns): %v", err)
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
	if err := runListWithStore(ctx, &defOut, ap.Store, nil, nil); err != nil {
		t.Fatalf("runListWithStore(default): %v", err)
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

// TestColumnSourceLadder pins the rule that decides what a projection costs:
// the requirement is the MAXIMUM rung over the selected columns, not the first
// or the last one found.
//
// The two mixed cases are the whole test. `parent` sits at sourceRelations and
// `blocked` at sourceReadiness, so a fold that returned the first non-zero rung
// would answer sourceRelations for `id,parent,blocked`, and one that returned
// the last would answer sourceRelations for `id,blocked,parent`. Either way the
// loader would fetch the graph, `blocked` would be computed from data that
// cannot express it, and the cell would print "-" — the shape of a true answer
// for a ticket that is genuinely blocked. Both orders are asserted because a max
// is the only fold that survives both.
func TestColumnSourceLadder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns []columnSpec
		want    columnSource
	}{
		{"default projection", defaultColumns(), sourceIssue},
		{"issue fields only", mustColumns("id", "title"), sourceIssue},
		{"parent alone", mustColumns("id", "parent"), sourceRelations},
		{"blocked alone", mustColumns("id", "blocked"), sourceReadiness},
		{"parent before blocked", mustColumns("id", "parent", "blocked"), sourceReadiness},
		{"blocked before parent", mustColumns("id", "blocked", "parent"), sourceReadiness},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := columnSourceFor(tc.columns); got != tc.want {
				t.Errorf("columnSourceFor(%v) = %d, want %d", columnNames(tc.columns), got, tc.want)
			}
		})
	}
}

// TestListDerivedColumnsLoadsOnlyWhatIsProjected proves each rung is paid for
// only when a column on it is selected — the no-extra-query guarantee for the
// default and issue-only projections, and the reason `--columns blocked` is
// allowed to cost the annotation pipeline at all.
//
// The `parent` case asserts blocked is FALSE on a genuinely blocked issue, which
// reads backwards until you see what it pins: that rung loads the graph and
// nothing else, so it must not be able to answer `blocked`. If some future edit
// re-derives the cell from dependency edges to "save" the annotation pass, this
// is where the shorter list comes back, and this line is what fails.
func TestListDerivedColumnsLoadsOnlyWhatIsProjected(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	blocker, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "gate", Title: "Blocker", Topic: "gate", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(blocker): %v", err)
	}
	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "gate", Title: "X", Topic: "gate", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if _, err := ap.Store.AddRelation(ctx, storage.AddRelationInput{SrcID: issue.ID, DstID: blocker.ID, Type: "blocks", CreatedBy: "test"}); err != nil {
		t.Fatalf("AddRelation: %v", err)
	}
	issues := []model.Issue{issue, blocker}

	for _, columns := range [][]columnSpec{defaultColumns(), mustColumns("id", "title")} {
		cells, err := listDerivedColumns(ctx, ap.Store, nil, columns, issues)
		if err != nil || cells != nil {
			t.Fatalf("%v: want nil map, got %v (err %v)", columnNames(columns), cells, err)
		}
	}

	graphOnly, err := listDerivedColumns(ctx, ap.Store, nil, mustColumns("id", "parent"), issues)
	if err != nil {
		t.Fatalf("parent projection: %v", err)
	}
	if _, ok := graphOnly[issue.ID]; !ok {
		t.Fatalf("parent projection: want populated map for %s, got %v", issue.ID, graphOnly)
	}
	if graphOnly[issue.ID].blocked {
		t.Errorf("parent projection set blocked for %s from the graph alone; that cell is "+
			"ClassifyReadiness's to write and this rung never ran it", issue.ID)
	}

	classified, err := listDerivedColumns(ctx, ap.Store, nil, mustColumns("id", "blocked"), issues)
	if err != nil {
		t.Fatalf("blocked projection: %v", err)
	}
	if !classified[issue.ID].blocked {
		t.Errorf("blocked projection: %s has a live open dependency and came back unblocked: %+v",
			issue.ID, classified[issue.ID])
	}
	if classified[blocker.ID].blocked {
		t.Errorf("blocked projection: %s blocks nothing and has no dependency of its own, "+
			"so a blocked cell here means the map is populated rather than computed: %+v",
			blocker.ID, classified[blocker.ID])
	}
}
