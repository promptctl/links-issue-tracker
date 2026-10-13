package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// seedCrossEpicPair creates two tasks, each inside an epic of its own, and
// returns their ids. Separate epics because a blocked-by edge between siblings
// of one epic is refused, and these tests are about the edge, not that refusal.
func seedCrossEpicPair(t *testing.T, ctx context.Context, ap *app.App, titleA, titleB string) (string, string) {
	t.Helper()
	a := seedTaskIn(t, ctx, ap, titleA, seedEpicRaw(t, ctx, ap, titleA+" (epic)", "dep"))
	b := seedTaskIn(t, ctx, ap, titleB, seedEpicRaw(t, ctx, ap, titleB+" (epic)", "dep"))
	return a, b
}

// TestDepAddRecordsTheSentenceItReads pins the vocabulary contract: `lit dep
// add A --blocked-by B` records that A waits for B — B lands under A's
// blocked_by and A under B's blocks — and prints the edge as that sentence.
// `dep rm` with the same words removes the same edge.
func TestDepAddRecordsTheSentenceItReads(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	waiter, blocker := seedCrossEpicPair(t, ctx, ap, "Waiter", "Blocker")

	var stdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"add", waiter, "--blocked-by", blocker}); err != nil {
		t.Fatalf("dep add %s --blocked-by %s error = %v", waiter, blocker, err)
	}
	if want := waiter + " blocked by " + blocker; !strings.HasPrefix(stdout.String(), want+"\n") {
		t.Fatalf("dep add output = %q, want it to open with %q", stdout.String(), want)
	}
	waiterDetail, err := ap.Store.GetIssueDetail(ctx, waiter)
	if err != nil {
		t.Fatalf("GetIssueDetail(waiter): %v", err)
	}
	if got := issueIDs(waiterDetail.BlockedBy); len(got) != 1 || got[0] != blocker {
		t.Fatalf("waiter blocked_by = %v, want [%s]", got, blocker)
	}
	if len(waiterDetail.Blocks) != 0 {
		t.Fatalf("waiter blocks = %v, want none", issueIDs(waiterDetail.Blocks))
	}
	blockerDetail, err := ap.Store.GetIssueDetail(ctx, blocker)
	if err != nil {
		t.Fatalf("GetIssueDetail(blocker): %v", err)
	}
	if got := issueIDs(blockerDetail.Blocks); len(got) != 1 || got[0] != waiter {
		t.Fatalf("blocker blocks = %v, want [%s]", got, waiter)
	}

	var lsStdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &lsStdout, ap, []string{"ls", blocker, "--type", "blocked-by"}); err != nil {
		t.Fatalf("dep ls error = %v", err)
	}
	if want := waiter + " blocked by " + blocker + "\n"; lsStdout.String() != want {
		t.Fatalf("dep ls from the blocker's end = %q, want %q", lsStdout.String(), want)
	}

	var rmStdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &rmStdout, ap, []string{"rm", waiter, "--blocked-by", blocker}); err != nil {
		t.Fatalf("dep rm error = %v", err)
	}
	edges, err := ap.Store.ListRelationsForIssue(ctx, waiter, model.RelBlockedBy)
	if err != nil {
		t.Fatalf("ListRelationsForIssue: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("edges after dep rm = %+v, want none", edges)
	}
}

func TestDepAddRelatedTo(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issueA, issueB := seedCrossEpicPair(t, ctx, ap, "A", "B")

	var stdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"add", issueA, "--related-to", issueB}); err != nil {
		t.Fatalf("dep add --related-to error = %v", err)
	}
	if !strings.Contains(stdout.String(), " related to ") {
		t.Fatalf("dep add output = %q, want a related-to sentence", stdout.String())
	}
	edges, err := ap.Store.ListRelationsForIssue(ctx, issueA, model.RelRelatedTo)
	if err != nil {
		t.Fatalf("ListRelationsForIssue: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("related-to edges = %+v, want one", edges)
	}
}

// TestDepAddRmNameExactlyOneEdge pins that each command takes one issue and
// exactly one relation flag, and that every other shape — two bare ids, no
// relation, two relations — is refused with the usage line naming the form
// that works.
func TestDepAddRmNameExactlyOneEdge(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issueA, issueB := seedCrossEpicPair(t, ctx, ap, "A", "B")

	for _, verb := range []string{"add", "rm"} {
		for name, args := range map[string][]string{
			"two bare ids":  {verb, issueA, issueB},
			"no relation":   {verb, issueA},
			"no issue":      {verb, "--blocked-by", issueB},
			"two relations": {verb, issueA, "--blocked-by", issueB, "--related-to", issueB},
		} {
			var stdout bytes.Buffer
			err := runAppFamily(depFamily, ctx, &stdout, ap, args)
			if _, ok := err.(UsageError); !ok {
				t.Errorf("dep %s (%s) error = %v (%T), want UsageError", verb, name, err, err)
				continue
			}
			if want := "lit dep " + verb + " <id> --blocked-by <blocker-id>"; !strings.Contains(err.Error(), want) {
				t.Errorf("dep %s (%s) error = %q, want it to name %q", verb, name, err.Error(), want)
			}
		}
	}
}

