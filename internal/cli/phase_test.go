package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workflows"
)

// prefixed is the facts under one namespace.
func prefixed(facts map[string]any, prefix string) map[string]any {
	out := map[string]any{}
	for key, value := range facts {
		if strings.HasPrefix(key, prefix) {
			out[key] = value
		}
	}
	return out
}

// phaseFacts is what the last invocation's event says its phase move did.
func phaseFacts(t *testing.T) map[string]any {
	t.Helper()
	return prefixed(writtenFacts(t, lastEvent(t)), "phase.")
}

// workflowFacts is what the last invocation's event says about the workflow
// occasion it fired.
func workflowFacts(t *testing.T) map[string]any {
	t.Helper()
	return prefixed(writtenFacts(t, lastEvent(t)), "workflows.")
}

// bulkItemWorkflowFacts is what the last bulk run's one item says about the
// workflow occasion its transition fired.
func bulkItemWorkflowFacts(t *testing.T) map[string]any {
	t.Helper()
	items := writtenEvent(t, lastEvent(t)).Occurrences["bulk.items"]
	if len(items) != 1 {
		t.Fatalf("bulk.items = %v, want one item", items)
	}
	return prefixed(items[0].(map[string]any)["bulk.item.facts"].(map[string]any), "workflows.")
}

// A definition bound to `enter <state>` or `exit <state>` fires on every
// transition that makes that crossing, through every command that makes one,
// single or bulk, and on nothing else: lit start exits open and enters plan
// inside in_progress, a phase move exits one phase and enters the next
// without leaving in_progress, a move to the phase the ticket is in crosses
// nothing, lit done and lit close both enter closed — so both print the
// shipped done guidance — and lit open exits it. The command's event names the
// crossings and what fired on them, the shipped defaults among them.
func TestEveryCrossingFiresOnTheTransitionThatMakesIt(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	var bound []string
	for _, state := range workflows.States() {
		for _, side := range []string{"enter", "exit"} {
			id := side + "-" + state
			bound = append(bound, id)
			writeCLIWorkflow(t, dir, id+".md", "---\nwhen: ["+side+" "+state+"]\n---\nFIRED "+id+" on <id>")
		}
	}
	for _, step := range []struct {
		args   []string
		ticket string
		facts  func(*testing.T) map[string]any
		when   []any
		fired  []any
	}{
		{[]string{"start", first, "--by", "agent"}, first, workflowFacts,
			[]any{"exit open", "enter in_progress", "enter plan"}, []any{"enter-in_progress", "enter-plan", "exit-open", "phase-plan"}},
		{[]string{"phase", first, "--to", "impl"}, first, workflowFacts,
			[]any{"exit plan", "enter impl"}, []any{"enter-impl", "exit-plan", "phase-impl"}},
		{[]string{"phase", first, "--to", "review"}, first, workflowFacts,
			[]any{"exit impl", "enter review"}, []any{"enter-review", "exit-impl", "phase-review"}},
		{[]string{"phase", first, "--to", "merge"}, first, workflowFacts,
			[]any{"exit review", "enter merge"}, []any{"enter-merge", "exit-review", "phase-merge"}},
		{[]string{"phase", first, "--to", "merge"}, first, workflowFacts,
			[]any{}, []any{}},
		{[]string{"done", first}, first, workflowFacts,
			[]any{"exit in_progress", "exit merge", "enter closed"}, []any{"done", "enter-closed", "exit-in_progress", "exit-merge"}},
		{[]string{"open", first}, first, workflowFacts,
			[]any{"exit closed", "enter open"}, []any{"enter-open", "exit-closed"}},
		{[]string{"close", first, "--resolution", "wontfix"}, first, workflowFacts,
			[]any{"exit open", "enter closed"}, []any{"done", "enter-closed", "exit-open"}},
		{[]string{"bulk", "start", "--ids", second, "--by", "agent"}, second, bulkItemWorkflowFacts,
			[]any{"exit open", "enter in_progress", "enter plan"}, []any{"enter-in_progress", "enter-plan", "exit-open", "phase-plan"}},
		{[]string{"bulk", "done", "--ids", second}, second, bulkItemWorkflowFacts,
			[]any{"exit in_progress", "exit plan", "enter closed"}, []any{"done", "enter-closed", "exit-in_progress", "exit-plan"}},
	} {
		out := runCLIInDir(t, dir, step.args...)
		for _, id := range bound {
			body := "FIRED " + id + " on " + step.ticket
			if want := slices.Contains(step.fired, any(id)); strings.Contains(out, body) != want {
				t.Errorf("lit %v printed %q; want %q printed: %v", step.args, out, body, want)
			}
		}
		// Entering closed prints the shipped done default's post-close
		// capture reminder, whichever command closed the ticket.
		if slices.Contains(step.fired, any("done")) {
			t.Logf("lit %v printed:\n%s", step.args, out)
			if body := "Ticket " + step.ticket + " has been closed. Before moving on"; !strings.Contains(out, body) {
				t.Errorf("lit %v printed %q; want the done guidance %q", step.args, out, body)
			}
		}
		// The load's warnings are the command's, said once however many
		// tickets it ran over; a bulk item carries only what it fired.
		if got := workflowFacts(t)["workflows.warnings"]; !reflect.DeepEqual(got, []any{}) {
			t.Errorf("lit %v workflows.warnings = %v, want none", step.args, got)
		}
		want := map[string]any{"workflows.when": step.when, "workflows.fired": step.fired}
		got := step.facts(t)
		if step.args[0] != "bulk" {
			delete(got, "workflows.warnings")
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}

// A definition written for the lit that named transitions — an `events:` or
// `states:` key, or one of the transition events listed under `when:` — never
// fires, and every command it would have fired on says so on stderr and on its
// event, naming the spelling this lit reads; the command still succeeds.
func TestDefinitionsInTheRetiredSpellingsWarnAndNeverFire(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "start", first, "--by", "agent")
	for name, activation := range map[string]string{
		"old-events": "events: [work_finished]",
		"old-states": "states: [closed]",
		"old-when":   "when: [work_finished]",
	} {
		writeCLIWorkflow(t, dir, name+".md", "---\n"+activation+"\n---\nOLD "+name+" fired")
	}

	stdout, stderr, err := runCLIInDirSplit(t, dir, "done", first)
	if err != nil {
		t.Fatalf("lit done error = %v, want it to succeed past the unreadable definitions; stderr %q", err, stderr)
	}
	if strings.Contains(stdout, "OLD ") {
		t.Errorf("lit done printed %q; want no retired-spelling definition to fire", stdout)
	}
	if !strings.Contains(stdout, "has been closed") {
		t.Errorf("lit done printed %q; want the shipped done guidance beside the warnings", stdout)
	}
	warnings := workflowFacts(t)["workflows.warnings"].([]any)
	if len(warnings) != 3 {
		t.Fatalf("workflows.warnings = %v, want one per retired-spelling file", warnings)
	}
	for _, warning := range warnings {
		line := "lit: workflow " + warning.(string) + "\n"
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr = %q, want the warning line %q", stderr, line)
		}
		if !strings.Contains(warning.(string), "when: [enter closed]") {
			t.Errorf("warning %q does not name the spelling this lit reads", warning)
		}
	}
}

