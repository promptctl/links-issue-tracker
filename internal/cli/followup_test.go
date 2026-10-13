package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestRunFollowupFilesBesideTheClosedTicket pins where a follow-up lands: under
// the closed ticket's own parent, as its sibling, with the link to the ticket
// it follows up carried on the row rather than by the hierarchy.
func TestRunFollowupFilesBesideTheClosedTicket(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Renderer", Topic: "renderer", IssueType: model.TypeEpic,
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	closed, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title:     "Renderer cache invalidation",
		Topic:     "renderer",
		IssueType: "task",
		Priority:  0,
		ParentID:  epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(closed) error = %v", err)
	}
	if _, err := ap.Store.Apply(ctx, closed.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("StartIssue error = %v", err)
	}
	if _, err := ap.Store.Apply(ctx, closed.ID, storage.Change{Action: model.Done{}, Actor: "tester"}); err != nil {
		t.Fatalf("Apply(done) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{
		"--on", closed.ID,
		"--title", "Surface stale cache rows in doctor",
	}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}

	createdID := firstIssueID(t, stdout.String())
	// The id hangs under the epic, not under the closed ticket; the segment
	// beneath it is a content hash, not a position, so nothing asserts its
	// value. [LAW:behavior-not-structure]
	if !strings.HasPrefix(createdID, epic.ID+".") {
		t.Fatalf("created.ID = %q, want it to hang under the epic %q", createdID, epic.ID)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, createdID)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s) error = %v", createdID, err)
	}
	created := detail.Issue
	if detail.Parent == nil || detail.Parent.ID != epic.ID {
		t.Fatalf("created parent = %v, want the epic %s", detail.Parent, epic.ID)
	}
	if created.FollowsUp != closed.ID {
		t.Fatalf("created.FollowsUp = %q, want %q", created.FollowsUp, closed.ID)
	}
	if created.Topic != closed.Topic {
		t.Fatalf("created.Topic = %q, want inherited %q", created.Topic, closed.Topic)
	}
	if !strings.Contains(created.Description, closed.ID) {
		t.Fatalf("created.Description = %q, want reference to %q", created.Description, closed.ID)
	}
	if created.IssueType != "task" {
		t.Fatalf("created.IssueType = %q, want task", created.IssueType)
	}
	// The closed ticket did not become a container.
	closedDetail, err := ap.Store.GetIssueDetail(ctx, closed.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s) error = %v", closed.ID, err)
	}
	if len(closedDetail.Children) != 0 {
		t.Fatalf("closed ticket has children %v, want none", closedDetail.Children)
	}
	if len(closedDetail.FollowUps) != 1 || closedDetail.FollowUps[0].ID != createdID {
		t.Fatalf("closed ticket follow-ups = %v, want [%s]", closedDetail.FollowUps, createdID)
	}
}

// TestRunFollowupOnATopLevelTicketIsTopLevel pins the parentless case. The
// only ticket outside any epic is a top-level epic, and a follow-up lands
// beside the ticket it follows up: a task beside a top-level epic would be a
// task outside every epic, so the store refuses it and names the two ways in;
// a follow-up that is itself an epic lands at the top level, and a task filed
// with --parent lands in the epic it names, each with the link carried on its
// row.
func TestRunFollowupOnATopLevelTicketIsTopLevel(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	closed, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Loose end", Topic: "sync", IssueType: model.TypeEpic, Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue error = %v", err)
	}

	var refused bytes.Buffer
	err = runFollowup(ctx, &refused, ap, []string{"--on", closed.ID, "--title", "Tie it off"})
	var validation model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("runFollowup(task beside a top-level epic) error = %v (%T), want model.ValidationError", err, err)
	}
	if !strings.Contains(err.Error(), "as its parent, or make it an epic") {
		t.Fatalf("runFollowup(task beside a top-level epic) error = %q, want it to name a parent epic and the epic type as the ways in", err.Error())
	}

	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{"--on", closed.ID, "--title", "Tie it off", "--type", "epic"}); err != nil {
		t.Fatalf("runFollowup(--type epic) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	if detail.Parent != nil {
		t.Fatalf("created parent = %s, want none", detail.Parent.ID)
	}
	if detail.Issue.FollowsUp != closed.ID {
		t.Fatalf("created.FollowsUp = %q, want %q", detail.Issue.FollowsUp, closed.ID)
	}

	home, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Where the work goes", Topic: "sync", IssueType: model.TypeEpic,
	})
	if err != nil {
		t.Fatalf("CreateIssue(home epic) error = %v", err)
	}
	var filed bytes.Buffer
	if err := runFollowup(ctx, &filed, ap, []string{"--on", closed.ID, "--title", "Tie it off", "--parent", home.ID}); err != nil {
		t.Fatalf("runFollowup(--parent) error = %v", err)
	}
	task, err := ap.Store.GetIssueDetail(ctx, firstIssueID(t, filed.String()))
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	if task.Parent == nil || task.Parent.ID != home.ID {
		t.Fatalf("created parent = %v, want %s, the epic --parent named", task.Parent, home.ID)
	}
	if task.Issue.IssueType != model.TypeTask || task.Issue.FollowsUp != closed.ID {
		t.Fatalf("created type = %s, FollowsUp = %q; want a task that follows up %s", task.Issue.IssueType, task.Issue.FollowsUp, closed.ID)
	}
}

