package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestListYAMLRoundTripsThroughImport is the ticket's acceptance pipeline:
// `lit ls --format yaml > f.yaml && lit import --path f.yaml` updates nothing
// and reports every document as held, and editing one title in f.yaml before
// the import updates exactly that one issue. Each invocation's event records
// the format that printed and what the import held and moved.
func TestListYAMLRoundTripsThroughImport(t *testing.T) {
	isolateEvents(t)
	dir := newInitTestRepo(t)
	seeded := runCLIInDir(t, dir, "import", "--path", writeImportFile(t, "seed.yaml", "local_id: epic\ntitle: epic\ntype: epic\ntopic: demo\n---\nlocal_id: full\ntitle: full\ntype: task\ntopic: demo\nparent: epic\ndescription: |-\n  first line\n\n  second paragraph\nassignee: ada\nlabels: [alpha, beta]\n---\nlocal_id: bare\ntitle: bare\ntype: task\ntopic: demo\nparent: epic\n"))
	full := importedID(t, seeded, "full")

	listed := runCLIInDir(t, dir, "ls", "--query", "sort:rank:asc", "--format", "yaml")
	assertFacts(t, map[string]string{"ls.format": "yaml"})
	docs := strings.Count(listed, "id: ")
	if docs != 3 || strings.Count(listed, "\n---\n") != 2 || strings.Contains(listed, "topic:") {
		t.Fatalf("ls --format yaml printed %d documents, want 3 update documents separated by --- and stating no topic:\n%s", docs, listed)
	}
	file := writeImportFile(t, "f.yaml", listed)

	out := runCLIInDir(t, dir, "import", "--path", file)
	if !strings.Contains(out, "updated 0 issues\n3 issues already had the fields the file states\n") {
		t.Errorf("the unedited round trip's import output:\n%s", out)
	}
	assertFacts(t, map[string]string{"import.updated": "0", "import.fields_held": "3", "import.parent_held": "2", "import.blocked_by_held": "3", "import.edges_added": "0", "import.edges_removed": "0"})

	edited := strings.Replace(listed, "title: full\n", "title: full, retitled\n", 1)
	if edited == listed {
		t.Fatalf("the listing states no `title: full` line to edit:\n%s", listed)
	}
	if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
		t.Fatalf("WriteFile(f.yaml) error = %v", err)
	}
	out = runCLIInDir(t, dir, "import", "--path", file)
	if want := fmt.Sprintf("updated 1 issues\n  %s\n2 issues already had the fields the file states\n", full); !strings.Contains(out, want) {
		t.Errorf("the one-title edit's import output lacks %q:\n%s", want, out)
	}
	assertFacts(t, map[string]string{"import.updated": "1", "import.fields_held": "2"})
	if shown := runCLIInDir(t, dir, "show", full); !strings.Contains(shown, "full, retitled") {
		t.Errorf("lit show %s after the edit:\n%s", full, shown)
	}
}

// TestListYAMLRefusesColumns: the update documents print every editable field,
// so a projection asked of them is refused as a usage error before any row is
// printed, rather than ignored.
func TestListYAMLRefusesColumns(t *testing.T) {
	dir := newInitTestRepo(t)
	newEpicInDir(t, dir, "epic")
	out, err := runCLIInDirAllowError(t, dir, "ls", "--format", "yaml", "--columns", "id,title")
	if err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--columns does not apply to --format yaml") {
		t.Fatalf("ls --format yaml --columns: error = %v (exit %d), want the usage refusal", err, ExitCode(err))
	}
	if strings.Contains(out, "id: ") {
		t.Errorf("ls printed documents before refusing:\n%s", out)
	}
}

// TestListYAMLOfNoIssuesPrintsNothing: a selection that matches nothing is an
// empty stream of update documents, printed as success, not a failure to
// close a stream that never started.
func TestListYAMLOfNoIssuesPrintsNothing(t *testing.T) {
	dir := newInitTestRepo(t)
	newEpicInDir(t, dir, "epic")
	if out := runCLIInDir(t, dir, "ls", "--query", "label:matches-nothing", "--format", "yaml"); out != "" {
		t.Errorf("ls --format yaml over no issues printed:\n%s", out)
	}
}

// TestListYAMLKeepsWhatAnotherWriterChanged is the concurrent read-edit-write
// round trip: between `lit ls --format yaml` and `lit import`, another writer
// raises the issue's priority and adds a blocker, and the file edits only the
// title. The import lands the title, keeps the other writer's priority and
// edge, and says so on stdout and on its event. A second file read before
// another writer moves the lane, editing the lane too, is refused as a write
// conflict that names the way on and writes nothing.
func TestListYAMLKeepsWhatAnotherWriterChanged(t *testing.T) {
	isolateEvents(t)
	dir := newInitTestRepo(t)
	seeded := runCLIInDir(t, dir, "import", "--path", writeImportFile(t, "seed.yaml", "local_id: epic\ntitle: epic\ntype: epic\ntopic: demo\n---\nlocal_id: other\ntitle: other\ntype: epic\ntopic: demo\n---\nlocal_id: issue\ntitle: issue\ntype: task\ntopic: demo\nparent: epic\n---\nlocal_id: blocker\ntitle: blocker\ntype: task\ntopic: demo\nparent: other\n"))
	issue, blocker := importedID(t, seeded, "issue"), importedID(t, seeded, "blocker")

	listed := runCLIInDir(t, dir, "ls", "--ids", issue, "--format", "yaml")
	runCLIInDir(t, dir, "update", issue, "--priority", "urgent")
	runCLIInDir(t, dir, "dep", "add", issue, "--blocked-by", blocker)
	file := writeImportFile(t, "f.yaml", strings.Replace(listed, "title: issue\n", "title: issue, retitled\n", 1))

	out := runCLIInDir(t, dir, "import", "--path", file)
	if want := fmt.Sprintf("kept 2 values other writers changed since the file was read\n  %s priority\n  %s blocked-by %s\n", issue, issue, blocker); !strings.Contains(out, want) {
		t.Errorf("the import output lacks %q:\n%s", want, out)
	}
	assertFacts(t, map[string]string{"import.updated": "1", "import.kept": "2", "import.conflicts": "0"})
	shown := runCLIInDir(t, dir, "show", issue)
	for _, want := range []string{"issue, retitled", "priority: urgent", blocker} {
		if !strings.Contains(shown, want) {
			t.Errorf("lit show %s after the import lacks %q:\n%s", issue, want, shown)
		}
	}

	listed = runCLIInDir(t, dir, "ls", "--ids", issue, "--format", "yaml")
	runCLIInDir(t, dir, "update", issue, "--lane", "their-lane")
	conflicting := writeImportFile(t, "g.yaml", strings.Replace(listed, "lane: \"\"\n", "lane: our-lane\n", 1))
	_, err := runCLIInDirAllowError(t, dir, "import", "--path", conflicting)
	if err == nil || ExitCode(err) != ExitConflict || !strings.Contains(err.Error(), issue+" lane: it moved since the file was read") || !strings.Contains(err.Error(), "lit ls --ids") {
		t.Fatalf("import over another writer's lane: error = %v (exit %d), want the write conflict naming the lane and the way on", err, ExitCode(err))
	}
	assertFacts(t, map[string]string{"import.conflicts": "1", "import.updated": "0"})
	if shown := runCLIInDir(t, dir, "show", issue); !strings.Contains(shown, "their-lane") {
		t.Errorf("lit show %s after the refused import lacks the other writer's lane:\n%s", issue, shown)
	}
}
