package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestDepAddRmRoundTripWithNamedFlags(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epicA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Blocker epic A", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicA) error = %v", err)
	}
	epicB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Blocked epic B", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicB) error = %v", err)
	}
	child1, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child 1", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child1) error = %v", err)
	}
	child2, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child 2", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child2) error = %v", err)
	}

	// Add per-child blocks using named flags (--blocker/--blocked).
	for _, childID := range []string{child1.ID, child2.ID} {
		var stdout bytes.Buffer
		if err := runDep(ctx, &stdout, ap, []string{"add", "--type", "blocks", "--blocker", epicA.ID, "--blocked", childID}); err != nil {
			t.Fatalf("dep add --blocker %s --blocked %s error = %v", epicA.ID, childID, err)
		}
		if !strings.Contains(stdout.String(), "--blocks-->") {
			t.Fatalf("dep add output = %q, want blocks arrow", stdout.String())
		}
	}

	// Add epic-level block.
	var stdout bytes.Buffer
	if err := runDep(ctx, &stdout, ap, []string{"add", "--type", "blocks", "--blocker", epicA.ID, "--blocked", epicB.ID}); err != nil {
		t.Fatalf("dep add epic-level block error = %v", err)
	}

	// Rank A above B.
	if err := ap.Store.RankAbove(ctx, epicA.ID, epicB.ID); err != nil {
		t.Fatalf("RankAbove error = %v", err)
	}

	// Remove per-child blocks using positional args.
	for _, childID := range []string{child1.ID, child2.ID} {
		var rmStdout bytes.Buffer
		if err := runDep(ctx, &rmStdout, ap, []string{"rm", "--type", "blocks", epicA.ID, childID}); err != nil {
			t.Fatalf("dep rm %s %s error = %v", epicA.ID, childID, err)
		}
		if !strings.Contains(rmStdout.String(), "ok") {
			t.Fatalf("dep rm output = %q, want ok", rmStdout.String())
		}
	}

	// Remove epic-level block.
	var rmEpicStdout bytes.Buffer
	if err := runDep(ctx, &rmEpicStdout, ap, []string{"rm", "--type", "blocks", epicA.ID, epicB.ID}); err != nil {
		t.Fatalf("dep rm epic-level block error = %v", err)
	}
}

func TestDepAddRmWithPositionalArgs(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issueA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Blocker A", Topic: "dep", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	issueB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Blocked B", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}

	// Add using positional args (from to).
	var addStdout bytes.Buffer
	if err := runDep(ctx, &addStdout, ap, []string{"add", "--type", "blocks", issueA.ID, issueB.ID}); err != nil {
		t.Fatalf("dep add positional error = %v", err)
	}
	if !strings.Contains(addStdout.String(), issueA.ID) || !strings.Contains(addStdout.String(), issueB.ID) {
		t.Fatalf("dep add output = %q, want both IDs", addStdout.String())
	}

	// Remove using same positional args.
	var rmStdout bytes.Buffer
	if err := runDep(ctx, &rmStdout, ap, []string{"rm", "--type", "blocks", issueA.ID, issueB.ID}); err != nil {
		t.Fatalf("dep rm positional error = %v", err)
	}
}

func TestDepAddRejectsSameEpicBlocks(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	siblingA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(siblingA) error = %v", err)
	}
	siblingB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(siblingB) error = %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{name: "sibling positional", args: []string{"add", "--type", "blocks", siblingA.ID, siblingB.ID}},
		{name: "sibling named flags", args: []string{"add", "--type", "blocks", "--blocker", siblingA.ID, "--blocked", siblingB.ID}},
		{name: "epic blocks its own child", args: []string{"add", "--type", "blocks", epic.ID, siblingA.ID}},
		{name: "epic blocked by its own child", args: []string{"add", "--type", "blocks", siblingA.ID, epic.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runDep(ctx, &stdout, ap, tc.args)
			if err == nil {
				t.Fatalf("dep add should reject same-epic block, got nil; stdout=%q", stdout.String())
			}
			if err.Error() != sameEpicBlocksRejectionMessage {
				t.Fatalf("error = %q, want %q", err.Error(), sameEpicBlocksRejectionMessage)
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

	epicA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Epic A", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicA) error = %v", err)
	}
	epicB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Epic B", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(epicB) error = %v", err)
	}
	childOfA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child A", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicA.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childOfA) error = %v", err)
	}
	childOfB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child B", Topic: "dep", IssueType: "task", Priority: 0, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(childOfB) error = %v", err)
	}
	floatA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Float A", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(floatA) error = %v", err)
	}
	floatB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Float B", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(floatB) error = %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{name: "child of A blocks child of B", args: []string{"add", "--type", "blocks", childOfA.ID, childOfB.ID}},
		{name: "epic A blocks child of B", args: []string{"add", "--type", "blocks", epicA.ID, childOfB.ID}},
		{name: "epic A blocks epic B", args: []string{"add", "--type", "blocks", epicA.ID, epicB.ID}},
		{name: "floating blocks floating", args: []string{"add", "--type", "blocks", floatA.ID, floatB.ID}},
		{name: "floating blocks child of B", args: []string{"add", "--type", "blocks", floatA.ID, childOfB.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runDep(ctx, &stdout, ap, tc.args); err != nil {
				t.Fatalf("dep add cross-epic case %q errored = %v", tc.name, err)
			}
		})
	}
}

func TestDepRmReportsDiagnosticIDsOnNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issueA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "dep", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	issueB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}

	var stderr bytes.Buffer
	err = runDep(ctx, &stderr, ap, []string{"rm", "--type", "blocks", issueA.ID, issueB.ID})
	if err == nil {
		t.Fatal("dep rm nonexistent relation should error")
	}
	// The error should include the store-level keys for diagnosis.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "src=") || !strings.Contains(errMsg, "dst=") || !strings.Contains(errMsg, "type=") {
		t.Fatalf("error message should include diagnostic keys, got: %q", errMsg)
	}
}
