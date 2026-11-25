package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
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

// TestWorkflowsOverviewShowsEmbeddedDefaultAtItsCrossing pins the floor: with
// no project or global workflow files authored, `lit workflows` still shows
// the embedded "done" default at the crossing it binds to (enter closed) — the
// same "always at least the embedded layer" guarantee workflows.Load gives.
func TestWorkflowsOverviewShowsEmbeddedDefaultAtItsCrossing(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if !strings.Contains(output, "\n"+`  enter closed  [done "Post-close capture reminder" (embedded)]`+"\n") {
		t.Fatalf("workflows output = %q, want the embedded done default at enter closed", output)
	}
	if strings.Contains(output, "\nWarnings") {
		t.Fatalf("workflows output = %q, want no warnings section with only the embedded layer loaded", output)
	}
}

// TestWorkflowsOverviewPlacesProjectDefinitionAtEveryBoundKey is the core
// done-claim: a project-authored definition shows at each of its `when:`
// moments with its source layer, and label-scoped definitions group under
// their label too — both keys of the same definition, since composition
// across keys is AND to fire but each key gets its own place in this static
// view.
func TestWorkflowsOverviewPlacesProjectDefinitionAtEveryBoundKey(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "design/reminder.md",
		"---\nid: need-design-note\nname: Need Design Reminder\nlabels: [need-design]\nwhen: [show_ticket, exit review]\n---\nNeeds a design pass.")

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	want := `need-design-note "Need Design Reminder" (project)`
	for _, moment := range []string{"show_ticket", "exit review"} {
		if !strings.Contains(output, "\n  "+moment+"  ["+want+"]\n") {
			t.Fatalf("workflows output = %q, want the definition listed at %s", output, moment)
		}
	}
	if !strings.Contains(output, "need-design  ["+want+"]") {
		t.Fatalf("workflows output = %q, want the definition grouped under its label", output)
	}
}

// TestWorkflowsOverviewSurfacesCustomStateOnTheSpine confirms a definition
// bound to a state that isn't one of the built-ins still puts that
// state on the spine — custom stages are the same shape as built-in ones by
// design (states are open strings, not a closed enum).
func TestWorkflowsOverviewSurfacesCustomStateOnTheSpine(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "verify.md",
		"---\nid: verify-reminder\nwhen: [enter verify]\n---\nVerify before accepting.")

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if !strings.Contains(output, "\n  exit closed\n  enter verify  [verify-reminder (project)]\n  exit verify\n") {
		t.Fatalf("workflows output = %q, want the custom \"verify\" state on the spine after the built-ins, with enter bound", output)
	}
}

// TestWorkflowsOverviewFlagsMalformedFileAsAWarning is the "inert/malformed
// file is visibly flagged" done-claim: a file whose frontmatter fails to
// parse never breaks `lit workflows`, and shows up under Warnings instead of
// silently vanishing.
func TestWorkflowsOverviewFlagsMalformedFileAsAWarning(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "broken.md", "---\nlabels: 17\n---\nbad")

	output, err := runLit(t, root, "workflows")
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
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "design/reminder.md",
		"---\nid: need-design-note\nname: Need Design Reminder\nlabels: [need-design]\nwhen: [show_ticket, enter closed]\n---\nNeeds a design pass.")

	output, err := runLit(t, root, "workflows", "show", "need-design-note")
	if err != nil {
		t.Fatalf("Run(workflows show) error = %v", err)
	}
	for _, want := range []string{
		"id: need-design-note",
		"name: Need Design Reminder",
		"source: project",
		"path: design/reminder.md",
		"when: show_ticket, enter closed",
		"labels: need-design",
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
	t.Parallel()
	root := tempRepo(t)

	_, err := runLit(t, root, "workflows", "show", "does-not-exist")
	if err == nil {
		t.Fatalf("Run(workflows show does-not-exist) error = nil, want a model.ValidationError")
	}
	var validation model.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Run(workflows show does-not-exist) error = %v (%T), want model.ValidationError", err, err)
	}
}

