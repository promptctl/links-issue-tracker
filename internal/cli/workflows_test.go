package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// TestWorkflowsOverviewWarnsOfAStateTheLifecycleLacks: the spine is the
// lifecycle's states, so a definition bound to a state lit does not have puts
// nothing on it, and the overview's warnings say the entry never fires and
// name the states it can.
func TestWorkflowsOverviewWarnsOfAStateTheLifecycleLacks(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "verify.md",
		"---\nid: verify-reminder\nwhen: [enter verify]\n---\nVerify before accepting.")

	output, err := runLit(t, root, "workflows")
	if err != nil {
		t.Fatalf("Run(workflows) error = %v", err)
	}
	if strings.Contains(output, "enter verify  [") || strings.Contains(output, "\n  exit verify\n") {
		t.Fatalf("workflows output = %q, want no \"verify\" state on the spine", output)
	}
	if want := `project verify.md: when: entry "enter verify" never fires: unknown state "verify": a ticket's states are open, in_progress, plan, impl, review, merge, closed`; !strings.Contains(output, want) {
		t.Fatalf("workflows output = %q, want the warning %q", output, want)
	}
}

// TestWorkflowsOverviewFlagsMalformedFileAsAWarning is the "keyless/malformed
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

// A definition that fires less than it says — or never — shows why under
// its fields: the load's warnings for its file. One that never fires says
// `fires: never` in place of keys it does not fire on.
func TestWorkflowsShowNamesWhyADefinitionFiresLessThanItSays(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "broken.md", "---\nid: broken\nwhen: enter closed\n---\nbad")
	writeCLIWorkflow(t, root, "unread.md", "---\nid: unread\nwhen: [enter closed]\nevents: [work_finished]\n---\nold")
	writeCLIWorkflow(t, root, "partial.md", "---\nid: partial\nwhen: [enter reveiw, exit plan]\n---\nhalf")

	for id, want := range map[string][]string{
		"broken":  {"path: broken.md\nfires: never\n", "\nWarnings (loaded but not fully active)\n  project broken.md: invalid frontmatter: when:", "---\nbad"},
		"unread":  {"path: unread.md\nfires: never\n", "\n  project unread.md: this lit does not read \"events\"", "---\nold"},
		"partial": {"when: exit plan\n", "\n  project partial.md: when: entry \"enter reveiw\" never fires: unknown state \"reveiw\"", "---\nhalf"},
	} {
		output, err := runLit(t, root, "workflows", "show", id)
		if err != nil {
			t.Fatalf("Run(workflows show %s) error = %v", id, err)
		}
		for _, line := range want {
			if !strings.Contains(output, line) {
				t.Errorf("workflows show %s output = %q, want it to contain %q", id, output, line)
			}
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

// `lit workflows edit --when <moment> --label <name>` scaffolds a fresh
// project file firing on exactly the keys the flags name, each entry live in
// the spelling it reads back as and a key left out shown commented, in a file
// named after what it binds; it loads with no warning. A label that reads
// like a moment is a label, because --label says so.
func TestWorkflowsEditScaffoldsADefinitionOnTheKeysItNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args         []string
		file         string
		when, labels []string
	}{
		{[]string{"--when", "show_ticket"}, "show_ticket.md", []string{"show_ticket"}, nil},
		{[]string{"--when", "enter review"}, "enter-review.md", []string{"enter review"}, nil},
		{[]string{"--when", "Enter  In_Progress"}, "enter-in_progress.md", []string{"enter in_progress"}, nil},
		{[]string{"--when", "exit in-progress"}, "exit-in_progress.md", []string{"exit in_progress"}, nil},
		{[]string{"--when", "enter archived", "--when", "exit live"}, "exit-live+enter-archived.md", []string{"exit live", "enter archived"}, nil},
		{[]string{"--when", "enter closed", "--when", "show_ticket"}, "show_ticket+enter-closed.md", []string{"show_ticket", "enter closed"}, nil},
		{[]string{"--label", "Needs-Design"}, "needs-design.md", nil, []string{"needs-design"}},
		{[]string{"--label", "needs-design", "--label", "blocked"}, "blocked+needs-design.md", nil, []string{"blocked", "needs-design"}},
		{[]string{"--label", "exit plan"}, "exit-plan.md", nil, []string{"exit plan"}},
		{[]string{"--when", "enter closed", "--label", "needs-design"}, "enter-closed+needs-design.md", []string{"enter closed"}, []string{"needs-design"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t)
			output, err := runLit(t, root, append([]string{"workflows", "edit"}, tc.args...)...)
			if err != nil {
				t.Fatalf("Run(workflows edit %v) error = %v", tc.args, err)
			}
			path := filepath.Join(root, ".lit", "workflows", tc.file)
			if !strings.Contains(output, "scaffolded a new definition at ") || !strings.Contains(output, tc.file) {
				t.Fatalf("workflows edit output = %q, want it to name the scaffold %s", output, path)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(%s) error = %v", path, readErr)
			}
			if commented := strings.Contains(string(content), "\n# when: ["); commented != (len(tc.when) == 0) {
				t.Fatalf("scaffolded content = %q, want when: commented only when no --when was given", content)
			}
			if commented := strings.Contains(string(content), "\n# labels: ["); commented != (len(tc.labels) == 0) {
				t.Fatalf("scaffolded content = %q, want labels: commented only when no --label was given", content)
			}
			set := workflows.Load(runEnv(t), root)
			def, ok := set.Lookup(strings.TrimSuffix(tc.file, ".md"))
			if !ok {
				t.Fatalf("workflows.Load(%s) has no definition for %s; loaded: %+v", root, tc.file, set.Definitions)
			}
			keys, fires := def.Activation.Keys()
			if !fires || !slices.Equal(workflows.Spellings(keys.When.Entries()), tc.when) || !slices.Equal(keys.Labels.Entries(), tc.labels) {
				t.Fatalf("definition = %+v, want when %v and labels %v", def, tc.when, tc.labels)
			}
			if len(set.Warnings) != 0 {
				t.Fatalf("warnings = %v, want the scaffold to load cleanly", set.Warnings)
			}
		})
	}
}

