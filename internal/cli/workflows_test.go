package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workflows"
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

// TestWorkflowsEditScaffoldsFreshEventPoint is `edit`'s "no existing
// definition" done-claim for an event point: a fresh project file lands with
// the requested event live and the other two dimensions shown commented.
func TestWorkflowsEditScaffoldsFreshEventPoint(t *testing.T) {
	root := chdirTempRepo(t)

	output, err := runLit(t, "workflows", "edit", "work_started")
	if err != nil {
		t.Fatalf("Run(workflows edit work_started) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "work_started.md")
	if !strings.Contains(output, path) {
		t.Fatalf("workflows edit output = %q, want it to name %q", output, path)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), `events: ["work_started"]`) {
		t.Fatalf("scaffolded content = %q, want the live events line", content)
	}
	if strings.Contains(string(content), "# events: [work_started]") {
		t.Fatalf("scaffolded content = %q, want no redundant commented events example", content)
	}
}

// TestWorkflowsEditScaffoldsFreshStateExitPoint covers the `<state>:exit`
// syntax and its distinct filename from the bare (enter) form.
func TestWorkflowsEditScaffoldsFreshStateExitPoint(t *testing.T) {
	root := chdirTempRepo(t)

	if _, err := runLit(t, "workflows", "edit", "closed:exit"); err != nil {
		t.Fatalf("Run(workflows edit closed:exit) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "closed_exit.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), `states: [{name: "closed", when: exit}]`) {
		t.Fatalf("scaffolded content = %q, want the live exit state activation", content)
	}
}

// TestWorkflowsEditScaffoldsFreshStateExitPointWithColonInStateName pins the
// last-colon split: a state name that itself contains a colon (states are
// documented as open strings with no restriction against one) must still
// parse its :enter/:exit suffix correctly rather than misclassifying the
// whole point as a label.
func TestWorkflowsEditScaffoldsFreshStateExitPointWithColonInStateName(t *testing.T) {
	root := chdirTempRepo(t)

	if _, err := runLit(t, "workflows", "edit", "deploy:staging:enter"); err != nil {
		t.Fatalf("Run(workflows edit deploy:staging:enter) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "deploy-staging.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), `states: ["deploy:staging"]`) {
		t.Fatalf("scaffolded content = %q, want the live state activation with the colon preserved in the state name", content)
	}
}

// TestWorkflowsEditScaffoldsFreshLabelPointAsFallback covers the fallback
// branch: a point that is neither a known event nor a built-in state name is
// treated as a label.
func TestWorkflowsEditScaffoldsFreshLabelPointAsFallback(t *testing.T) {
	root := chdirTempRepo(t)

	if _, err := runLit(t, "workflows", "edit", "needs-design"); err != nil {
		t.Fatalf("Run(workflows edit needs-design) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "needs-design.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), `labels: ["needs-design"]`) {
		t.Fatalf("scaffolded content = %q, want the live labels line", content)
	}
}

// TestWorkflowsEditScaffoldsFreshStateWithCommaAsOneStateActivation is the
// YAML-escaping regression: unlike labels (model.NormalizeLabel rejects a
// comma outright), state names have no such restriction, so a state
// containing a comma is the domain-valid case where an unquoted flow
// sequence would silently split one activation into two.
func TestWorkflowsEditScaffoldsFreshStateWithCommaAsOneStateActivation(t *testing.T) {
	root := chdirTempRepo(t)

	if _, err := runLit(t, "workflows", "edit", "foo,bar:enter"); err != nil {
		t.Fatalf("Run(workflows edit foo,bar:enter) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "foo-bar.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), `states: ["foo,bar"]`) {
		t.Fatalf("scaffolded content = %q, want the comma-containing state name double-quoted", content)
	}

	set := workflows.Load(root)
	def, ok := set.Lookup("foo-bar")
	if !ok {
		t.Fatalf("workflows.Load(%s) has no definition with id foo-bar; loaded ids: %+v", root, set.Definitions)
	}
	if len(def.States) != 1 || def.States[0].State != "foo,bar" || def.States[0].When != workflows.WhenEnter {
		t.Fatalf("def.States = %+v, want exactly one activation {foo,bar enter}", def.States)
	}
}

// TestWorkflowsEditOverridesEmbeddedDefaultVerbatim is the ticket's worked
// example: overriding the "done" embedded default copies its exact
// frontmatter and body into a new project file, ready to customize.
func TestWorkflowsEditOverridesEmbeddedDefaultVerbatim(t *testing.T) {
	root := chdirTempRepo(t)

	if _, err := runLit(t, "workflows", "edit", "done"); err != nil {
		t.Fatalf("Run(workflows edit done) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "done.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), "id: done") || !strings.Contains(string(content), "Ticket <id> has been closed.") {
		t.Fatalf("scaffolded override = %q, want the embedded done.md content verbatim", content)
	}

	// The override now resolves as the project-layer source.
	output, err := runLit(t, "workflows", "show", "done")
	if err != nil {
		t.Fatalf("Run(workflows show done) error = %v", err)
	}
	if !strings.Contains(output, "source: project") {
		t.Fatalf("workflows show done output = %q, want source: project", output)
	}
}

// TestWriteWorkflowScaffoldEnforcesNoClobberEvenPastTheFastPathCheck pins the
// TOCTOU fix directly: writeWorkflowScaffold's O_EXCL open is the actual
// enforcer, not just refuseExistingFile's earlier stat — calling it twice for
// the same path (as a concurrent `edit` racing past the first call's stat
// check would) must fail on the second call, never silently overwrite the
// first call's content.
func TestWriteWorkflowScaffoldEnforcesNoClobberEvenPastTheFastPathCheck(t *testing.T) {
	root := chdirTempRepo(t)
	path := filepath.Join(root, ".lit", "workflows", "race.md")

	if err := writeWorkflowScaffold(path, "race", []byte("first")); err != nil {
		t.Fatalf("writeWorkflowScaffold() first call error = %v", err)
	}
	err := writeWorkflowScaffold(path, "race", []byte("second"))
	var conflict MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("writeWorkflowScaffold() second call error = %v (%T), want MergeConflictError", err, err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(content) != "first" {
		t.Fatalf("content = %q, want the first call's content untouched", content)
	}
}

// TestWorkflowsEditRefusesToClobberAnExistingFile is the never-silently-
// overwrite guard: a fresh scaffold whose target filename is already
// occupied (by an unrelated definition) fails loudly instead of clobbering it.
func TestWorkflowsEditRefusesToClobberAnExistingFile(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "blocked.md", "---\nid: something-else\nlabels: [other]\n---\nunrelated body")

	_, err := runLit(t, "workflows", "edit", "blocked")
	var conflict MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Run(workflows edit blocked) error = %v (%T), want MergeConflictError", err, err)
	}
	content, readErr := os.ReadFile(filepath.Join(root, ".lit", "workflows", "blocked.md"))
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if !strings.Contains(string(content), "unrelated body") {
		t.Fatalf("blocked.md content = %q, want it untouched", content)
	}
}