// TestWorkflowsShowMissingIDIsUsageError covers the malformed-invocation
// shape: `lit workflows show` with no id.
func TestWorkflowsShowMissingIDIsUsageError(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	_, err := runLit(t, root, "workflows", "show")
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
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "reminder.md", "---\nid: my-id\nwhen: [show_ticket]\n---\nbody")

	_, err := runLit(t, root, "workflows", "show", "my-id", "extra", "junk")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows show my-id extra junk) error = %v (%T), want UsageError", err, err)
	}
}

// TestWorkflowsEditScaffoldsFreshMomentPoints is `edit`'s "no existing
// definition" done-claim for a moment: a point spelled as a `when:` entry —
// an event, or entering or exiting any state, built-in or custom — lands as a
// fresh project file with that entry live under `when:` and the labels key
// shown commented, and loads binding exactly that moment.
func TestWorkflowsEditScaffoldsFreshMomentPoints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		point, file string
		want        workflows.Moment
	}{
		{"show_ticket", "show_ticket.md", workflows.EventShowTicket},
		{"enter review", "enter-review.md", workflows.Crossing{Side: workflows.Enter, State: "review"}},
		{"exit closed", "exit-closed.md", workflows.Crossing{Side: workflows.Exit, State: "closed"}},
		{"Enter  In_Progress", "enter-in_progress.md", workflows.Crossing{Side: workflows.Enter, State: "in_progress"}},
		// A state is an open string: a colon or a comma in its name is the
		// state's, and quoting keeps a comma from splitting one entry in two.
		{"enter deploy:staging", "enter-deploy-staging.md", workflows.Crossing{Side: workflows.Enter, State: "deploy:staging"}},
		{"exit foo,bar", "exit-foo-bar.md", workflows.Crossing{Side: workflows.Exit, State: "foo,bar"}},
	} {
		t.Run(tc.point, func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t)
			output, err := runLit(t, root, "workflows", "edit", tc.point)
			if err != nil {
				t.Fatalf("Run(workflows edit %q) error = %v", tc.point, err)
			}
			path := filepath.Join(root, ".lit", "workflows", tc.file)
			if !strings.Contains(output, path) {
				t.Fatalf("workflows edit output = %q, want it to name %q", output, path)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, readErr)
			}
			if live := "\nwhen: [" + strconv.Quote(tc.want.String()) + "]\n"; !strings.Contains(string(content), live) {
				t.Fatalf("scaffolded content = %q, want the live line %q", content, live)
			}
			if strings.Contains(string(content), "# when: [") || !strings.Contains(string(content), "# labels: [") {
				t.Fatalf("scaffolded content = %q, want labels commented as reference and no redundant when example", content)
			}
			set := workflows.Load(runEnv(t), root)
			def, ok := set.Lookup(strings.TrimSuffix(tc.file, ".md"))
			if !ok {
				t.Fatalf("workflows.Load(%s) has no definition for %s; loaded: %+v", root, tc.file, set.Definitions)
			}
			if !reflect.DeepEqual(def.When, []workflows.Moment{tc.want}) || def.Labels != nil {
				t.Fatalf("definition = %+v, want exactly when: [%s]", def, tc.want)
			}
			if len(set.Warnings) != 0 {
				t.Fatalf("warnings = %v, want the scaffold to load cleanly", set.Warnings)
			}
		})
	}
}

// TestWorkflowsEditScaffoldsFreshLabelPointAsFallback covers the fallback:
// a point that is no moment this lit fires — a bare state name among them,
// since a state is crossed, not named — is treated as a label.
func TestWorkflowsEditScaffoldsFreshLabelPointAsFallback(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"needs-design", "review"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t)
			if _, err := runLit(t, root, "workflows", "edit", point); err != nil {
				t.Fatalf("Run(workflows edit %s) error = %v", point, err)
			}
			path := filepath.Join(root, ".lit", "workflows", point+".md")
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, readErr)
			}
			if !strings.Contains(string(content), "\nlabels: [\""+point+"\"]\n") {
				t.Fatalf("scaffolded content = %q, want the live labels line", content)
			}
			if !strings.Contains(string(content), "# when: [") {
				t.Fatalf("scaffolded content = %q, want the when key shown commented as reference", content)
			}
		})
	}
}

