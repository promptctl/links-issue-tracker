package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// dbState is everything about the store a write could move: the commit at HEAD,
// and whether the working set differs from it. Reading both is what separates
// "derivation committed nothing" from "derivation wrote rows and simply has not
// committed them yet" — a distinction a commit-hash check alone would miss.
type dbState struct {
	head  string
	dirty int
}

func readDBState(t *testing.T, ctx context.Context, st *Store) dbState {
	t.Helper()
	var state dbState
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&state.head); err != nil {
		t.Fatalf("read dolt head: %v", err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_status`).Scan(&state.dirty); err != nil {
		t.Fatalf("read dolt status: %v", err)
	}
	return state
}

// deriveClaims walks the whole read path a claims consumer walks: list every
// issue including the ones that have left the flow, resolve their parents, read
// the history, and derive. It is written the way callers must write it — the
// issue list is deliberately unfiltered, because a lane's establishing event can
// sit on a closed ticket.
func deriveClaims(t *testing.T, ctx context.Context, st *Store) claims.Standings {
	t.Helper()
	issues, err := st.ListIssues(ctx, ListIssuesFilter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListIssues() error = %v", err)
	}
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	relations, err := st.GetRelationsByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("GetRelationsByIDs() error = %v", err)
	}
	parents := make(map[string]*model.Issue, len(relations))
	for id, rel := range relations {
		parents[id] = rel.Parent
	}
	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents() error = %v", err)
	}
	evidence, err := claims.NewEvidence(issues, parents, events)
	if err != nil {
		t.Fatalf("NewEvidence() error = %v", err)
	}
	return claims.Derive(evidence,
		claims.Freshness{Now: time.Now(), Window: 24 * time.Hour},
		claims.NewLocalCheckouts("test-workspace-id", []string{"abc23456defgh"}),
	)
}

// TestDerivingClaimsWritesNothing is the design's central structural promise
// held to the database itself: a claim is a reading, so asking who holds what
// must leave the store byte-identical. Nothing is stored, so nothing is
// released, transferred, or cleaned up — and that only stays true if deriving
// never quietly materializes a cache, a marker, or a "last computed" row.
func TestDerivingClaimsWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	st.AttributeTo("abc23456defgh")
	exerciseEveryEventKind(t, ctx, st)

	before := readDBState(t, ctx, st)
	if before.dirty != 0 {
		t.Fatalf("fixture left %d uncommitted tables — the assertion below could not tell a derivation write from this one", before.dirty)
	}

	standings := deriveClaims(t, ctx, st)
	if len(standings) == 0 {
		t.Fatal("derived no lanes at all — the fixture proves nothing about writes")
	}

	if after := readDBState(t, ctx, st); after != before {
		t.Errorf("deriving claims moved the database: head %s → %s, %d tables dirty", before.head, after.head, after.dirty)
	}
}

// TestDerivedClaimSurvivesARoundTripThroughTheDatabase closes the loop the pure
// tests cannot: attribution written by the real write path, read back by the
// real read path, and derived into the claim it should be. It is the one test
// that would catch the columns being written, read, or joined wrongly while
// every in-memory case stayed green.
func TestDerivedClaimSurvivesARoundTripThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	const token = "abc23456defgh"
	st.AttributeTo(token)

	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix: "test", Title: "Claimable work", Topic: "claims", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if _, err := st.Apply(ctx, issue.ID, Change{
		Action: model.Start{Assignee: "tester"}, Actor: "tester", Reason: "begin",
	}); err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}

	// The ticket has no epic, so it is a lane of one keyed by its own id.
	lane := model.LaneOf(issue, nil)
	standing := deriveClaims(t, ctx, st).Of(lane)
	held, ok := standing.(claims.Held)
	if !ok {
		t.Fatalf("standing of the started ticket's lane = %#v, want Held", standing)
	}
	if want := model.NewAttribution(token, "test-workspace-id"); held.By != want {
		t.Fatalf("holder = %+v, want %+v", held.By, want)
	}
	if len(held.Contested) != 0 {
		t.Fatalf("contested = %v, want none: one checkout produced every event", held.Contested)
	}
}