// The workflows lit ships tell the agent, as the work enters each phase, the
// command that records the next move: entering every phase but the last
// prints the `lit phase` that moves it on, and entering the last prints the
// `lit done` that ends it.
func TestShippedWorkflowsNameEachPhaseMove(t *testing.T) {
	t.Parallel()
	dir, first, _ := statuslineCheckout(t)
	phases := model.Phases()
	for i, phase := range phases {
		args := []string{"phase", first, "--to", string(phase)}
		if phase == model.PhasePlan {
			args = []string{"start", first, "--by", "agent"}
		}
		out := runCLIInDir(t, dir, args...)
		if want := phaseMoveAfter(phases, i, first); !strings.Contains(out, want) {
			t.Errorf("lit %v printed %q; want it to name %s", args, out, want)
		}
	}
}

// Every text that teaches an agent to work a ticket names each move after
// lit start with the command that records it.
func TestAgentGuidanceNamesEveryPhaseMove(t *testing.T) {
	t.Parallel()
	texts := map[string]string{}
	for _, name := range []string{"quickstart-work.md"} {
		raw, err := templates.EmbeddedDefault(name)
		if err != nil {
			t.Fatalf("EmbeddedDefault(%s) error = %v", name, err)
		}
		texts[name] = string(raw)
	}
	skill, err := os.ReadFile(filepath.Join(mustRepoRoot(t), filepath.FromSlash(nextSkillPath)))
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", nextSkillPath, err)
	}
	texts[nextSkillPath] = string(skill)

	phases := model.Phases()
	for name, text := range texts {
		for i := range phases {
			if want := phaseMoveAfter(phases, i, "<id>"); !strings.Contains(text, want) {
				t.Errorf("%s does not name %s", name, want)
			}
		}
	}
}