// A binding is a set of entries, so every spelling and every ordering of one
// binding names one file, and scaffolding it again is refused as the conflict
// it is rather than written as a second definition firing beside the first.
func TestWorkflowsEditScaffoldsOneFilePerBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ first, again []string }{
		{[]string{"--when", "enter in_progress"}, []string{"--when", "Enter In-Progress"}},
		{[]string{"--when", "enter closed", "--when", "show_ticket"}, []string{"--when", "show_ticket", "--when", "enter closed"}},
		{[]string{"--label", "b", "--label", "a"}, []string{"--label", "a", "--label", "b"}},
	} {
		t.Run(strings.Join(tc.again, " "), func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t)
			if _, err := runLit(t, root, append([]string{"workflows", "edit"}, tc.first...)...); err != nil {
				t.Fatalf("Run(workflows edit %v) error = %v", tc.first, err)
			}
			_, err := runLit(t, root, append([]string{"workflows", "edit"}, tc.again...)...)
			var conflict MergeConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("Run(workflows edit %v) error = %v (%T), want MergeConflictError", tc.again, err, err)
			}
			if entries, readErr := os.ReadDir(filepath.Join(root, ".lit", "workflows")); readErr != nil || len(entries) != 1 {
				t.Fatalf("ReadDir(.lit/workflows) = %v, %v; want the one file the first scaffold wrote", entries, readErr)
			}
		})
	}
}

// A new definition's file name is its id, so a name a loaded definition
// already holds is refused, naming that definition: written, the file would
// silently replace a shipped default (`--label done` and the post-close
// reminder) or lose to a project file declaring that id.
func TestWorkflowsEditRefusesAnIDALoadedDefinitionHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args    []string
		project map[string]string
		holder  string
	}{
		{[]string{"--label", "done"}, nil, "embedded definition done.md"},
		{[]string{"--label", "Phase-Impl"}, nil, "embedded definition phase-impl.md"},
		{[]string{"--when", "enter closed"}, map[string]string{"notes.md": "---\nid: enter-closed\nwhen: [show_ticket]\n---\nnotes"}, "project definition notes.md"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			root := tempRepo(t)
			for rel, content := range tc.project {
				writeCLIWorkflow(t, root, rel, content)
			}
			_, err := runLit(t, root, append([]string{"workflows", "edit"}, tc.args...)...)
			var conflict MergeConflictError
			if !errors.As(err, &conflict) || !strings.Contains(err.Error(), tc.holder+" already holds that id") {
				t.Fatalf("Run(workflows edit %v) error = %v (%T), want a MergeConflictError naming the %s", tc.args, err, err, tc.holder)
			}
			entries, _ := os.ReadDir(filepath.Join(root, ".lit", "workflows"))
			if len(entries) != len(tc.project) {
				t.Fatalf("ReadDir(.lit/workflows) = %v, want nothing scaffolded", entries)
			}
		})
	}
}