// TestWorkflowsEditOverridesEmbeddedDefaultVerbatim is the ticket's worked
// example: overriding the "done" embedded default copies its exact
// frontmatter and body into a new project file, ready to customize.
func TestWorkflowsEditOverridesEmbeddedDefaultVerbatim(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	if _, err := runLit(t, root, "workflows", "edit", "done"); err != nil {
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
	output, err := runLit(t, root, "workflows", "show", "done")
	if err != nil {
		t.Fatalf("Run(workflows show done) error = %v", err)
	}
	if !strings.Contains(output, "source: project") {
		t.Fatalf("workflows show done output = %q, want source: project", output)
	}
}

// TestWriteWorkflowScaffoldEnforcesNoClobberEvenPastTheFastPathCheck pins
// directly that writeWorkflowScaffold's O_EXCL open is the actual enforcer, not
// just refuseExistingFile's earlier stat — calling it twice for the same path
// (as a concurrent `edit` racing past the first call's stat check would) must
// fail on the second call, never silently overwrite the first call's content.
func TestWriteWorkflowScaffoldEnforcesNoClobberEvenPastTheFastPathCheck(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
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
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "blocked.md", "---\nid: something-else\nlabels: [other]\n---\nunrelated body")

	_, err := runLit(t, root, "workflows", "edit", "blocked")
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
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "custom.md", "---\nid: my-custom\nwhen: [show_ticket]\n---\nalready mine")

	output, err := runLit(t, root, "workflows", "edit", "my-custom")
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

// TestWorkflowsDryRunExplainsAHypotheticalMatch is the worked example: "what
// would inject when a ticket labeled needs-design enters closed?"
func TestWorkflowsDryRunExplainsAHypotheticalMatch(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "design.md", "---\nid: design-note\nlabels: [needs-design]\nwhen: [enter closed]\n---\nTicket <id>: needs a design pass.")

	output, err := runLit(t, root, "workflows", "dry-run", "--when", "enter closed", "--label", "needs-design", "--issue", "lit-7")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	for _, want := range []string{
		`occasion: when=["enter closed"] labels=needs-design issue=lit-7` + "\n",
		"Fired (2)\n",
		`done "Post-close capture reminder" (embedded)  [when:enter closed]` + "\n",
		"design-note (project)  [when:enter closed, label:needs-design]\n",
		"Ticket lit-7: needs a design pass.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output = %q, want it to contain %q", output, want)
		}
	}
}

// TestWorkflowsDryRunNoMatchesReportsNone covers the miss shape.
func TestWorkflowsDryRunNoMatchesReportsNone(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	output, err := runLit(t, root, "workflows", "dry-run", "--when", "show_backlog")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	if !strings.Contains(output, "Fired (0)\n  (none)") {
		t.Fatalf("dry-run output = %q, want Fired (0) with no matches", output)
	}
}

