package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A top-level id is sized by the ids sharing its space. COUNT(*) FROM issues
// is flat too and fails here: child rows would lengthen every top-level id of
// a store with deep epics.
func TestATopLevelIDIsSizedByTopLevelRowsAlone(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	hashLength := func() int {
		t.Helper()
		epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Sized", Topic: "env", IssueType: model.TypeEpic})
		if err != nil {
			t.Fatalf("CreateIssue() error = %v", err)
		}
		return len(strings.TrimPrefix(epic.ID, "test-env-"))
	}

	var children, epics []string
	for i := range 1000 {
		children = append(children, seedTaskRow(fmt.Sprintf("test-env-aaa.%03d", i), fmt.Sprintf("K%06d", i)))
	}
	for i := range 164 {
		epics = append(epics, seedEpicRow(fmt.Sprintf("test-env-%03d", i), fmt.Sprintf("L%06d", i)))
	}

	seedInsert(t, ctx, st, seedIssuesHead, children)
	seedCommit(t, ctx, st)
	if got := hashLength(); got != issueid.MinHashLength {
		t.Errorf("hash length beside 1,000 child rows = %d, want %d", got, issueid.MinHashLength)
	}

	seedInsert(t, ctx, st, seedIssuesHead, epics)
	seedCommit(t, ctx, st)
	if got := hashLength(); got != issueid.MinHashLength+1 {
		t.Errorf("hash length past 163 top-level rows = %d, want %d", got, issueid.MinHashLength+1)
	}
}

// A scan and the index read return one count, and only under a store's size do
// they differ, so the plan is what holds this: a timing ratio cannot under load.
func TestTopLevelIDsCountThroughTheirIndex(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 60, false)
	plan := planOf(t, ctx, st, topLevelIDCount)
	if !strings.Contains(plan, "IndexedTableAccess(issues)") {
		t.Errorf("%s plans as\n%s\nwant an index read", topLevelIDCount, plan)
	}
}
