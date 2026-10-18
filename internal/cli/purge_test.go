package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// TestRunPurgeRefusesWithoutConfirmationAndPurgesWithIt pins the command's
// gate and its report. Off a terminal and without --yes nothing is removed and
// the refusal is purge_unconfirmed, so an agent is told the purge is the
// user's call. With --yes the ticket is gone, and the report names the id and
// the counts but never the title — a ticket is often purged because of what it
// says — and the invocation's event counts each kind of row removed.
func TestRunPurgeRefusesWithoutConfirmationAndPurgesWithIt(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "Epic", Topic: "purge", IssueType: model.TypeEpic})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	doomed, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "pasted a secret", Topic: "purge", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(doomed) error = %v", err)
	}
	if _, _, err := ap.Store.AddComment(ctx, storage.AddCommentInput{IssueID: doomed.ID, Body: "more secret", CreatedBy: "tester"}); err != nil {
		t.Fatalf("AddComment error = %v", err)
	}
	events, err := ap.Store.ListEvents(ctx, doomed.ID)
	if err != nil {
		t.Fatalf("ListEvents error = %v", err)
	}

	var refused bytes.Buffer
	err = runLeaf(purgeLeaf(), ctx, &refused, ap, []string{doomed.ID})
	var unconfirmed purgeUnconfirmedError
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("purge without --yes error = %v, want purgeUnconfirmedError", err)
	}
	if got := commandErrorReason(err); got != reasonPurgeUnconfirmed {
		t.Errorf("reason = %q, want %q", got, reasonPurgeUnconfirmed)
	}
	if remediation := commandErrorRemediation(reasonPurgeUnconfirmed); !strings.Contains(remediation, "only when the user directs the purge") {
		t.Errorf("remediation = %q, want it to say the purge is the user's call", remediation)
	}
	if _, err := ap.Store.GetIssue(ctx, doomed.ID); err != nil {
		t.Fatalf("an unconfirmed purge removed the issue: GetIssue error = %v", err)
	}

	var stdout bytes.Buffer
	invocation := telemetry.Begin(telemetry.Event{})
	purge := purgeLeaf()
	invoked := telemetry.With(ctx, invocation)
	positional, err := parseLeaf(invoked, purge, []string{doomed.ID, "--yes"}, &stdout)
	if err != nil {
		t.Fatalf("parse purge --yes error = %v", err)
	}
	if err := purge.work(invoked, &stdout, ap, positional); err != nil {
		t.Fatalf("purge --yes error = %v", err)
	}
	counts := invocation.Finish(telemetry.End{}).Counts
	if want := map[string]int{"purge.comments": 1, "purge.plan_reviews": 0, "purge.labels": 0, "purge.edges": 1, "purge.events": len(events)}; !maps.Equal(counts, want) {
		t.Errorf("purge event counts = %v, want %v", counts, want)
	}
	// One edge: the parent edge to the epic.
	want := fmt.Sprintf("purged %s (comments=1 labels=0 edges=1 events=%d plan_reviews=0)\n", doomed.ID, len(events))
	if stdout.String() != want {
		t.Errorf("output = %q, want %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), doomed.Title) {
		t.Errorf("output %q echoes the purged ticket's title", stdout.String())
	}
	_, err = ap.Store.GetIssue(ctx, doomed.ID)
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("GetIssue after purge error = %v, want NotFoundError", err)
	}
}

// TestRunPurgeOfAMissingIssueIsNotFoundBeforeAnyConfirmation pins the order:
// an id that does not exist is reported as missing, not as an unconfirmed
// purge that --yes would then fail on anyway.
func TestRunPurgeOfAMissingIssueIsNotFoundBeforeAnyConfirmation(t *testing.T) {
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	err := runLeaf(purgeLeaf(), context.Background(), &stdout, ap, []string{"test-nope"})
	var notFound storage.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("purge of a missing id error = %v, want NotFoundError", err)
	}
}