// dry-run is asked in every spelling a `when:` entry takes — an event, an
// entry into a state, an exit from one, built-in or custom — and reads each
// the way the frontmatter does, so a definition fires in a dry-run exactly
// when it says it fires. A state name is an open string that can carry a
// comma, so the occasion line keeps one crossing of a state named foo,bar
// apart from two crossings.
func TestWorkflowsDryRunReadsEveryWhenSpelling(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	for id, when := range map[string]string{
		"on-show":     "show_ticket",
		"on-exit":     "exit plan",
		"on-enter":    "enter review",
		"on-foo-bar":  `"enter foo,bar"`,
		"on-staging":  "exit deploy:staging",
		"on-progress": "enter in_progress",
	} {
		writeCLIWorkflow(t, root, id+".md", "---\nid: "+id+"\nwhen: ["+when+"]\n---\n"+id+" body")
	}

	for _, tc := range []struct {
		when       []string
		occasion   string
		firedLines []string
	}{
		{[]string{"show_ticket"}, `when=["show_ticket"]`, []string{"on-show (project)  [when:show_ticket]"}},
		{[]string{"exit plan"}, `when=["exit plan"]`, []string{"on-exit (project)  [when:exit plan]"}},
		{[]string{"Enter  Review"}, `when=["enter review"]`, []string{
			`phase-review "Record when the merge is under way" (embedded)  [when:enter review]`,
			"on-enter (project)  [when:enter review]",
		}},
		{[]string{"enter foo,bar"}, `when=["enter foo,bar"]`, []string{"on-foo-bar (project)  [when:enter foo,bar]"}},
		{[]string{"enter foo", "enter bar"}, `when=["enter foo","enter bar"]`, nil},
		{[]string{"exit deploy:staging"}, `when=["exit deploy:staging"]`, []string{"on-staging (project)  [when:exit deploy:staging]"}},
		{[]string{"exit open", "enter in_progress", "enter plan"}, `when=["exit open","enter in_progress","enter plan"]`, []string{
			`phase-plan "Plan the ticket, then record when the code starts" (embedded)  [when:enter plan]`,
			"on-progress (project)  [when:enter in_progress]",
		}},
		{nil, `when=[]`, nil},
	} {
		args := []string{"workflows", "dry-run"}
		for _, when := range tc.when {
			args = append(args, "--when", when)
		}
		output, err := runLit(t, root, args...)
		if err != nil {
			t.Fatalf("Run(%v) error = %v", args, err)
		}
		if want := "occasion: " + tc.occasion + " labels=- issue=-\n"; !strings.Contains(output, want) {
			t.Errorf("%v output = %q, want the occasion line %q", args, output, want)
		}
		if want := "Fired (" + strconv.Itoa(len(tc.firedLines)) + ")\n"; !strings.Contains(output, want) {
			t.Errorf("%v output = %q, want %q", args, output, want)
		}
		for _, line := range tc.firedLines {
			if !strings.Contains(output, "\n  "+line+"\n") {
				t.Errorf("%v output = %q, want the fired line %q", args, output, line)
			}
		}
	}
}

// dry-run refuses a moment no command fires — an event outside the catalog,
// the transition events retired among them — rather than explain a match on
// something that cannot happen, and names the spellings that can.
func TestWorkflowsDryRunRefusesAMomentLitNeverFires(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	for _, when := range []string{"work_finished", "ticket_closed", "warp_drive_engaged", "enter"} {
		_, err := runLit(t, root, "workflows", "dry-run", "--when", when)
		var validation model.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("dry-run --when %s error = %v (%T), want a ValidationError", when, err, err)
		}
		for _, want := range []string{strconv.Quote(when), "enter <state> or exit <state>", "show_ticket"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("dry-run --when %s error = %q, want it to name %s", when, err, want)
			}
		}
	}
}

// TestWorkflowsEditRejectsOversuppliedArguments guards the same silent-
// swallow shape TestWorkflowsRejectsOversuppliedArguments pins for `show`.
func TestWorkflowsEditRejectsOversuppliedArguments(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	_, err := runLit(t, root, "workflows", "edit", "show_ticket", "extra")
	if _, ok := err.(UsageError); !ok {
		t.Fatalf("Run(workflows edit show_ticket extra) error = %v (%T), want UsageError", err, err)
	}
}

// TestWorkflowsDryRunRejectsUnknownFlags guards against a typo'd flag
// silently being ignored as a stray positional argument.
func TestWorkflowsDryRunRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	for _, flag := range []string{"--whn", "--event", "--enter", "--exit"} {
		_, err := runLit(t, root, "workflows", "dry-run", flag, "show_ticket")
		if _, ok := err.(UsageError); !ok {
			t.Fatalf("Run(workflows dry-run %s) error = %v (%T), want UsageError", flag, err, err)
		}
	}
}

