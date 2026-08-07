package workflows

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchInjectsMatchingBodyWithIDInterpolated(t *testing.T) {
	workspaceRoot, _ := isolate(t)
	project := filepath.Join(workspaceRoot, ".lit", "workflows")
	writeWorkflow(t, project, "needs-design.md", "---\nlabels: [needs-design]\n---\nTicket <id> needs a design review.")

	var out bytes.Buffer
	err := Dispatch(&out, workspaceRoot, Occasion{Event: EventShowTicket, IssueID: "lit-42", Labels: []string{"needs-design"}})
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
	if err := Dispatch(&out, workspaceRoot, Occasion{Event: EventShowBacklog}); err != nil {
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
	if err := Dispatch(&out, workspaceRoot, Occasion{Event: EventShowTicket, IssueID: "lit-1"}); err != nil {
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
	if err := Dispatch(&out, workspaceRoot, Occasion{Event: EventShowBacklog}); err != nil {
		t.Fatalf("Dispatch() error = %v, want the malformed file to degrade to a warning, not fail dispatch", err)
	}
	if got := out.String(); !strings.Contains(got, "good body") {
		t.Fatalf("Dispatch() wrote %q, want the good definition's body", got)
	}
}

func TestDispatchEmbeddedDoneDefaultFiresOnWorkFinished(t *testing.T) {
	workspaceRoot, _ := isolate(t)

	var out bytes.Buffer
	if err := Dispatch(&out, workspaceRoot, Occasion{Event: EventWorkFinished, IssueID: "lit-7"}); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Ticket lit-7 has been closed.") {
		t.Fatalf("Dispatch() wrote %q, want the embedded done default's body with <id> interpolated", got)
	}
}
