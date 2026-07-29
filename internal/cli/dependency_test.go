package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
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

	// Add per-child blocks using named flags (--from/--to).
	for _, childID := range []string{child1.ID, child2.ID} {
		var stdout bytes.Buffer
		if err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"add", "--type", "blocks", "--from", epicA.ID, "--to", childID}); err != nil {
			t.Fatalf("dep add --from %s --to %s error = %v", epicA.ID, childID, err)
		}
		if !strings.Contains(stdout.String(), "--blocks-->") {
			t.Fatalf("dep add output = %q, want blocks arrow", stdout.String())
		}
	}

	// Add epic-level block.
	var stdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"add", "--type", "blocks", "--from", epicA.ID, "--to", epicB.ID}); err != nil {
		t.Fatalf("dep add epic-level block error = %v", err)
	}

	// Rank A above B.
	if _, err := ap.Store.RankAbove(ctx, epicA.ID, epicB.ID); err != nil {
		t.Fatalf("RankAbove error = %v", err)
	}

	// Remove per-child blocks using named flags.
	for _, childID := range []string{child1.ID, child2.ID} {
		var rmStdout bytes.Buffer
		if err := runAppFamily(depFamily, ctx, &rmStdout, ap, []string{"rm", "--type", "blocks", "--from", epicA.ID, "--to", childID}); err != nil {
			t.Fatalf("dep rm --from %s --to %s error = %v", epicA.ID, childID, err)
		}
		if !strings.Contains(rmStdout.String(), "ok") {
			t.Fatalf("dep rm output = %q, want ok", rmStdout.String())
		}
	}

	// Remove epic-level block.
	var rmEpicStdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &rmEpicStdout, ap, []string{"rm", "--type", "blocks", "--from", epicA.ID, "--to", epicB.ID}); err != nil {
		t.Fatalf("dep rm epic-level block error = %v", err)
	}
}

func TestDepAddRmRejectsBarePositionalArgs(t *testing.T) {
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

	var addStdout bytes.Buffer
	err = runAppFamily(depFamily, ctx, &addStdout, ap, []string{"add", "--type", "blocks", issueA.ID, issueB.ID})
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("dep add bare positional error = %v (%T), want UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "--from") || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("dep add bare positional error = %q, want it to name --from/--to", err.Error())
	}

	var rmStdout bytes.Buffer
	err = runAppFamily(depFamily, ctx, &rmStdout, ap, []string{"rm", "--type", "blocks", issueA.ID, issueB.ID})
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("dep rm bare positional error = %v (%T), want UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "--from") || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("dep rm bare positional error = %q, want it to name --from/--to", err.Error())
	}
}

func TestDepAddRmWithNamedFlags(t *testing.T) {
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

	// Add using named flags.
	var addStdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &addStdout, ap, []string{"add", "--type", "blocks", "--from", issueA.ID, "--to", issueB.ID}); err != nil {
		t.Fatalf("dep add --from/--to error = %v", err)
	}
	if !strings.Contains(addStdout.String(), issueA.ID) || !strings.Contains(addStdout.String(), issueB.ID) {
		t.Fatalf("dep add output = %q, want both IDs", addStdout.String())
	}

	// Remove using the same named flags.
	var rmStdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &rmStdout, ap, []string{"rm", "--type", "blocks", "--from", issueA.ID, "--to", issueB.ID}); err != nil {
		t.Fatalf("dep rm --from/--to error = %v", err)
	}
}

func TestDepAddParentChildWithNamedFlags(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	parent, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Parent", Topic: "dep", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	child, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runAppFamily(depFamily, ctx, &stdout, ap, []string{"add", "--type", "parent-child", "--from", child.ID, "--to", parent.ID}); err != nil {
		t.Fatalf("dep add parent-child error = %v", err)
	}
	if !strings.Contains(stdout.String(), "--child-of-->") {
		t.Fatalf("dep add output = %q, want child-of arrow", stdout.String())
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
		{name: "sibling named flags", args: []string{"add", "--type", "blocks", "--from", siblingA.ID, "--to", siblingB.ID}},
		{name: "epic blocks its own child", args: []string{"add", "--type", "blocks", "--from", epic.ID, "--to", siblingA.ID}},
		{name: "epic blocked by its own child", args: []string{"add", "--type", "blocks", "--from", siblingA.ID, "--to", epic.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runAppFamily(depFamily, ctx, &stdout, ap, tc.args)
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
		{name: "child of A blocks child of B", args: []string{"add", "--type", "blocks", "--from", childOfA.ID, "--to", childOfB.ID}},
		{name: "epic A blocks child of B", args: []string{"add", "--type", "blocks", "--from", epicA.ID, "--to", childOfB.ID}},
		{name: "epic A blocks epic B", args: []string{"add", "--type", "blocks", "--from", epicA.ID, "--to", epicB.ID}},
		{name: "floating blocks floating", args: []string{"add", "--type", "blocks", "--from", floatA.ID, "--to", floatB.ID}},
		{name: "floating blocks child of B", args: []string{"add", "--type", "blocks", "--from", floatA.ID, "--to", childOfB.ID}},
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

	issueA, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "A", Topic: "dep", IssueType: "task", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(A) error = %v", err)
	}
	issueB, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "B", Topic: "dep", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue(B) error = %v", err)
	}

	var stderr bytes.Buffer
	err = runAppFamily(depFamily, ctx, &stderr, ap, []string{"rm", "--type", "blocks", "--from", issueA.ID, "--to", issueB.ID})
	if err == nil {
		t.Fatal("dep rm nonexistent relation should error")
	}
	// The error should include the store-level keys for diagnosis.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "src=") || !strings.Contains(errMsg, "dst=") || !strings.Contains(errMsg, "type=") {
		t.Fatalf("error message should include diagnostic keys, got: %q", errMsg)
	}
}

// TestDepRejectsUnknownRelationType pins the trust-boundary contract: every
// dep subcommand rejects a relation type outside the sealed set at the CLI
// boundary with the documented message. For 'ls' this is load-bearing — an
// unknown --type filter must error loudly, not silently match nothing.
// [LAW:no-silent-failure]
func TestDepRejectsUnknownRelationType(t *testing.T) {
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

	wantMsg := "relation type must be blocks, parent-child, or related-to"
	cases := [][]string{
		{"add", "--type", "depends-on", "--from", issueA.ID, "--to", issueB.ID},
		{"rm", "--type", "depends-on", "--from", issueA.ID, "--to", issueB.ID},
		{"ls", "--type", "depends-on", issueA.ID},
	}
	for _, args := range cases {
		var stdout bytes.Buffer
		err := runAppFamily(depFamily, ctx, &stdout, ap, args)
		if err == nil || err.Error() != wantMsg {
			t.Errorf("dep %s error = %v, want %q", args[0], err, wantMsg)
		}
	}
}
