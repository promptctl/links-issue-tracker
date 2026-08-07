package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

func writeImportFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", name, err)
	}
	return path
}

// The JSON tree-spec path must keep working unchanged now that `import`
// dispatches on file extension instead of being JSON-only.
func TestRunImportTreeJSONPathUnchanged(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	path := writeImportFile(t, "tree.json", `[
		{"local_id":"e1","title":"Epic","type":"epic","topic":"import","priority":0},
		{"local_id":"t1","title":"Child","type":"task","topic":"import","priority":0,"parent":"e1"}
	]`)
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
		t.Fatalf("runImportTree(json) error = %v", err)
	}
	if !strings.Contains(stdout.String(), "imported 2 issues") {
		t.Fatalf("output = %q, want the imported-count line", stdout.String())
	}
}

func TestRunImportYAMLCreatesEpicAndChild(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	path := writeImportFile(t, "batch.yaml", `
local_id: e1
title: Epic
type: epic
topic: import
---
local_id: t1
title: Child
type: task
topic: import
parent: e1
`)
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
		t.Fatalf("runImportTree(yaml create) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "created 2 issues") || !strings.Contains(out, "updated 0 issues") {
		t.Fatalf("output = %q, want created 2 / updated 0", out)
	}
}

func TestRunImportYAMLUpdatesByID(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Before", Topic: "import", IssueType: "task",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	path := writeImportFile(t, "update.yml", `
id: `+created.ID+`
title: After
labels: [reviewed]
`)
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
		t.Fatalf("runImportTree(yaml update) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "created 0 issues") || !strings.Contains(out, "updated 1 issues") {
		t.Fatalf("output = %q, want created 0 / updated 1", out)
	}
	updated, err := ap.Store.GetIssue(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if updated.Title != "After" {
		t.Fatalf("updated.Title = %q, want After", updated.Title)
	}
	if len(updated.Labels) != 1 || updated.Labels[0] != "reviewed" {
		t.Fatalf("updated.Labels = %#v, want [reviewed]", updated.Labels)
	}
}

func TestRunImportYAMLMixedFileIsLegal(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	existing, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Prefix: "test", Title: "Old", Topic: "import", IssueType: "task",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	path := writeImportFile(t, "mixed.yaml", `
title: Fresh
type: task
topic: import
---
id: `+existing.ID+`
priority: 1
`)
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
		t.Fatalf("runImportTree(yaml mixed) error = %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "created 1 issues") || !strings.Contains(out, "updated 1 issues") {
		t.Fatalf("output = %q, want created 1 / updated 1", out)
	}
}

// An id that selects no existing ticket is an error surfaced to the caller,
// not a silent create — the ticket's explicit requirement.
func TestRunImportYAMLUnknownIDIsError(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	path := writeImportFile(t, "ghost.yaml", `
id: does-not-exist-1
title: New title
`)
	var stdout bytes.Buffer
	err := runImportTree(ctx, &stdout, ap, []string{"--path", path})
	if err == nil {
		t.Fatal("runImportTree(yaml unknown id) error = nil, want not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("runImportTree(yaml unknown id) error = %v, want not-found error", err)
	}
}

func TestRunImportRequiresPath(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var stdout bytes.Buffer
	err := runImportTree(ctx, &stdout, ap, nil)
	if err == nil {
		t.Fatal("runImportTree(no --path) error = nil, want usage error")
	}
	if ExitCode(err) != ExitUsage {
		t.Fatalf("runImportTree(no --path) exit code = %d, want %d", ExitCode(err), ExitUsage)
	}
}
