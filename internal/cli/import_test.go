package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
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

// importedID is the real id an import report maps ref to.
func importedID(t *testing.T, output, ref string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if id, ok := strings.CutPrefix(line, "  "+ref+" -> "); ok {
			return id
		}
	}
	t.Fatalf("import report maps no %q:\n%s", ref, output)
	return ""
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

// Both import formats name the importer as the author of every ticket they
// create: the created event records --by, as lit new's does, so a plan
// imported with a ticket has the importer as its author.
func TestRunImportAttributesCreatesToBy(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	for _, tc := range []struct {
		file string
		body func(epic string) string
	}{
		{"tree.json", func(string) string {
			return `[{"local_id":"e1","title":"Imported epic","type":"epic","topic":"import","priority":0},{"local_id":"t1","parent":"e1","title":"Imported","type":"task","topic":"import","priority":0}]`
		}},
		{"create.yaml", func(epic string) string { return "title: Imported\ntype: task\ntopic: import\nparent: " + epic + "\n" }},
	} {
		t.Run(tc.file, func(t *testing.T) {
			ctx := context.Background()
			ap := newTestCLIApp(t)
			path := writeImportFile(t, tc.file, tc.body(seedEpicRaw(t, ctx, ap, "Import", "import")))
			var stdout bytes.Buffer
			if err := runImportTree(ctx, &stdout, ap, []string{"--path", path, "--by", "alice"}); err != nil {
				t.Fatalf("runImportTree(%s --by alice) error = %v", tc.file, err)
			}
			for _, id := range importedIDs(t, stdout.String()) {
				events, err := ap.Store.ListEvents(ctx, id)
				if err != nil {
					t.Fatalf("ListEvents(%s) error = %v", id, err)
				}
				if events[0].Action != model.ActionCreated || events[0].Actor != "alice" {
					t.Errorf("%s's first event = %s by %q, want %s by alice", id, events[0].Action, events[0].Actor, model.ActionCreated)
				}
			}
		})
	}
}

