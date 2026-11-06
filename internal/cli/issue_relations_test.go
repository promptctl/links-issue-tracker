package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestParentSetRejectsBarePositionalArgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ap := newTestCLIApp(t)

	// The child starts inside another epic; `parent set` moves it.
	child, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Child"), Lane: storage.SharedLane, Topic: "parent", IssueType: "task", Priority: 0, ParentID: seedEpicRaw(t, ctx, ap, "Former home", "parent")})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	parent, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Parent"), Lane: storage.SharedLane, Topic: "parent", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	err = runAppFamily(t, parentFamily, ctx, &stdout, ap, []string{"set", child.ID.Raw(), parent.ID.Raw()})
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("parent set bare positional error = %v (%T), want UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "--child") || !strings.Contains(err.Error(), "--parent") {
		t.Fatalf("parent set bare positional error = %q, want it to name --child/--parent", err.Error())
	}
}

func TestParentSetWithNamedFlags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ap := newTestCLIApp(t)

	// The child starts inside another epic; `parent set` moves it.
	child, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Child"), Lane: storage.SharedLane, Topic: "parent", IssueType: "task", Priority: 0, ParentID: seedEpicRaw(t, ctx, ap, "Former home", "parent")})
	if err != nil {
		t.Fatalf("CreateIssue(child) error = %v", err)
	}
	parent, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Parent"), Lane: storage.SharedLane, Topic: "parent", IssueType: "epic", Priority: 1})
	if err != nil {
		t.Fatalf("CreateIssue(parent) error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runAppFamily(t, parentFamily, ctx, &stdout, ap, []string{"set", "--child", child.ID.Raw(), "--parent", parent.ID.Raw()}); err != nil {
		t.Fatalf("parent set --child/--parent error = %v", err)
	}
	// `parent set` renders the parent-child edge through the same canonical
	// projection `dep` uses, so the same edge reads the same way whichever
	// command wrote it: child child of parent.
	if !strings.Contains(stdout.String(), child.ID.Raw()+" child of "+parent.ID.Raw()) {
		t.Fatalf("parent set output = %q, want child-of arrow", stdout.String())
	}

	detail, err := ap.Store.GetIssueDetail(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail(child) error = %v", err)
	}
	if detail.Parent == nil || detail.Parent.ID != parent.ID {
		t.Fatalf("child parent = %+v, want %s", detail.Parent, parent.ID)
	}
}
