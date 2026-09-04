package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
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

// mappingRefs extracts, in printed order, the ref half of every
// "  <ref> -> <id>" mapping line in an import report, requiring each line's
// exact shape. The real ids hash the creation timestamp, so they differ
// between two runs of the same file by construction; the refs and their
// order are the file's own names and must not.
func mappingRefs(t *testing.T, output string) []string {
	t.Helper()
	refs := []string{}
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "  ") || !strings.Contains(line, " -> ") {
			continue
		}
		ref, id, _ := strings.Cut(strings.TrimPrefix(line, "  "), " -> ")
		if ref == "" || id == "" {
			t.Fatalf("mapping line %q, want \"  <ref> -> <id>\"", line)
		}
		refs = append(refs, ref)
	}
	return refs
}

// Both determinism tests print a five-issue mapping and require the exact
// same ref sequence on every run: the engine's creation order (dependencies
// before dependents — t1 before t2 even though the file lists t2 first),
// not the file order and never Go's randomized map order (links-import-g329).
// With five entries a map-ordered print would match by luck ~1/120 runs, so
// a regression fails loudly rather than flaking.
func TestRunImportTreeJSONMappingOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	content := `[
		{"local_id":"e1","title":"Epic","type":"epic","topic":"import","priority":0},
		{"local_id":"t2","title":"Second","type":"task","topic":"import","priority":0,"parent":"e1","depends_on":["t1"]},
		{"local_id":"t1","title":"First","type":"task","topic":"import","priority":0,"parent":"e1"},
		{"local_id":"t3","title":"Third","type":"task","topic":"import","priority":0,"parent":"e1"},
		{"local_id":"t4","title":"Fourth","type":"task","topic":"import","priority":0}
	]`
	want := "e1,t1,t2,t3,t4"
	for run := 0; run < 2; run++ {
		ap := newTestCLIApp(t)
		path := writeImportFile(t, "tree.json", content)
		var stdout bytes.Buffer
		if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
			t.Fatalf("run %d: runImportTree(json) error = %v", run, err)
		}
		if got := strings.Join(mappingRefs(t, stdout.String()), ","); got != want {
			t.Fatalf("run %d: mapping order = %s, want %s\noutput:\n%s", run, got, want, stdout.String())
		}
	}
}

func TestRunImportYAMLMappingOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	content := `
local_id: e1
title: Epic
type: epic
topic: import
---
local_id: t2
title: Second
type: task
topic: import
parent: e1
depends_on: [t1]
---
local_id: t1
title: First
type: task
topic: import
parent: e1
---
local_id: t3
title: Third
type: task
topic: import
parent: e1
---
local_id: t4
title: Fourth
type: task
topic: import
`
	want := "e1,t1,t2,t3,t4"
	for run := 0; run < 2; run++ {
		ap := newTestCLIApp(t)
		path := writeImportFile(t, "batch.yaml", content)
		var stdout bytes.Buffer
		if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
			t.Fatalf("run %d: runImportTree(yaml) error = %v", run, err)
		}
		if got := strings.Join(mappingRefs(t, stdout.String()), ","); got != want {
			t.Fatalf("run %d: mapping order = %s, want %s\noutput:\n%s", run, got, want, stdout.String())
		}
	}
}

func TestRunImportYAMLUpdatesByID(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
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
	existing, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
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

// --by has no consumer on the JSON tree-spec dispatch branch (it always
// attributes creates to "links"); a set-but-unused --by there must be
// rejected, not silently discarded, so a caller relying on it gets the same
// loud failure this command gave before --by existed on `import` at all.
func TestRunImportRejectsByFlagOnJSONPath(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	path := writeImportFile(t, "tree.json", `[{"local_id":"e1","title":"Epic","type":"epic","topic":"import","priority":0}]`)
	var stdout bytes.Buffer
	err := runImportTree(ctx, &stdout, ap, []string{"--path", path, "--by", "alice"})
	if err == nil {
		t.Fatal("runImportTree(json --by) error = nil, want usage error")
	}
	if ExitCode(err) != ExitUsage {
		t.Fatalf("runImportTree(json --by) exit code = %d, want %d", ExitCode(err), ExitUsage)
	}
}

// --by is likewise unused when a YAML file contains only creates (a create
// has no actor field to carry it) — the same rejection the JSON path gets,
// for the same reason, on the format that does support --by in general.
func TestRunImportRejectsByFlagOnCreateOnlyYAML(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	path := writeImportFile(t, "create-only.yaml", "title: Fresh\ntype: task\ntopic: import\n")
	var stdout bytes.Buffer
	err := runImportTree(ctx, &stdout, ap, []string{"--path", path, "--by", "alice"})
	if err == nil {
		t.Fatal("runImportTree(create-only yaml --by) error = nil, want usage error")
	}
	if ExitCode(err) != ExitUsage {
		t.Fatalf("runImportTree(create-only yaml --by) exit code = %d, want %d", ExitCode(err), ExitUsage)
	}
}

// A YAML file with at least one update document does consume --by — it
// attributes that document's field-change event, same as `lit update --by`.
func TestRunImportYAMLUpdateHonorsByFlag(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Before", Topic: "import", IssueType: "task",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	path := writeImportFile(t, "update.yaml", "id: "+created.ID+"\ntitle: After\n")
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path, "--by", "alice"}); err != nil {
		t.Fatalf("runImportTree(yaml update --by) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	found := false
	for _, e := range detail.Events {
		if e.Actor == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no event attributed to --by actor \"alice\": %#v", detail.Events)
	}
}

func TestRunImportYAMLUpdateTrimsReason(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	created, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Reason trim", Topic: "import", IssueType: "task",
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	path := writeImportFile(t, "reason.yaml", `
id: `+created.ID+`
priority: 1
reason: "  padded reason  "
`)
	var stdout bytes.Buffer
	if err := runImportTree(ctx, &stdout, ap, []string{"--path", path}); err != nil {
		t.Fatalf("runImportTree(yaml reason) error = %v", err)
	}
	detail, err := ap.Store.GetIssueDetail(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetIssueDetail() error = %v", err)
	}
	found := false
	for _, e := range detail.Events {
		if e.Reason == "padded reason" {
			found = true
		}
		if strings.Contains(e.Reason, "  ") {
			t.Fatalf("event.Reason = %q, want trimmed (no padding)", e.Reason)
		}
	}
	if !found {
		t.Fatalf("no event recorded the trimmed reason: %#v", detail.Events)
	}
}