func TestDepAddRejectsSameEpicBlocks(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	siblingA, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "A", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(siblingA) error = %v", err)
	}
	siblingB, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "B", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(siblingB) error = %v", err)
	}

	subEpic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Sub-epic", Topic: "dep", IssueType: "epic", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(subEpic) error = %v", err)
	}
	nestedLeaf, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Nested leaf", Topic: "dep", IssueType: "task", Priority: 0, ParentID: subEpic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(nestedLeaf) error = %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{name: "sibling named flags", args: []string{"add", siblingB.ID, "--blocked-by", siblingA.ID}},
		{name: "epic blocks its own child", args: []string{"add", siblingA.ID, "--blocked-by", epic.ID}},
		{name: "epic blocked by its own child", args: []string{"add", epic.ID, "--blocked-by", siblingA.ID}},
		{name: "nested leaf blocks its outer epic", args: []string{"add", epic.ID, "--blocked-by", nestedLeaf.ID}},
		{name: "outer epic blocks its nested leaf", args: []string{"add", nestedLeaf.ID, "--blocked-by", epic.ID}},
		{name: "sub-epic blocks its parent epic", args: []string{"add", epic.ID, "--blocked-by", subEpic.ID}},
		{name: "epic blocks its sub-epic", args: []string{"add", subEpic.ID, "--blocked-by", epic.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runAppFamily(depFamily, ctx, &stdout, ap, tc.args)
			if err == nil {
				t.Fatalf("dep add should reject same-epic block, got nil; stdout=%q", stdout.String())
			}
			if err.Error() != storage.SameEpicBlocksRejectionMessage {
				t.Fatalf("error = %q, want %q", err.Error(), storage.SameEpicBlocksRejectionMessage)
			}
			if code := ExitCode(err); code != ExitValidation {
				t.Fatalf("ExitCode(err) = %d, want %d (ExitValidation)", code, ExitValidation)
			}
		})
	}
}

func TestDepAddAllowsCrossEpicAndFloatingBlocks(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epicA, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicA) error = %v", err)
	}
	epicB, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicB) error = %v", err)
	}
	childOfA, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Child A", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childOfA) error = %v", err)
	}
	childOfB, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Child B", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childOfB) error = %v", err)
	}
	// The top level holds only epics, so the ticket in no epic is an epic.
	topEpicA, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Float A", Topic: "dep", IssueType: "epic", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(topEpicA) error = %v", err)
	}
	topEpicB, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Float B", Topic: "dep", IssueType: "epic", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(topEpicB) error = %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{name: "child of A blocks child of B", args: []string{"add", childOfB.ID, "--blocked-by", childOfA.ID}},
		{name: "epic A blocks child of B", args: []string{"add", childOfB.ID, "--blocked-by", epicA.ID}},
		{name: "epic A blocks epic B", args: []string{"add", epicB.ID, "--blocked-by", epicA.ID}},
		{name: "top-level epic blocks top-level epic", args: []string{"add", topEpicB.ID, "--blocked-by", topEpicA.ID}},
		{name: "top-level epic blocks child of B", args: []string{"add", childOfB.ID, "--blocked-by", topEpicA.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runAppFamily(depFamily, ctx, &stdout, ap, tc.args); err != nil {
				t.Fatalf("dep add cross-epic case %q errored = %v", tc.name, err)
			}
		})
	}
}

func TestDepRmReportsDiagnosticIDsOnNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issueA, issueB := seedCrossEpicPair(t, ctx, ap, "A", "B")

	var stderr bytes.Buffer
	err := runAppFamily(depFamily, ctx, &stderr, ap, []string{"rm", issueB, "--blocked-by", issueA})
	if err == nil {
		t.Fatal("dep rm nonexistent relation should error")
	}
	// The error should include the store-level keys for diagnosis.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "src=") || !strings.Contains(errMsg, "dst=") || !strings.Contains(errMsg, "type=") {
		t.Fatalf("error message should include diagnostic keys, got: %q", errMsg)
	}
}

// TestDepRejectsUnknownRelationType pins the trust-boundary contract: dep ls
// rejects a --type outside the sealed set with the documented message, so an
// unknown filter errors loudly instead of silently matching nothing — the old
// "blocks" spelling included.
// [LAW:no-silent-failure]
func TestDepRejectsUnknownRelationType(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issueA, _ := seedCrossEpicPair(t, ctx, ap, "A", "B")

	wantMsg := "relation type must be blocked-by, parent-child, or related-to"
	for _, relType := range []string{"depends-on", "blocks"} {
		var stdout bytes.Buffer
		err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"ls", "--type", relType, issueA})
		if err == nil || err.Error() != wantMsg {
			t.Errorf("dep ls --type %s error = %v, want %q", relType, err, wantMsg)
		}
	}
}

func issueIDs(issues []model.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}
