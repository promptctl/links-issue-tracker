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
)

// phaseFacts is what the last invocation's event says its phase move did.
func phaseFacts(t *testing.T) map[string]any {
	t.Helper()
	facts := map[string]any{}
	for key, value := range writtenFacts(t, lastEvent(t)) {
		if strings.HasPrefix(key, "phase.") {
			facts[key] = value
		}
	}
	return facts
}

// workflowFacts is what the last invocation's event says about the workflow
// occasion it fired.
func workflowFacts(t *testing.T) map[string]any {
	t.Helper()
	facts := map[string]any{}
	for key, value := range writtenFacts(t, lastEvent(t)) {
		if strings.HasPrefix(key, "workflows.") {
			facts[key] = value
		}
	}
	return facts
}

// Workflow definitions bound to a phase fire as the work moves through it:
// lit start enters plan inside in_progress, a phase move exits one phase and
// enters the next without leaving in_progress, a move to the phase the ticket
// is in exits and enters nothing, and lit done exits the phase along with
// in_progress.
// The command's event names the occasion and what fired on it.
func TestPhaseMovesFireStateBoundWorkflows(t *testing.T) {
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	bound := map[string]string{
		"enter-plan":   "states: [plan]",
		"enter-review": "states: [review]",
		"exit-impl":    "states: [{name: impl, when: exit}]",
		"exit-review":  "states: [{name: review, when: exit}]",
		"working":      "states: [in_progress]",
	}
	for id, activation := range bound {
		writeCLIWorkflow(t, dir, id+".md", "---\n"+activation+"\n---\nFIRED "+id+" on <id>")
	}
	approvePlanInDir(t, dir, first)

	for _, step := range []struct {
		args    []string
		event   string
		exited  []any
		entered []any
		fired   []any
	}{
		{[]string{"start", first, "--by", "agent"}, "work_started", []any{"open"}, []any{"in_progress", "plan"}, []any{"enter-plan", "phase-plan", "working"}},
		{[]string{"phase", first, "--to", "impl"}, "phase_set", []any{"plan"}, []any{"impl"}, []any{"phase-impl"}},
		{[]string{"phase", first, "--to", "review"}, "phase_set", []any{"impl"}, []any{"review"}, []any{"enter-review", "exit-impl", "phase-review"}},
		{[]string{"phase", first, "--to", "review"}, "phase_set", []any{}, []any{}, []any{}},
		{[]string{"done", first}, "work_finished", []any{"in_progress", "review"}, []any{"closed"}, []any{"done", "exit-review"}},
	} {
		out := runCLIInDir(t, dir, step.args...)
		for id := range bound {
			body := "FIRED " + id + " on " + first
			if want := slices.Contains(step.fired, any(id)); strings.Contains(out, body) != want {
				t.Errorf("lit %v printed %q; want %q printed: %v", step.args, out, body, want)
			}
		}
		want := map[string]any{"workflows.event": step.event, "workflows.exited": step.exited, "workflows.entered": step.entered, "workflows.fired": step.fired}
		if got := workflowFacts(t); !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}

// The workflows lit ships tell the agent, as the work enters each phase, the
// command that records the next move: entering every phase but the last
// prints the `lit phase` that moves it on, and entering the last prints the
// `lit done` that ends it.
func TestShippedWorkflowsNameEachPhaseMove(t *testing.T) {
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
	texts := map[string]string{}
	for _, name := range []string{"quickstart-work.md", "agents-section.md"} {
		raw, err := templates.EmbeddedDefault(name)
		if err != nil {
			t.Fatalf("EmbeddedDefault(%s) error = %v", name, err)
		}
		texts[name] = string(raw)
	}
	skill, err := os.ReadFile(filepath.Join(mustRepoRoot(t), "claude-plugin", "skills", "next", "SKILL.md"))
	if err != nil {
		t.Fatalf("ReadFile(SKILL.md) error = %v", err)
	}
	texts["claude-plugin/skills/next/SKILL.md"] = string(skill)

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
		want := map[string]any{"phase.from": step.from, "phase.to": step.to, "phase.changed": step.changed}
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
	dir, _, _ := statuslineCheckout(t)
	const usage = "usage: lit phase <id> --to <plan|impl|review|merge> [--reason <text>]"
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
