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

// The JSON tree-spec path must keep working with `import` dispatching on file
// extension.
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

// An export and a tree spec are two formats, and only `lit backup restore`
// reads an export. The file here is what `lit export` really writes, so the
// test tracks the export's shape rather than a hand-made imitation of it.
func TestRunImportOfAnExportNamesBackupRestore(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	if _, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Exported", Topic: "import", IssueType: "task",
		ParentID: seedEpicRaw(t, ctx, ap, "Import", "import"),
	}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	var export bytes.Buffer
	if err := runLeaf(exportLeaf(), ctx, &export, ap, nil); err != nil {
		t.Fatalf("export error = %v", err)
	}
	path := writeImportFile(t, "export.json", export.String())

	err := runImportTree(ctx, &bytes.Buffer{}, ap, []string{"--path", path})
	if err == nil {
		t.Fatal("runImportTree(export) error = nil, want a refusal")
	}
	var stderr bytes.Buffer
	if code := WriteCommandError(&stderr, err); code != ExitValidation {
		t.Fatalf("exit code = %d, want %d (a deterministic refusal)", code, ExitValidation)
	}
	rendered := stderr.String()
	for _, want := range []string{"lit backup restore --path", "replaces this workspace's issues"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered error = %q, want it to contain %q", rendered, want)
		}
	}
	for _, misdirection := range []string{"ImportTreeSpec", "Retry the command", "lit doctor"} {
		if strings.Contains(rendered, misdirection) {
			t.Fatalf("rendered error = %q, must not contain %q", rendered, misdirection)
		}
	}
}

// A file that parses but breaks a rule is refused the way a file that does
// not parse is: exit 3 and advice to change the file. The file is what is
// wrong, so retrying repeats the refusal and lit doctor has nothing to find.
func TestRunImportRefusesARuleBreakingFileWithoutRetryAdvice(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, file, content, want string
	}{
		{"tree spec missing a title", "x.json", `[{"local_id":"a","type":"task","topic":"demo"}]`, `import: spec "a" missing title`},
		{"tree spec missing a topic", "x.json", `[{"local_id":"a","title":"x","type":"epic"}]`, `import: spec "a": topic is required`},
		{"tree spec with a topic too short to be one", "x.json", `[{"local_id":"a","title":"x","type":"epic","topic":"x"}]`, `import: spec "a": topic must be at least 3 characters`},
		{"tree spec of null", "x.json", `null`, "import: no issues in input"},
		{"tree spec with a cycle", "x.json", `[
			{"local_id":"a","title":"a","type":"epic","topic":"demo","blocked_by":["b"]},
			{"local_id":"b","title":"b","type":"epic","topic":"demo","blocked_by":["a"]}
		]`, "import: cycle detected involving"},
		{"tree spec ordering two leaves of one epic", "x.json", `[
			{"local_id":"e","title":"e","type":"epic","topic":"demo"},
			{"local_id":"a","title":"a","type":"task","topic":"demo","parent":"e"},
			{"local_id":"b","title":"b","type":"task","topic":"demo","parent":"e","blocked_by":["a"]}
		]`, `import: spec "b" blocked_by "a": ` + storage.SameEpicBlocksRejectionMessage},
		{"bulk file missing a topic", "x.yaml", "title: x\ntype: task\n", "bulk: doc 0 missing topic"},
		{"bulk update that empties a title", "x.yaml", "id: demo-1\ntitle: \" \"\n", `bulk: doc 0 (id "demo-1") sets an empty title`},
		{"bulk file with a cycle", "x.yaml", "local_id: a\ntitle: a\ntype: epic\ntopic: demo\nblocked_by: [b]\n---\nlocal_id: b\ntitle: b\ntype: epic\ntopic: demo\nblocked_by: [a]\n", "bulk: cycle detected involving"},
		{"bulk file ordering two leaves of one epic", "x.yaml", "local_id: e\ntitle: e\ntype: epic\ntopic: demo\n---\nlocal_id: a\ntitle: a\ntype: task\ntopic: demo\nparent: e\n---\ntitle: b\ntype: bug\ntopic: demo\nparent: e\nblocked_by: [a]\n", `bulk: doc 2 blocked_by "a": ` + storage.SameEpicBlocksRejectionMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ap := newTestCLIApp(t)
			path := writeImportFile(t, tc.file, tc.content)
			err := runImportTree(ctx, &bytes.Buffer{}, ap, []string{"--path", path})
			if err == nil {
				t.Fatal("runImportTree error = nil, want a refusal")
			}
			var stderr bytes.Buffer
			if code := WriteCommandError(&stderr, err); code != ExitValidation {
				t.Fatalf("exit code = %d, want %d (a deterministic refusal); error = %v", code, ExitValidation, err)
			}
			rendered := stderr.String()
			for _, want := range []string{"reason=validation_refused", tc.want} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("rendered error = %q, want it to contain %q", rendered, want)
				}
			}
			for _, misdirection := range []string{"Retry the command", "lit doctor"} {
				if strings.Contains(rendered, misdirection) {
					t.Fatalf("rendered error = %q, must not contain %q", rendered, misdirection)
				}
			}
		})
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
// not the file order and never Go's randomized map order.
// With five entries a map-ordered print would match by luck ~1/120 runs, so
// a regression fails loudly rather than flaking. t1 is a sub-epic: between
// two leaves of one epic a blocked_by is refused, since rank orders them.
func TestRunImportTreeJSONMappingOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	content := `[
		{"local_id":"e1","title":"Epic","type":"epic","topic":"import","priority":0},
		{"local_id":"t2","title":"Second","type":"task","topic":"import","priority":0,"parent":"e1","blocked_by":["t1"]},
		{"local_id":"t1","title":"First","type":"epic","topic":"import","priority":0,"parent":"e1"},
		{"local_id":"t3","title":"Third","type":"task","topic":"import","priority":0,"parent":"e1"},
		{"local_id":"t4","title":"Fourth","type":"task","topic":"import","priority":0,"parent":"e1"}
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
blocked_by: [t1]
---
local_id: t1
title: First
type: epic
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
parent: e1
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
		ParentID: seedEpicRaw(t, ctx, ap, "Import", "import"),
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
	epic := seedEpicRaw(t, ctx, ap, "Import", "import")
	existing, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{
		Prefix: "test", Title: "Old", Topic: "import", IssueType: "task", ParentID: epic,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	// A create names its epic by real id when the epic is not in the file.
	path := writeImportFile(t, "mixed.yaml", `
title: Fresh
type: task
topic: import
parent: `+epic+`
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
// not a silent create.
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
// rejected, not silently discarded.
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
		ParentID: seedEpicRaw(t, ctx, ap, "Import", "import"),
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
		ParentID: seedEpicRaw(t, ctx, ap, "Import", "import"),
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