// A --when naming no moment lit fires, or a --label no ticket can carry, is
// refused, and nothing is scaffolded for it: a file bound to it could never
// fire.
func TestWorkflowsEditRefusesWhatNoKeyReads(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--when", "enter reveiw"},
		{"--when", "work_finished"},
		{"--when", "exit in progress"},
		{"--when", "enter closed", "--label", "a,b"},
		{"--label", "  "},
	} {
		root := tempRepo(t)
		_, err := runLit(t, root, append([]string{"workflows", "edit"}, args...)...)
		var validation model.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("Run(workflows edit %v) error = %v (%T), want a ValidationError", args, err, err)
		}
		if _, statErr := os.Stat(filepath.Join(root, ".lit", "workflows")); !os.IsNotExist(statErr) {
			t.Fatalf("Stat(.lit/workflows) error = %v, want nothing scaffolded for %v", statErr, args)
		}
	}
}

// A positional only ever names a definition: one nothing loaded holds is
// refused, naming the flags that scaffold a new one, and nothing is written.
func TestWorkflowsEditRefusesAnIDNothingHolds(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	_, err := runLit(t, root, "workflows", "edit", "enter closed")
	var validation model.ValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), `no workflow definition with id "enter closed"`) || !strings.Contains(err.Error(), "--when") || !strings.Contains(err.Error(), "--label") {
		t.Fatalf("Run(workflows edit \"enter closed\") error = %v (%T), want a ValidationError naming --when and --label", err, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".lit", "workflows")); !os.IsNotExist(statErr) {
		t.Fatalf("Stat(.lit/workflows) error = %v, want nothing scaffolded", statErr)
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

	_, err := runLit(t, root, "workflows", "edit", "--label", "blocked")
	var conflict MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Run(workflows edit --label blocked) error = %v (%T), want MergeConflictError", err, err)
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
		"occasion: when=enter closed labels=needs-design issue=lit-7\n",
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
// entry into a state, an exit from one — and reads each the way the
// frontmatter does, so a definition fires in a dry-run exactly when it says
// it fires.
func TestWorkflowsDryRunReadsEveryWhenSpelling(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	for id, when := range map[string]string{
		"on-show":     "show_ticket",
		"on-exit":     "exit plan",
		"on-enter":    "enter review",
		"on-progress": "enter in_progress",
	} {
		writeCLIWorkflow(t, root, id+".md", "---\nid: "+id+"\nwhen: ["+when+"]\n---\n"+id+" body")
	}

	for _, tc := range []struct {
		when       []string
		occasion   string
		firedLines []string
	}{
		{[]string{"show_ticket"}, "when=show_ticket", []string{"on-show (project)  [when:show_ticket]"}},
		{[]string{"exit plan"}, "when=exit plan", []string{"on-exit (project)  [when:exit plan]"}},
		{[]string{"Enter  Review"}, "when=enter review", []string{
			`phase-review "Record when the merge is under way" (embedded)  [when:enter review]`,
			"on-enter (project)  [when:enter review]",
		}},
		{[]string{"exit open", "enter in-progress", "enter plan"}, "when=exit open,enter in_progress,enter plan", []string{
			`phase-plan "Plan the ticket, then record when the code starts" (embedded)  [when:enter plan]`,
			"on-progress (project)  [when:enter in_progress]",
		}},
		{nil, "when=-", nil},
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

// dry-run refuses a --when naming no moment a command fires — an event
// outside the catalog, the retired transition events among them, a state the
// lifecycle lacks, a blank — rather than explain a match on something that
// cannot happen, and names what can.
func TestWorkflowsDryRunRefusesAMomentLitNeverFires(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	for when, says := range map[string][]string{
		"work_finished":      {"enter <state> or exit <state>", "show_ticket"},
		"ticket_closed":      {"enter <state> or exit <state>", "show_ticket"},
		"warp_drive_engaged": {"enter <state> or exit <state>", "show_ticket"},
		"enter":              {"enter <state> or exit <state>", "show_ticket"},
		"enter verify":       {"open, in_progress, plan, impl, review, merge, closed, live, archived, deleted"},
		"  ":                 {"blank"},
	} {
		_, err := runLit(t, root, "workflows", "dry-run", "--when", when)
		var validation model.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("dry-run --when %q error = %v (%T), want a ValidationError", when, err, err)
		}
		for _, want := range append([]string{"--when " + strconv.Quote(when)}, says...) {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("dry-run --when %q error = %q, want it to name %s", when, err, want)
			}
		}
	}
}

// dry-run reads --label the way a ticket's label is stored, so a label asked
// in any case matches the definition bound to it, and refuses one no ticket
// can carry.
func TestWorkflowsDryRunReadsLabelsAsATicketCarriesThem(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "design.md", "---\nid: design-note\nlabels: [needs-design]\n---\nbody")

	output, err := runLit(t, root, "workflows", "dry-run", "--label", " Needs-Design ")
	if err != nil {
		t.Fatalf("Run(workflows dry-run --label) error = %v", err)
	}
	for _, want := range []string{"occasion: when=- labels=needs-design issue=-\n", "design-note (project)  [label:needs-design]\n"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output = %q, want it to contain %q", output, want)
		}
	}
	for _, label := range []string{"a,b", "  "} {
		_, err := runLit(t, root, "workflows", "dry-run", "--label", label)
		var validation model.ValidationError
		if !errors.As(err, &validation) || !strings.Contains(err.Error(), "--label "+strconv.Quote(label)) {
			t.Errorf("dry-run --label %q error = %v (%T), want a ValidationError naming it", label, err, err)
		}
	}
}