// phaseMoveAfter is the command that records the move out of phases[i] for
// ticket id, quoted as agent guidance writes it.
func phaseMoveAfter(phases []model.Phase, i int, id string) string {
	if i == len(phases)-1 {
		return "`lit done " + id + "`"
	}
	return "`lit phase " + id + " --to " + string(phases[i+1]) + "`"
}

// lit phase moves an in-progress ticket's phase: the event names the phase
// left, the phase entered and whether anything changed; lit show and
// `--field phase` read the phase back; a move to the phase the ticket is in
// records nothing; a ticket that is not in progress refuses the move; and the
// statusline file keeps naming the ticket being worked on.
func TestPhaseMovesAnInProgressTicket(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	runCLIInDir(t, dir, "start", first, "--by", "agent")

	for _, step := range []struct {
		to      string
		from    string
		changed bool
		notice  string
	}{
		{"impl", "plan", true, "phase: plan → impl\n"},
		{"impl", "impl", false, "phase: impl (unchanged)\n"},
		{"review", "impl", true, "phase: impl → review\n"},
		{"impl", "review", true, "phase: review → impl\n"},
	} {
		if out := runCLIInDir(t, dir, "phase", first, "--to", step.to); !strings.HasPrefix(out, step.notice) {
			t.Fatalf("lit phase --to %s printed %q, want it to open with the line %q", step.to, out, step.notice)
		}
		// A move that records nothing crosses no hold, so the gate reads no lane
		// for it.
		want := map[string]any{"phase.from": step.from, "phase.to": step.to, "phase.changed": step.changed}
		if step.changed {
			want["phase.lane_family_issues"], want["phase.lane_held"] = float64(2), true
		}
		if got := phaseFacts(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("lit phase --to %s event facts = %v, want %v", step.to, got, want)
		}
		if got := runCLIInDir(t, dir, "show", first, "--field", "phase"); got != step.to+"\n" {
			t.Fatalf("show --field phase after --to %s = %q, want %q", step.to, got, step.to+"\n")
		}
	}
	if show := runCLIInDir(t, dir, "show", first); !strings.Contains(show, "\nstatus: in_progress\nphase: impl\n") {
		t.Errorf("lit show does not print the phase under the status:\n%s", show)
	}
	history := runCLIInDir(t, dir, "history", first)
	if got := strings.Count(history, "phase: plan → impl"); got != 1 {
		t.Errorf("history records plan → impl %d times, want once (the repeated move records nothing):\n%s", got, history)
	}
	if got := workingOnID(readStatusline(t, dir)); got != first {
		t.Errorf("statusline working_on = %q after phase moves, want %q", got, first)
	}

	out, err := runCLIInDirAllowError(t, dir, "phase", second, "--to", "impl")
	if err == nil || !strings.Contains(out+err.Error(), "only an in-progress ticket has a phase") {
		t.Errorf("lit phase on an open ticket = %q, %v; want the in-progress refusal", out, err)
	}
	if got := runCLIInDir(t, dir, "show", second, "--field", "phase"); got != "\n" {
		t.Errorf("show --field phase on an open ticket = %q, want an empty value", got)
	}
}

// A mis-written `lit phase` is a usage error whatever the id names, and every
// usage line it prints is the command's one line, naming the required --to.
func TestPhaseUsageNamesTheRequiredPhase(t *testing.T) {
	t.Parallel()
	dir, _, _ := statuslineCheckout(t)
	const usage = "usage: lit phase <id> --to <plan|impl|review|merge> [--take] [--reason <text>]"
	for _, args := range [][]string{
		{"phase"},
		{"phase", "no-such-id"},
		{"phase", "no-such-id", "--to", "shipping"},
	} {
		out, err := runCLIInDirAllowError(t, dir, args...)
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("lit %v exit = %d (%v), want %d, a usage error", args, got, err, ExitUsage)
		}
		if !strings.Contains(out+err.Error(), usage) {
			t.Errorf("lit %v = %q, %v; want the usage line %q", args, out, err, usage)
		}
	}
}
