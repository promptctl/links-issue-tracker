package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The retry advice that must never appear on a container refusal. Both halves
// are terminal: an epic's state derives from its children, so nothing the agent
// runs again can move either one. Matched as the two fragments the default arm
// emits rather than the whole sentence, so a reworded default still fails here.
const (
	retryAdvice  = "Retry the command"
	doctorAdvice = "run `lit doctor`"
)

// epicFixture builds an epic with childCount children and closes closedCount of
// them, returning the epic's id. It drives the real store, so the epic's state
// is the derived one production computes rather than a planted value.
func (h readyTestHarness) epicFixture(childCount, closedCount int) string {
	h.t.Helper()
	epic := h.createIssue(storage.CreateIssueInput{
		Prefix: "test", Title: "An epic", Topic: "epics", IssueType: "epic",
	})
	for i := 0; i < childCount; i++ {
		child := h.createIssue(storage.CreateIssueInput{
			Prefix: "test", Title: "A child", Topic: "epics", IssueType: "task", ParentID: epic.ID,
		})
		if i < closedCount {
			h.applyAction(child.ID, model.Done{}, "")
		}
	}
	return epic.ID
}

// runTransitionErr drives the real command — the same runTransition `lit done`
// and `lit start` enter through — and hands back its error. Driving the command
// rather than calling commandErrorReason on a hand-built value is the point:
// the error has to travel from the raise site through Apply to the sink, so a
// raise site that goes back to an untyped fmt.Errorf fails these tests.
func (h readyTestHarness) runTransitionErr(issueID string, spec transitionSpec) error {
	h.t.Helper()
	return runTransition(h.ctx, io.Discard, h.ap, []string{issueID}, spec)
}

// renderCommandError is what the agent actually reads on stderr: the message
// and the remediation, through the same sink main() uses.
func renderCommandError(t *testing.T, err error) string {
	t.Helper()
	var stderr bytes.Buffer
	WriteCommandError(&stderr, err)
	return stderr.String()
}

// `lit done` on an epic whose children are all closed asked for a state the
// workspace is already in. The condition is terminal — an epic's state derives
// from its children, so retrying cannot change it and `lit doctor` has nothing
// to diagnose — and it used to exit 1 carrying the default's "Retry the
// command", which is an instruction to loop aimed squarely at an unattended
// agent (links-cli-errors-1u9g).
func TestDoneOnAClosedEpicIsTerminalAndSaysNoActionIsNeeded(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.epicFixture(2, 2)

	err := h.runTransitionErr(epic, doneSpec)
	if err == nil {
		t.Fatalf("done on a fully-closed epic error = nil, want the already-in-state answer")
	}
	if got, want := commandErrorReason(err), "state_already_holds"; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
	if got, want := ExitCode(err), ExitNoWork; got != want {
		t.Errorf("exit = %d, want %d (ran correctly, changed nothing)", got, want)
	}
	rendered := renderCommandError(t, err)
	for _, forbidden := range []string{retryAdvice, doctorAdvice} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("rendered error contains %q, which tells an agent to loop on a condition that cannot change:\n%s", forbidden, rendered)
		}
	}
	// The message has to say the epic is already closed and that the requested
	// action has nothing to do — the two facts the ticket names as done.
	for _, want := range []string{"already closed", "`done`", "nothing to do"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered error missing %q:\n%s", want, rendered)
		}
	}
}

// The other half of the same epic: `start` asks for a state the children do NOT
// establish, so it is a refusal rather than a satisfied request. Both used to
// print one byte-identical sentence that never mentioned which action was
// asked for — one message serving two opposite facts, the same collapse
// links-cli-q7hg removed from `lit next`'s empty answers. Nothing here asserts
// the prose beyond that: what is pinned is that the two answers are TELLABLE
// APART, in all three of message, reason, and exit code.
func TestStartAndDoneOnOneClosedEpicDoNotShareOneAnswer(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.epicFixture(2, 2)

	doneErr := h.runTransitionErr(epic, doneSpec)
	startErr := h.runTransitionErr(epic, startSpec)
	if doneErr == nil || startErr == nil {
		t.Fatalf("done error = %v, start error = %v, want both non-nil", doneErr, startErr)
	}
	if doneErr.Error() == startErr.Error() {
		t.Errorf("done and start on a closed epic share one message, so an agent cannot tell a satisfied request from a refusal:\n%s", doneErr.Error())
	}
	if commandErrorReason(doneErr) == commandErrorReason(startErr) {
		t.Errorf("done and start share reason %q", commandErrorReason(doneErr))
	}
	if ExitCode(doneErr) == ExitCode(startErr) {
		t.Errorf("done and start share exit code %d", ExitCode(doneErr))
	}
	// The refusal names the action it refused, which the old count-only wording
	// ("0 of its 1 children are not done") never did.
	if !strings.Contains(startErr.Error(), "`start`") {
		t.Errorf("start refusal does not name the action it refused: %s", startErr.Error())
	}
}

// Every container refusal is terminal, not only the already-closed one, so no
// shape of epic and no status action may reach the default's retry advice.
// This is the enumeration guard: it sweeps the actions `lit` exposes across the
// three epic shapes, so a new action or a new arm that forgets the mapping
// fails here rather than in front of an agent.
func TestNoContainerRefusalAdvisesRetrying(t *testing.T) {
	shapes := []struct {
		name                  string
		children, closedCount int
	}{
		{"childless epic", 0, 0},
		{"epic with unfinished children", 2, 1},
		{"fully closed epic", 2, 2},
	}
	specs := []struct {
		name string
		spec transitionSpec
	}{
		{"done", doneSpec},
		{"start", startSpec},
		{"open", openSpec},
	}
	for _, shape := range shapes {
		for _, spec := range specs {
			t.Run(shape.name+"/"+spec.name, func(t *testing.T) {
				h := newReadyTestHarness(t)
				epic := h.epicFixture(shape.children, shape.closedCount)

				err := h.runTransitionErr(epic, spec.spec)
				if err == nil {
					t.Fatalf("%s on a %s error = nil, want a container answer", spec.name, shape.name)
				}
				rendered := renderCommandError(t, err)
				for _, forbidden := range []string{retryAdvice, doctorAdvice} {
					if strings.Contains(rendered, forbidden) {
						t.Errorf("%s on a %s advises %q:\n%s", spec.name, shape.name, forbidden, rendered)
					}
				}
				if got := ExitCode(err); got == ExitGeneric {
					t.Errorf("%s on a %s exits %d (the unclassified-fault code), want a classified terminal code", spec.name, shape.name, got)
				}
			})
		}
	}
}

// Satisfied is the one comparison both the message and the CLI mappings read,
// so it is pinned directly: the requested target against the state the children
// establish, never a re-derivation from the progress counts. A closed epic
// satisfies `done` and refuses `start` — the distinction the type exists to
// carry.
func TestContainerActionErrorSatisfiedComparesTargetToDerivedState(t *testing.T) {
	closed := model.ContainerActionError{
		ID: "test-epics-abcd", Action: model.ActionDone,
		Target: model.StateClosed, State: model.StateClosed,
		Progress: model.Progress{Total: 2, Closed: 2},
	}
	if !closed.Satisfied() {
		t.Errorf("done on a closed epic: Satisfied() = false, want true")
	}
	refused := closed
	refused.Action, refused.Target = model.ActionStart, model.StateInProgress
	if refused.Satisfied() {
		t.Errorf("start on a closed epic: Satisfied() = true, want false")
	}
}
