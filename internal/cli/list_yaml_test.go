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
	assertFacts(t, map[string]string{"import.updated": "0", "import.fields_held": "3", "import.parent_held": "3", "import.blocked_by_held": "3", "import.edges_added": "0", "import.edges_removed": "0"})

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
