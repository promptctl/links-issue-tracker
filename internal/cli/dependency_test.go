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
	child1, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child 1", Topic: "dep", IssueType: "task", Priority: 2, ParentID: epicB.ID})
	if err != nil {
		t.Fatalf("CreateIssue(child1) error = %v", err)
	}
	child2, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child 2", Topic: "dep", IssueType: "task", Priority: 2, ParentID: epicB.ID})
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
	issueB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Blocked B", Topic: "dep", IssueType: "task", Priority: 2})
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

func TestDepRmReportsDiagnosticIDsOnNotFound(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	issueA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "dep", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	issueB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "dep", IssueType: "task", Priority: 2})
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