// TestShowRendersTheFollowUpLinkOnBothEnds pins the rendering: the follow-up
// shows the ticket it follows up under `follows_up:`, and that ticket shows it
// under `follow_ups:`; neither renders as parent or child.
func TestShowRendersTheFollowUpLinkOnBothEnds(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	closed := seedOpenIssueRaw(t, ctx, ap, "Origin")

	var sink bytes.Buffer
	if err := runFollowup(ctx, &sink, ap, []string{"--on", closed, "--title", "Follow-up"}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}
	followUp := firstIssueID(t, sink.String())

	var out bytes.Buffer
	if err := runShow(ctx, &out, ap, []string{followUp}); err != nil {
		t.Fatalf("runShow(%s) error = %v", followUp, err)
	}
	if text := out.String(); !strings.Contains(text, "follows_up:\n- "+closed) || strings.Contains(text, "parent:\n- "+closed) {
		t.Fatalf("show of the follow-up wants a follows_up group naming %s and no parent group naming it; got:\n%s", closed, text)
	}
	out.Reset()
	if err := runShow(ctx, &out, ap, []string{closed}); err != nil {
		t.Fatalf("runShow(%s) error = %v", closed, err)
	}
	if text := out.String(); !strings.Contains(text, "follow_ups:\n- "+followUp) || strings.Contains(text, "children:") {
		t.Fatalf("show of the origin wants a follow_ups group naming %s and no children; got:\n%s", followUp, text)
	}
}

func TestRunFollowupRespectsExplicitOverrides(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	epic := seedEpicRaw(t, ctx, ap, "Sync", "sync")
	parent, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title:     "Sync flow tightening",
		Topic:     "sync",
		IssueType: "task",
		Priority:  0,
		ParentID:  epic,
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{
		"--on", parent.ID,
		"--title", "Add sync metrics surface",
		"--description", "Custom description for the follow-up.",
		"--topic", "observability",
		"--type", "feature",
		"--priority", "1",
		"--labels", "metrics,observability",
	}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}

	created, err := ap.Store.GetIssue(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if created.Topic != "observability" {
		t.Fatalf("created.Topic = %q, want observability", created.Topic)
	}
	if created.Description != "Custom description for the follow-up." {
		t.Fatalf("created.Description = %q, want explicit override", created.Description)
	}
	if created.IssueType != "feature" {
		t.Fatalf("created.IssueType = %q, want feature", created.IssueType)
	}
	if created.Priority != 1 {
		t.Fatalf("created.Priority = %d, want 1", created.Priority)
	}
}

func TestRunFollowupRequiresOnAndTitle(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	cases := []struct {
		name string
		args []string
	}{
		{"missing --on", []string{"--title", "x"}},
		{"missing --title", []string{"--on", "test-aaaaaaaa-bbbbbbbb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runFollowup(ctx, &stdout, ap, c.args); err == nil {
				t.Fatalf("runFollowup() expected usage error, got nil")
			}
		})
	}
}

func TestRunFollowupUnknownParentReturnsError(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var stdout bytes.Buffer
	err := runFollowup(ctx, &stdout, ap, []string{
		"--on", "test-deadbeef-cafef00d",
		"--title", "Phantom follow-up",
	})
	if err == nil {
		t.Fatalf("runFollowup() expected error for unknown parent, got nil")
	}
}

func TestRunFollowupWithoutAssigneeCreatesUnassigned(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-closer")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	parent, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test",
		Title: "Closed work", Topic: "lifecycle", IssueType: "task", Priority: 0,
		ParentID: seedEpicRaw(t, ctx, ap, "Lifecycle", "lifecycle"),
	})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}
	var stdout bytes.Buffer
	if err := runFollowup(ctx, &stdout, ap, []string{
		"--on", parent.ID, "--title", "Surfaced follow-up",
	}); err != nil {
		t.Fatalf("runFollowup() error = %v", err)
	}
	created, err := ap.Store.GetIssue(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got := created.AssigneeValue(); got != "" {
		t.Fatalf("created.AssigneeValue() = %q, want empty: followup capture is not a claim", got)
	}
}
