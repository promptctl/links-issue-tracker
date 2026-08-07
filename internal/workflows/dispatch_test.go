package workflows

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// testWorkspace builds a minimal workspace.Info over a plain temp dir — no
// git repository required — for tests that only need Dispatch's two effects
// (Load(ws.RootDir) and the firing trace under ws.StorageDir).
func testWorkspace(root string) workspace.Info {
	return workspace.Info{
		RootDir:     root,
		WorkspaceID: "test-workspace",
		Location:    workspace.Location{StorageDir: filepath.Join(root, ".lit")},
	}
}

func TestDispatchInjectsMatchingBodyWithIDInterpolated(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "needs-design.md", "---\nlabels: [needs-design]\n---\nTicket <id> needs a design review.")

	var out bytes.Buffer
	err := Dispatch(&out, testWorkspace(workspaceRoot), Occasion{Event: EventShowTicket, IssueID: "lit-42", Labels: []string{"needs-design"}})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Ticket lit-42 needs a design review.") {
		t.Fatalf("Dispatch() wrote %q, want the interpolated body", got)
	}
}

func TestDispatchWritesMultipleMatchesInIDOrder(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "z.md", "---\nid: z\nevents: [show_backlog]\n---\nz body")
	writeWorkflow(t, project, "a.md", "---\nid: a\nevents: [show_backlog]\n---\na body")

	var out bytes.Buffer
	if err := Dispatch(&out, testWorkspace(workspaceRoot), Occasion{Event: EventShowBacklog}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	got := out.String()
	if strings.Index(got, "a body") > strings.Index(got, "z body") || !strings.Contains(got, "a body") || !strings.Contains(got, "z body") {
		t.Fatalf("Dispatch() wrote %q, want \"a body\" before \"z body\"", got)
	}
}

func TestDispatchNonMatchingOccasionWritesNothing(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "only-backlog.md", "---\nevents: [show_backlog]\n---\nbacklog only")

	var out bytes.Buffer
	if err := Dispatch(&out, testWorkspace(workspaceRoot), Occasion{Event: EventShowTicket, IssueID: "lit-1"}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("Dispatch() wrote %q, want nothing", got)
	}
}

// TestDispatchMalformedDefinitionDegradesToWarningNotError pins the
// no-silent-failure/no-hard-failure contract at the seam every command
// routes through: a broken workflow file must never break the command that
// dispatched the occasion.
func TestDispatchMalformedDefinitionDegradesToWarningNotError(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "broken.md", "---\nlabels: 17\n---\nbad")
	writeWorkflow(t, project, "good.md", "---\nevents: [show_backlog]\n---\ngood body")

	var out bytes.Buffer
	if err := Dispatch(&out, testWorkspace(workspaceRoot), Occasion{Event: EventShowBacklog}); err != nil {
		t.Fatalf("Dispatch() error = %v, want the malformed file to degrade to a warning, not fail dispatch", err)
	}
	if got := out.String(); !strings.Contains(got, "good body") {
		t.Fatalf("Dispatch() wrote %q, want the good definition's body", got)
	}
}

func TestDispatchEmbeddedDoneDefaultFiresOnWorkFinished(t *testing.T) {
	workspaceRoot, _ := isolate(t)

	var out bytes.Buffer
	if err := Dispatch(&out, testWorkspace(workspaceRoot), Occasion{Event: EventWorkFinished, IssueID: "lit-7"}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Ticket lit-7 has been closed.") {
		t.Fatalf("Dispatch() wrote %q, want the embedded done default's body with <id> interpolated", got)
	}
}

// TestDispatchRecordsFiringTraceOnlyWhenSomethingFires pins the "proportional
// to guidance actually injected" design: a matching occasion leaves one
// firing-trace record naming why the definition fired, a non-matching one
// leaves the trace directory untouched.
func TestDispatchRecordsFiringTraceOnlyWhenSomethingFires(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	ws := testWorkspace(workspaceRoot)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "needs-design.md", "---\nid: needs-design-note\nlabels: [needs-design]\nevents: [show_ticket]\n---\nNeeds design.")

	var out bytes.Buffer
	if err := Dispatch(&out, ws, Occasion{Event: EventShowTicket, IssueID: "lit-42", Labels: []string{"needs-design"}}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	traceDir := filepath.Join(ws.StorageDir, "traces", "workflows")
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", traceDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("firing trace files = %d, want exactly 1", len(entries))
	}
	payload, err := os.ReadFile(filepath.Join(traceDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile(firing trace) error = %v", err)
	}
	var record FiringRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("Unmarshal(firing trace) error = %v", err)
	}
	if record.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("record.WorkspaceID = %q, want %q", record.WorkspaceID, ws.WorkspaceID)
	}
	if len(record.Fired) != 1 || record.Fired[0].ID != "needs-design-note" {
		t.Fatalf("record.Fired = %+v, want exactly needs-design-note", record.Fired)
	}
	wantReasons := []string{"event:show_ticket", "label:needs-design"}
	if strings.Join(record.Fired[0].Reasons, ",") != strings.Join(wantReasons, ",") {
		t.Fatalf("record.Fired[0].Reasons = %v, want %v", record.Fired[0].Reasons, wantReasons)
	}

	var out2 bytes.Buffer
	if err := Dispatch(&out2, ws, Occasion{Event: EventShowBacklog}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	entriesAfter, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", traceDir, err)
	}
	if len(entriesAfter) != 1 {
		t.Fatalf("firing trace files after a non-matching occasion = %d, want still 1 (no trace for a miss)", len(entriesAfter))
	}
}

// TestDispatchWithEmptyStorageDirNeverWritesRelativeToCWD is a regression
// guard: a caller-supplied workspace.Info with no (or a relative) StorageDir
// — every partially-populated test fixture across the codebase that never
// had reason to set it before firing traces existed — must never cause
// Dispatch to write a trace file relative to the process's working
// directory. A real workspace.Resolve()'d Info always has an absolute
// StorageDir (rooted at git-common-dir), so this path is unreachable in
// production; it guards a boundary Dispatch does not control.
func TestDispatchWithEmptyStorageDirNeverWritesRelativeToCWD(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	scratch := t.TempDir()
	if err := os.Chdir(scratch); err != nil {
		t.Fatalf("Chdir(%s) error = %v", scratch, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	ws := workspace.Info{RootDir: workspaceRoot} // StorageDir deliberately left empty
	var out bytes.Buffer
	if err := Dispatch(&out, ws, Occasion{Event: EventWorkFinished, IssueID: "lit-1"}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(scratch, "traces")); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(traces relative to cwd) error = %v, want no trace directory written relative to the working directory", statErr)
	}
}
