package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// The relation/label/bulk mutating verbs once wrote the raw --by flag (default
// $USER) into CreatedBy, while lifecycle verbs resolved it through the session
// rule — so under CLAUDE_CODE_SESSION_ID provenance split by which verb ran.
// These tests pin every such verb to the resolved acting identity, observing
// the recorded actor through the store rather than the implementation, so a
// regression that reintroduces the raw-$USER path fails here. [LAW:behavior-not-structure]
const attributionSessionID = "attribution-sess"

func attributionWantActor() string { return "claude_" + attributionSessionID }

func newAttributionIssue(t *testing.T, ap *app.App, title string) string {
	t.Helper()
	issue, err := ap.Store.CreateIssue(context.Background(), store.CreateIssueInput{
		Prefix: "test", Title: title, Topic: "attribution", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue(%q) error = %v", title, err)
	}
	return issue.ID
}

func TestRelationLabelBulkVerbsResolveActingIdentity(t *testing.T) {
	t.Setenv("USER", "shell-user")
	t.Setenv("CLAUDE_CODE_SESSION_ID", attributionSessionID)
	ctx := context.Background()
	want := attributionWantActor()

	t.Run("label add", func(t *testing.T) {
		ap := newTestCLIApp(t)
		id := newAttributionIssue(t, ap, "label target")
		var out bytes.Buffer
		if err := runLabelAdd(ctx, &out, ap, []string{id, "urgent"}); err != nil {
			t.Fatalf("runLabelAdd error = %v", err)
		}
		assertLabelActor(t, ap, id, "urgent", want)
	})

	t.Run("parent set", func(t *testing.T) {
		ap := newTestCLIApp(t)
		child := newAttributionIssue(t, ap, "child")
		parent := newAttributionIssue(t, ap, "parent")
		var out bytes.Buffer
		if err := runParentSet(ctx, &out, ap, []string{child, parent}); err != nil {
			t.Fatalf("runParentSet error = %v", err)
		}
		assertRelationActor(t, ap, child, model.RelParentChild, want)
	})

	t.Run("dep add", func(t *testing.T) {
		ap := newTestCLIApp(t)
		blocked := newAttributionIssue(t, ap, "blocked")
		blocker := newAttributionIssue(t, ap, "blocker")
		var out bytes.Buffer
		if err := runDepAdd(ctx, &out, ap, []string{blocker, blocked}); err != nil {
			t.Fatalf("runDepAdd error = %v", err)
		}
		assertRelationActor(t, ap, blocked, model.RelBlocks, want)
	})

	t.Run("bulk label add", func(t *testing.T) {
		ap := newTestCLIApp(t)
		id := newAttributionIssue(t, ap, "bulk label target")
		var out bytes.Buffer
		if err := runBulkLabel(ctx, &out, ap, []string{"add", "--ids", id, "--label", "urgent"}); err != nil {
			t.Fatalf("runBulkLabel error = %v", err)
		}
		assertLabelActor(t, ap, id, "urgent", want)
	})

	t.Run("bulk close", func(t *testing.T) {
		ap := newTestCLIApp(t)
		id := newAttributionIssue(t, ap, "bulk close target")
		var out bytes.Buffer
		if err := runBulkTransition(model.ActionClose)(ctx, &out, ap, []string{"--ids", id, "--reason", "done"}); err != nil {
			t.Fatalf("runBulkTransition(close) error = %v", err)
		}
		assertCloseEventActor(t, ap, id, want)
	})
}

func assertLabelActor(t *testing.T, ap *app.App, issueID, label, want string) {
	t.Helper()
	export, err := ap.Store.Export(context.Background())
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	for _, l := range export.Labels {
		if l.IssueID == issueID && l.Name == label {
			if l.CreatedBy != want {
				t.Fatalf("label %q CreatedBy = %q, want %q (raw $USER must not leak)", label, l.CreatedBy, want)
			}
			return
		}
	}
	t.Fatalf("label %q on %q not found in export", label, issueID)
}

func assertRelationActor(t *testing.T, ap *app.App, issueID string, relType model.RelationType, want string) {
	t.Helper()
	rels, err := ap.Store.ListRelationsForIssue(context.Background(), issueID, relType)
	if err != nil {
		t.Fatalf("ListRelationsForIssue error = %v", err)
	}
	if len(rels) == 0 {
		t.Fatalf("no %s relation recorded for %q", relType, issueID)
	}
	for _, rel := range rels {
		if rel.CreatedBy != want {
			t.Fatalf("%s relation CreatedBy = %q, want %q (raw $USER must not leak)", relType, rel.CreatedBy, want)
		}
	}
}

func assertCloseEventActor(t *testing.T, ap *app.App, issueID, want string) {
	t.Helper()
	detail, err := ap.Store.GetIssueDetail(context.Background(), issueID)
	if err != nil {
		t.Fatalf("GetIssueDetail error = %v", err)
	}
	for _, ev := range detail.Events {
		if ev.Action == string(model.ActionClose) {
			if ev.Actor != want {
				t.Fatalf("close event Actor = %q, want %q (raw $USER must not leak)", ev.Actor, want)
			}
			return
		}
	}
	t.Fatalf("no close event recorded for %q", issueID)
}