// TestWorkflowsEditRejectsOversuppliedArguments guards the same silent-
// swallow shape TestWorkflowsRejectsOversuppliedArguments pins for `show`, and
// edit's own: an id and keys at once, or neither.
func TestWorkflowsEditRejectsOversuppliedArguments(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)

	for _, args := range [][]string{
		{"done", "extra"},
		{"done", "--when", "enter closed"},
		{},
	} {
		_, err := runLit(t, root, append([]string{"workflows", "edit"}, args...)...)
		if _, ok := err.(UsageError); !ok {
			t.Fatalf("Run(workflows edit %v) error = %v (%T), want UsageError", args, err, err)
		}
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

// dry-run reads repeated flags as a definition reads a repeated entry: once.
func TestWorkflowsDryRunReadsARepeatedFlagOnce(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	output, err := runLit(t, root, "workflows", "dry-run", "--label", "Foo", "--label", "foo", "--when", "show_ticket", "--when", "SHOW_TICKET")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	if want := "occasion: when=show_ticket labels=foo issue=-\n"; !strings.Contains(output, want) {
		t.Fatalf("dry-run output = %q, want %q", output, want)
	}
}

// dry-run loads the definitions as a firing command does, so a file that can
// never fire is named on stderr beside the explanation of what does.
func TestWorkflowsDryRunNamesWhatCanNeverFire(t *testing.T) {
	t.Parallel()
	root := tempRepo(t)
	writeCLIWorkflow(t, root, "done.md", "---\nevents: [work_finished]\n---\nold done")

	stdout, stderr, err := runCLIInDirSplit(t, root, "workflows", "dry-run", "--when", "enter closed")
	if err != nil {
		t.Fatalf("Run(workflows dry-run) error = %v", err)
	}
	if !strings.Contains(stdout, "Fired (0)\n") {
		t.Fatalf("dry-run stdout = %q, want nothing fired: the project done.md holds the id and never fires", stdout)
	}
	if want := `lit: workflow project done.md: this lit does not read "events"`; !strings.Contains(stderr, want) {
		t.Fatalf("dry-run stderr = %q, want %q", stderr, want)
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
// embedded default bound to entering it, then the retentions — after the
// event catalog, in the spelling a `when:` entry uses.
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
	for _, state := range []string{"open", "in_progress", "plan", "impl", "review", "merge", "closed", "live", "archived", "deleted"} {
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
