package store

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// planStore holds an epic of three leaves, one labelled perf, one closed and
// one related to another, and an epic nested in it whose leaf blocks one.
func planStore(t *testing.T, ctx context.Context) (*Store, model.ID, model.ID) {
	t.Helper()
	st := openIssueStore(t, ctx)
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic"), Lane: storage.SharedLane, Topic: "plan", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	nested, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Nested"), Lane: storage.SharedLane, Topic: "plan", IssueType: "epic", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(nested) error = %v", err)
	}
	blocker, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Blocker"), Lane: storage.SharedLane, Topic: "plan", IssueType: "task", ParentID: nested.ID})
	if err != nil {
		t.Fatalf("CreateIssue(blocker) error = %v", err)
	}
	var leaves []model.ID
	for i, labels := range [][]string{{"perf"}, nil, nil} {
		leaf, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Leaf"), Lane: storage.SharedLane, Topic: "plan", IssueType: "task", ParentID: epic.ID, Labels: labels, Placement: storage.RankBottom})
		if err != nil {
			t.Fatalf("CreateIssue(leaf %d) error = %v", i, err)
		}
		leaves = append(leaves, leaf.ID)
	}
	if _, err := st.Apply(ctx, leaves[1], storage.Change{Action: model.Done{}, Actor: "ada"}); err != nil {
		t.Fatalf("close a leaf error = %v", err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: leaves[2], DstID: blocker.ID, Type: "blocked-by", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddRelation(blocked-by) error = %v", err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: leaves[0], DstID: leaves[2], Type: "related-to", CreatedBy: "ada"}); err != nil {
		t.Fatalf("AddRelation(related-to) error = %v", err)
	}
	return st, epic.ID, leaves[0]
}

// selectionPlan is the plan of the statement selecting text's issues.
func selectionPlan(t *testing.T, ctx context.Context, st *Store, text string) string {
	t.Helper()
	where, _, err := st.where(ctx, selecting(t, text).Selection)
	if err != nil {
		t.Fatalf("compile %q error = %v", text, err)
	}
	rows, err := st.db.QueryContext(ctx, "EXPLAIN PLAN SELECT i.id FROM issues i WHERE "+where.sql, where.args...)
	if err != nil {
		t.Fatalf("EXPLAIN %q error = %v", text, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan of %q error = %v", text, err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows of %q error = %v", text, err)
	}
	return plan.String()
}

// perRow finds what in a plan runs again for every row it is asked of: a
// subquery the engine does not cache, one printed inside an expression where
// the plan cannot say, and a nested-loop semi- or anti-join, which runs its
// right side again for every row on its left.
var perRow = regexp.MustCompile(`cacheable: false|Subquery\(|[─ ](SemiJoin|AntiJoin(IncludingNulls)?)\n`)

// Every subquery of a selection runs once, whatever the selection lists: one
// that read the listed row, or a semi-join over a grouped one, would run again
// for every row, and every atom form is here, negated and nested.
func TestEverySubqueryOfASelectionRunsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, epic, leaf := planStore(t, ctx)
	for _, text := range []string{
		"status=open,in_progress",
		"phase=plan",
		"labels=perf",
		"!labels",
		"labels!=perf",
		"parent=" + string(epic),
		"parent=" + string(epic) + "," + string(leaf),
		"!parent",
		"parent!=" + string(epic),
		"parent/status=in_progress type=bug",
		"siblings(labels=perf)",
		"related=" + string(leaf),
		"blocked_by(status!=closed)",
		"children/count>=2",
		"children/count!=3",
		"children(status=closed)/count=0",
		"children(children/count>0)/count>=1",
		"related/count>0",
		"labels/count=1",
		"/issues(type=bug)",
		"!/issues(type=bug)",
		"/issues(status=closed)/count=0",
	} {
		if plan := selectionPlan(t, ctx, st, text); perRow.MatchString(plan) {
			t.Errorf("%q plans a step that runs again for every row (%q):\n%s", text, perRow.FindString(plan), plan)
		}
	}
}

// A selection that names an issue reads it through the issues index, so a
// listing of one issue does not scan the store.
func TestASelectionNamingAnIssueReadsItByIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, leaf := planStore(t, ctx)
	plan := selectionPlan(t, ctx, st, "id="+string(leaf))
	listed := strings.Index(plan, "TableAlias(i)\n")
	if listed < 0 || !strings.Contains(strings.SplitN(plan[listed:], "\n", 3)[1], "IndexedTableAccess(issues)") {
		t.Errorf("id=%s does not read the issue through the issues index:\n%s", leaf, plan)
	}
}

// listingStatements counts the statements ListIssues issues for text over a
// store of epics epics, each holding one open leaf.
func listingStatements(t *testing.T, ctx context.Context, epics int, text string) int64 {
	t.Helper()
	st := openIssueStore(t, ctx)
	for range epics {
		epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Epic"), Lane: storage.SharedLane, Topic: "qcount", IssueType: "epic"})
		if err != nil {
			t.Fatalf("CreateIssue(epic) error = %v", err)
		}
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Leaf"), Lane: storage.SharedLane, Topic: "qcount", IssueType: "task", ParentID: epic.ID, Placement: storage.RankBottom}); err != nil {
			t.Fatalf("CreateIssue(leaf) error = %v", err)
		}
	}
	listing := selecting(t, text)
	counter := swapInCountingDB(t, st)
	atomic.StoreInt64(counter, 0)
	issues, err := st.ListIssues(ctx, listing)
	if err != nil {
		t.Fatalf("ListIssues(%q) error = %v", text, err)
	}
	if len(issues) != 2*epics {
		t.Fatalf("ListIssues(%q) listed %d issues, want every epic and leaf, %d", text, len(issues), 2*epics)
	}
	return atomic.LoadInt64(counter)
}

// A listing costs the same statements for forty epics as for one, with a
// selector that reads a state, a path, a negation and a count: each step is
// one statement over every row, never one a row, a parent or an epic.
func TestAListingsStatementsDoNotGrowWithWhatItLists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const text = "status=open !blocked_by labels!=wip children/count>=0"
	if one, forty := listingStatements(t, ctx, 1, text), listingStatements(t, ctx, 40, text); one != forty {
		t.Fatalf("ListIssues(%q) issues %d statements for one epic and %d for forty", text, one, forty)
	}
}

// A listing that reads a status walks the hierarchy once: its hydration reads
// the progress the selection read, so it costs no statement more than a
// listing that reads no state and walks only to hydrate.
func TestAListingThatReadsAStatusWalksOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if plain, status := listingStatements(t, ctx, 3, ""), listingStatements(t, ctx, 3, "status=open"); status != plain {
		t.Fatalf("ListIssues issues %d statements reading a status and %d reading none", status, plain)
	}
}