// TestWorkflowsDryRunNeverRecordsAFiringTrace pins that a hypothetical never
// touches the real firing-trace directory — nothing actually fired.
func TestWorkflowsDryRunNeverRecordsAFiringTrace(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "design.md", "---\nid: design-note\nwhen: [enter closed]\n---\nbody")

	if _, err := runLit(t, root, "workflows", "dry-run", "--when", "enter closed"); err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	traceDir := filepath.Join(root, ".lit", "traces", "workflows")
	if _, statErr := os.Stat(traceDir); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(%s) error = %v, want the trace dir to not exist (dry-run never records)", traceDir, statErr)
	}
}

// The phases are built-in states: the overview walks entering and exiting
// each after in_progress, in work order, before closed, each phase with the
// embedded default bound to entering it — after the event catalog, in the
// spelling a `when:` entry uses.
func TestWorkflowsOverviewWalksThePhasesInsideInProgress(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	shipped := map[string]string{
		"plan":   `phase-plan "Plan the ticket, then record when the code starts"`,
		"impl":   `phase-impl "Record when the PR is up for review"`,
		"review": `phase-review "Record when the merge is under way"`,
		"merge":  `phase-merge "Close the ticket once it has merged"`,
	}
	shipped["closed"] = `done "Post-close capture reminder"`
	var spine strings.Builder
	spine.WriteString("\nWhen\n")
	for _, event := range workflows.Catalog() {
		spine.WriteString("  " + string(event) + "\n")
	}
	for _, state := range []string{"open", "in_progress", "plan", "impl", "review", "merge", "closed"} {
		enter := "  enter " + state + "\n"
		if def, ok := shipped[state]; ok {
			enter = "  enter " + state + "  [" + def + " (embedded)]\n"
		}
		spine.WriteString(enter + "  exit " + state + "\n")
	}
	spine.WriteString("\nLabels\n")
	if !strings.Contains(output, spine.String()) {
		t.Fatalf("workflows output = %q, want the When spine %q", output, spine.String())
	}
}

// lit workflows --help names every state and event a definition can bind to:
// each phase among the built-in states, and each catalog event in the event
// catalog with the command that dispatches it.
func TestWorkflowsHelpNamesThePhasesAndEveryEvent(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	page, err := runLit(t, root, "workflows", "--help")
	if err != nil {
		t.Fatalf("Run(workflows --help) error = %v", err)
	}
	flowed := strings.Join(strings.Fields(page), " ")
	phases := make([]string, 0, len(model.Phases()))
	for _, phase := range model.Phases() {
		phases = append(phases, string(phase))
	}
	listed := strings.Join(phases[:len(phases)-1], ", ") + " and " + phases[len(phases)-1]
	if want := "the phases " + listed + " inside in_progress"; !strings.Contains(flowed, want) {
		t.Errorf("workflows --help does not say %q:\n%s", want, page)
	}
	for _, event := range workflows.Catalog() {
		if !strings.Contains(page, "\n    "+string(event)+" ") {
			t.Errorf("workflows --help event catalog does not list %s:\n%s", event, page)
		}
	}
}

// TestWorkflowsOverviewPrintsLeastToMostSpecificFirst pins the print order at a
// shared point: the embedded default is listed before the project definition
// bound to the same crossing, even though the project id sorts first.
func TestWorkflowsOverviewPrintsLeastToMostSpecificFirst(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "closing.md", "---\nid: close-note\nwhen: [enter closed]\n---\nNote it.")

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	want := `enter closed  [done "Post-close capture reminder" (embedded), close-note (project)]`
	if !strings.Contains(output, want) {
		t.Fatalf("workflows output = %q, want %q", output, want)
	}
}