// importedIDs is the real id of every ticket an import's output maps.
func importedIDs(t *testing.T, output string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		if _, id, ok := strings.Cut(line, " -> "); ok {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	if len(ids) == 0 {
		t.Fatalf("no created mapping in import output %q", output)
	}
	return ids
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

// One `lit import` reorders a backlog: ten epics a file names with
// `rank: top` are listed in file order, the import reports the order
// it wrote, and the invocation's event counts what the file created, updated
// and ranked.
func TestRunImportYAMLRankTopOrdersTheBacklogInOneCall(t *testing.T) {
	isolateEvents(t)
	dir := newInitTestRepo(t)
	// One import seeds the ten epics: a `lit new` per epic would cost this
	// serial test ten invocations for no behavior it pins.
	var seed strings.Builder
	for i := range 10 {
		fmt.Fprintf(&seed, "---\nlocal_id: e%d\ntitle: epic %d\ntype: epic\ntopic: demo\n", i, i)
	}
	seeded := runCLIInDir(t, dir, "import", "--path", writeImportFile(t, "seed.yaml", seed.String()))
	epics := make([]string, 10)
	for i := range epics {
		epics[i] = importedID(t, seeded, fmt.Sprintf("e%d", i))
	}
	var file strings.Builder
	var want []string
	for _, n := range []int{4, 9, 0, 6, 2, 8, 1, 7, 3, 5} {
		fmt.Fprintf(&file, "---\nid: %s\nrank: top\n", epics[n])
		want = append(want, epics[n])
	}
	path := writeImportFile(t, "order.yaml", file.String())

	out := runCLIInDir(t, dir, "import", "--path", path)
	if line := "ranked 10 issues at the top of the backlog in order: " + strings.Join(want, ", "); !strings.Contains(out, line) {
		t.Errorf("import output lacks %q:\n%s", line, out)
	}
	assertFacts(t, map[string]string{"import.created": "0", "import.updated": "0", "import.ranked": "10"})

	// Childless epics are no one's work, so lit backlog leaves them out; lit
	// ls lists every issue in the same rank order.
	listing := runCLIInDir(t, dir, "ls")
	at := -1
	for _, id := range want {
		next := strings.Index(listing, id)
		if next <= at {
			t.Fatalf("lit ls does not list %v in file order (%s at %d, after %d):\n%s", want, id, next, at, listing)
		}
		at = next
	}
}

// The invocation's event counts what each import format wrote — a JSON tree
// that creates two, then a YAML file that creates, updates and ranks one issue
// each, moves one under another epic and restates the parent another already
// has, and sets one issue's blocked_by and restates another's — and a file
// whose order names one issue reports it in the singular.
func TestRunImportEventCountsWhatTheFileWrote(t *testing.T) {
	isolateEvents(t)
	dir := newInitTestRepo(t)
	epic := newEpicInDir(t, dir, "home")
	tree := writeImportFile(t, "tree.json", `[
		{"local_id":"e1","title":"Epic","type":"epic","topic":"demo","priority":0},
		{"local_id":"t1","title":"Child","type":"task","topic":"demo","priority":0,"parent":"e1"}
	]`)
	created := runCLIInDir(t, dir, "import", "--path", tree)
	assertFacts(t, map[string]string{"import.created": "2"})
	child, e1 := importedID(t, created, "t1"), importedID(t, created, "e1")

	yaml := writeImportFile(t, "mixed.yaml", fmt.Sprintf("title: fresh\ntype: task\ntopic: demo\nparent: %s\nrank: top\n---\nid: %s\ntitle: home, renamed\nparent: \"\"\nblocked_by: [%s]\n---\nid: %s\nparent: %s\n---\nid: %s\nblocked_by: []\n", epic, epic, e1, child, epic, e1))
	out := runCLIInDir(t, dir, "import", "--path", yaml)
	if line := "ranked 1 issue at the top of " + epic + " in order: "; !strings.Contains(out, line) {
		t.Errorf("import output lacks %q:\n%s", line, out)
	}
	if lines := fmt.Sprintf("reparented 1 issues\n  %s\n1 issues already had the parent the file states\n  %s\n", child, epic); !strings.Contains(out, lines) {
		t.Errorf("import output lacks %q:\n%s", lines, out)
	}
	if lines := fmt.Sprintf("set blocked_by on 1 issues\n  %s\n1 issues already had the blocked_by the file states\n  %s\n", epic, e1); !strings.Contains(out, lines) {
		t.Errorf("import output lacks %q:\n%s", lines, out)
	}
	// The child's move removes one edge and adds one; home's blocked_by adds
	// the other.
	assertFacts(t, map[string]string{"import.created": "1", "import.updated": "1", "import.ranked": "1", "import.reparented": "1", "import.parent_held": "1",
		"import.blocked_by_set": "1", "import.blocked_by_held": "1", "import.edges_added": "2", "import.edges_removed": "1"})
}

// A refused file writes nothing, so the invocation's event counts nothing in
// either format: no update, reparent or blocked_by set from a YAML file refused
// at its last document, and no create from a JSON tree that is refused. The
// event tells a refusal at a write, which reset the workspace, from one the
// gate made before writing.
func TestRunImportEventCountsNothingForARefusedFile(t *testing.T) {
	isolateEvents(t)
	dir := newInitTestRepo(t)
	// One import seeds the fixture: a `lit new` per issue would cost this
	// serial test three invocations for no behavior it pins.
	seeded := runCLIInDir(t, dir, "import", "--path", writeImportFile(t, "seed.yaml", "local_id: from\ntitle: from\ntype: epic\ntopic: demo\n---\nlocal_id: to\ntitle: to\ntype: epic\ntopic: demo\n---\nlocal_id: mover\ntitle: mover\ntype: task\ntopic: demo\nparent: from\n"))
	from, to, mover := importedID(t, seeded, "from"), importedID(t, seeded, "to"), importedID(t, seeded, "mover")
	yaml := writeImportFile(t, "refused.yaml", fmt.Sprintf("title: fresh\ntype: epic\ntopic: demo\n---\nid: %s\nparent: %s\nblocked_by: [%s]\ntitle: renamed\n---\nid: missing-1\ntitle: ghost\n", mover, to, from))
	if out, err := runCLIInDirAllowError(t, dir, "import", "--path", yaml); err == nil {
		t.Fatalf("import succeeded; want the missing update refused:\n%s", out)
	}
	assertFacts(t, map[string]string{"import.created": "0", "import.updated": "0", "import.ranked": "0", "import.reparented": "0", "import.parent_held": "0",
		"import.blocked_by_set": "0", "import.blocked_by_held": "0", "import.edges_added": "0", "import.edges_removed": "0"})
	// Refused at a write, the file was reset to the commit it started from, and
	// the event names that commit with no reset error.
	if facts := writtenFacts(t, lastEvent(t)); facts["import.reset_to"] == nil || facts["import.reset_to"] == "" || facts["import.reset_error"] != "" {
		t.Errorf("event import.reset_to = %v, import.reset_error = %v; want the commit the file reset to and no reset error", facts["import.reset_to"], facts["import.reset_error"])
	}
	// The batch looked for a write another process left staged before it
	// wrote: none here, and the list is present rather than absent.
	if stranded, present := lastEvent(t).Occurrences["stranded_writes.commits"]; !present || len(stranded) != 0 {
		t.Errorf("event stranded_writes.commits = %v (present %v); want present and empty", stranded, present)
	}

	tree := writeImportFile(t, "refused.json", `[
		{"local_id":"a","title":"one","type":"epic","topic":"demo","priority":0},
		{"local_id":"a","title":"two","type":"epic","topic":"demo","priority":0}
	]`)
	if out, err := runCLIInDirAllowError(t, dir, "import", "--path", tree); err == nil {
		t.Fatalf("import succeeded; want the duplicate local_id refused:\n%s", out)
	}
	assertFacts(t, map[string]string{"import.created": "0"})
	// The gate refused the tree before any write, so nothing was reset, and
	// nothing looked for a staged write either.
	ev := lastEvent(t)
	if resetTo, present := writtenFacts(t, ev)["import.reset_to"]; present {
		t.Errorf("event import.reset_to = %v; want none: the gate refused the tree before it wrote", resetTo)
	}
	if stranded, present := ev.Occurrences["stranded_writes.commits"]; present {
		t.Errorf("event stranded_writes.commits = %v; want none: the gate refused the tree before the batch looked", stranded)
	}
}

func assertFacts(t *testing.T, want map[string]string) {
	t.Helper()
	facts := writtenFacts(t, lastEvent(t))
	for fact, value := range want {
		if got := fmt.Sprint(facts[fact]); got != value {
			t.Errorf("event fact %s = %v, want %s", fact, got, value)
		}
	}
}