// TestWorkflowsEditReopensAnExistingProjectOverrideWithoutRescaffolding
// pins edit's idempotence: editing an id that's already a project override
// just opens the existing file — it never re-scaffolds or errors.
func TestWorkflowsEditReopensAnExistingProjectOverrideWithoutRescaffolding(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "custom.md", "---\nid: my-custom\nevents: [show_ticket]\n---\nalready mine")

	output, err := runLit(t, "workflows", "edit", "my-custom")
	if err != nil {
		t.Fatalf("Run(workflows edit my-custom) error = %v", err)
	}
	path := filepath.Join(root, ".lit", "workflows", "custom.md")
	if !strings.Contains(output, path) {
		t.Fatalf("workflows edit output = %q, want it to name %q", output, path)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, readErr)
	}
	if !strings.Contains(string(content), "already mine") {
		t.Fatalf("custom.md content = %q, want the original body untouched", content)
	}
}

// TestWorkflowsDryRunExplainsAHypotheticalMatch is the ticket's own worked
// example: "what would inject on work_finished for a ticket labeled
// needs-design?"
func TestWorkflowsDryRunExplainsAHypotheticalMatch(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "design.md", "---\nid: design-note\nlabels: [needs-design]\nevents: [work_finished]\n---\nTicket <id>: needs a design pass.")

	output, err := runLit(t, "workflows", "dry-run", "--event", "work_finished", "--label", "needs-design", "--issue", "lit-7")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	if !strings.Contains(output, "Fired (2)") {
		t.Fatalf("dry-run output = %q, want 2 fired (design-note plus the embedded done default)", output)
	}
	if !strings.Contains(output, "design-note (project)  [event:work_finished, label:needs-design]") {
		t.Fatalf("dry-run output = %q, want design-note listed with both matched reasons", output)
	}
	if !strings.Contains(output, "Ticket lit-7: needs a design pass.") {
		t.Fatalf("dry-run output = %q, want the interpolated body previewed", output)
	}
}

// TestWorkflowsDryRunNoMatchesReportsNone covers the miss shape.
func TestWorkflowsDryRunNoMatchesReportsNone(t *testing.T) {
	chdirTempRepo(t)

	output, err := runLit(t, "workflows", "dry-run", "--event", "show_backlog")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	if !strings.Contains(output, "Fired (0)\n  (none)") {
		t.Fatalf("dry-run output = %q, want Fired (0) with no matches", output)
	}
}

// TestWorkflowsEditRejectsOversuppliedArguments guards the same silent-
// swallow shape TestWorkflowsRejectsOversuppliedArguments pins for `show`.
func TestWorkflowsEditRejectsOversuppliedArguments(t *testing.T) {
	chdirTempRepo(t)

	_, err := runLit(t, "workflows", "edit", "work_started", "extra")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows edit work_started extra) error = %v (%T), want UsageError", err, err)
	}
}

// TestWorkflowsDryRunRejectsUnknownFlags guards against a typo'd flag
// silently being ignored as a stray positional argument.
func TestWorkflowsDryRunRejectsUnknownFlags(t *testing.T) {
	chdirTempRepo(t)

	_, err := runLit(t, "workflows", "dry-run", "--evnt", "work_finished")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows dry-run --evnt) error = %v (%T), want UsageError", err, err)
	}
}

// TestWorkflowsDryRunNeverRecordsAFiringTrace pins that a hypothetical never
// touches the real firing-trace directory — nothing actually fired.
func TestWorkflowsDryRunNeverRecordsAFiringTrace(t *testing.T) {
	root := chdirTempRepo(t)
	writeCLIWorkflow(t, root, "design.md", "---\nid: design-note\nevents: [work_finished]\n---\nbody")

	if _, err := runLit(t, "workflows", "dry-run", "--event", "work_finished"); err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	traceDir := filepath.Join(root, ".lit", "traces", "workflows")
	if _, statErr := os.Stat(traceDir); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(%s) error = %v, want the trace dir to not exist (dry-run never records)", traceDir, statErr)
	}
}
