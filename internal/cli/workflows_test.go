package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCLIWorkflow authors a workflow definition file under the current
// workspace's .lit/workflows, mirroring writeProjectWorkflow's shape for
// tests that only need a workspace.Info (no *app.App / store).
func writeCLIWorkflow(t *testing.T, workspaceRoot, rel, content string) {
	t.Helper()
	path := filepath.Join(workspaceRoot, ".lit", "workflows", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// TestWorkflowsOverviewShowsEmbeddedDefaultAtItsEvent pins the floor: with no
// project or global workflow files authored, `lit workflows` still shows the
// embedded "done" default at the event it binds to (work_finished) — the
// same "always at least the embedded layer" guarantee workflows.Load gives.
func TestWorkflowsOverviewShowsEmbeddedDefaultAtItsEvent(t *testing.T) {
	chdirTempRepo(t)

	output, err := runLit(t, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if !strings.Contains(output, `work_finished  [done "Post-close capture reminder" (embedded)]`) {
		t.Fatalf("workflows output = %q, want the embedded done default at work_finished", output)
	}
	if strings.Contains(output, "\nWarnings") {
		t.Fatalf("workflows output = %q, want no warnings section with only the embedded layer loaded", output)
	}
}

// TestWorkflowsOverviewPlacesProjectDefinitionAtEveryBoundDimension is the
// ticket's core done-claim: a project-authored definition shows at its
// activation point(s) with its source layer, and label-scoped definitions
// group under their label too — both dimensions of the same definition,
// since composition across dimensions is AND to fire but each dimension gets
// its own place in this static view.
func TestWorkflowsOverviewPlacesProjectDefinitionAtEveryBoundDimension(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "design/reminder.md",
		"---\nid: need-design-note\nname: Need Design Reminder\nlabels: [need-design]\nevents: [show_ticket]\n---\nNeeds a design pass.")

	output, err := runLit(t, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	want := `need-design-note "Need Design Reminder" (project)`
	if !strings.Contains(output, "show_ticket  ["+want+"]") {
		t.Fatalf("workflows output = %q, want the definition listed under show_ticket", output)
	}
	if !strings.Contains(output, "need-design  ["+want+"]") {
		t.Fatalf("workflows output = %q, want the definition grouped under its label", output)
	}
}

// TestWorkflowsOverviewSurfacesCustomStateOnTheSpine confirms a definition
// bound to a state that isn't one of the three built-ins still puts that
// state on the spine — custom stages are the same shape as built-in ones by
// design (states are open strings, not a closed enum).
func TestWorkflowsOverviewSurfacesCustomStateOnTheSpine(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "verify.md",
		"---\nid: verify-reminder\nstates: [{name: verify, when: enter}]\n---\nVerify before accepting.")

	output, err := runLit(t, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if !strings.Contains(output, "  verify\n    enter  [verify-reminder (project)]\n    exit\n") {
		t.Fatalf("workflows output = %q, want the custom \"verify\" state on the spine with enter bound", output)
	}
}

// TestWorkflowsOverviewFlagsMalformedFileAsAWarning is the "inert/malformed
// file is visibly flagged" done-claim: a file whose frontmatter fails to
// parse never breaks `lit workflows`, and shows up under Warnings instead of
// silently vanishing.
func TestWorkflowsOverviewFlagsMalformedFileAsAWarning(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "broken.md", "---\nlabels: 17\n---\nbad")

	output, err := runLit(t, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if !strings.Contains(output, "\nWarnings (loaded but not fully active)\n") {
		t.Fatalf("workflows output = %q, want a Warnings section", output)
	}
	if !strings.Contains(output, "project broken.md: invalid frontmatter:") {
		t.Fatalf("workflows output = %q, want the malformed file named with its layer and path", output)
	}
	if strings.Contains(output, "invalid frontmatter:\n") {
		t.Fatalf("workflows output = %q, want the warning collapsed onto one line", output)
	}
}

// TestWorkflowsShowResolvesOneDefinition is `lit workflows show <id>`'s
// done-claim: matched frontmatter, source + path, and the full body.
func TestWorkflowsShowResolvesOneDefinition(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "design/reminder.md",
		"---\nid: need-design-note\nname: Need Design Reminder\nlabels: [need-design]\nevents: [show_ticket]\n---\nNeeds a design pass.")

	output, err := runLit(t, "workflows", "show", "need-design-note")
	if err != nil {
		t.Fatalf("Run(workflows show) error = %v", err)
	}
	for _, want := range []string{
		"id: need-design-note",
		"name: Need Design Reminder",
		"source: project",
		"path: design/reminder.md",
		"labels: need-design",
		"events: show_ticket",
		"---\nNeeds a design pass.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("workflows show output = %q, want it to contain %q", output, want)
		}
	}
}

// TestWorkflowsShowUnknownIDIsAValidationError ensures an id that isn't
// loaded fails loudly and by type, not with a silent empty view.
func TestWorkflowsShowUnknownIDIsAValidationError(t *testing.T) {
	chdirTempRepo(t)

	_, err := runLit(t, "workflows", "show", "does-not-exist")
	if err == nil {
		t.Fatalf("Run(workflows show does-not-exist) error = nil, want a ValidationError")
	}
	var validation ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Run(workflows show does-not-exist) error = %v (%T), want ValidationError", err, err)
	}
}

// TestWorkflowsShowMissingIDIsUsageError covers the malformed-invocation
// shape: `lit workflows show` with no id.
func TestWorkflowsShowMissingIDIsUsageError(t *testing.T) {
	chdirTempRepo(t)

	_, err := runLit(t, "workflows", "show")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows show) error = %v (%T), want UsageError", err, err)
	}
}

// TestWorkflowsRejectsOversuppliedArguments guards against silently
// swallowing extra positional args past `show <id>` — splitArgs caps
// positionals at 2 and shunts any overflow into the non-flag args pflag
// tolerates by default, so this must fail loudly rather than rendering a
// definition while quietly ignoring the rest of the command line.
func TestWorkflowsRejectsOversuppliedArguments(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "reminder.md", "---\nid: my-id\nevents: [show_ticket]\n---\nbody")

	_, err := runLit(t, "workflows", "show", "my-id", "extra", "junk")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows show my-id extra junk) error = %v (%T), want UsageError", err, err)
	}
}
