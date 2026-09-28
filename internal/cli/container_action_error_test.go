package cli

import (
	"bytes"
	"context"
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

// The two answers a container rejection can be. They are named here so the grid
// below reads as the truth table it is, with the literal strings the CLI emits
// declared once beside each other.
const (
	reasonSatisfied = "state_already_holds"
	reasonRefused   = "validation_refused"
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

// runTransitionErr drives the real command — the same runTransition every
// status verb enters through — and hands back its error. Driving the command
// rather than calling commandErrorReason on a hand-built value is the point:
// the error has to travel from the raise site through Apply to the sink, so a
// raise site that goes back to an untyped fmt.Errorf fails these tests.
func (h readyTestHarness) runTransitionErr(issueID string, spec transitionSpec, extraArgs ...string) error {
	h.t.Helper()
	return runTransition(h.ctx, io.Discard, h.ap, append([]string{issueID}, extraArgs...), spec)
}

// The flags an action needs at its own command boundary before the call can
// reach the container rejection deeper in. `close` is the only status action
// that requires one, and supplying it is what makes its cells test the
// rejection rather than the resolution parse. Keyed by action so the grid below
// stays a table of answers rather than of invocation detail.
var actionFlags = map[string][]string{
	"close": {"--resolution", "wontfix"},
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
// to diagnose — and the default's "Retry the command" would be an instruction
// to loop aimed squarely at an unattended agent.
func TestDoneOnAClosedEpicIsTerminalAndSaysNoActionIsNeeded(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.epicFixture(2, 2)

	err := h.runTransitionErr(epic, doneSpec)
	if err == nil {
		t.Fatalf("done on a fully-closed epic error = nil, want the already-in-state answer")
	}
	// The reason, the exit code and the absent retry advice are this cell's row
	// in TestEveryContainerRejectionCellHasItsOwnReasonAndExit, which owns them
	// for all sixteen cells. What is left here is the wording only this cell has:
	// the epic's own state and the action that asked for it.
	rendered := renderCommandError(t, err)
	for _, want := range []string{"already closed", "`done`", "nothing to do"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered error missing %q:\n%s", want, rendered)
		}
	}
}

// The other half of the same epic: `start` asks for a state the children do NOT
// establish, so it is a refusal rather than a satisfied request. What is pinned
// is that the two answers are TELLABLE APART, in all three of message, reason,
// and exit code.
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
	// The refusal names the action it refused.
	if !strings.Contains(startErr.Error(), "`start`") {
		t.Errorf("start refusal does not name the action it refused: %s", startErr.Error())
	}
}

// The prose each answer owes the agent, keyed by the reason the cell DECLARES
// rather than by the one the code produced. Both answers are terminal, so
// neither may carry the default's retry advice. Only a satisfied request may
// say the action has nothing to do: a refusal saying it is the answer-shaped
// void this grid exists to catch — the sentence that tells the agent with the
// most work left that there is none. [LAW:dataflow-not-control-flow] the cell's
// declared reason selects the phrases as a value, so the loop below runs one
// set of assertions for every cell rather than branching per answer.
var renderedPhrases = map[string]struct{ want, forbidden []string }{
	reasonSatisfied: {
		want:      []string{"nothing to do"},
		forbidden: []string{retryAdvice, doctorAdvice},
	},
	reasonRefused: {
		want:      []string{"cannot `"},
		forbidden: []string{retryAdvice, doctorAdvice, "nothing to do"},
	},
}

// The whole grid of container rejections — every epic shape crossed with every
// status action `lit` exposes — with the reason and exit code each cell must
// produce written out one by one. The four actions are the four `Target()`s:
// archive, unarchive, delete and restore are retention actions and never reach
// this rejection, so adding one of those does not belong here, while adding a
// fifth status action leaves a visibly short grid.
//
// The wants are written out, and not derived from Satisfied(), because
// Satisfied() is the thing under test: a table that computed its wants would
// agree with a wrong predicate exactly as readily as a right one.
// [LAW:verifiable-goals] a check that passes under the defect is not a check.
//
// Exactly two of the sixteen cells are satisfied requests, and they are the two
// actions targeting Closed on the one shape that has nothing left to do. The
// three near-misses are the defect's shape: a part-done epic already derives
// in_progress, so `start` matches its own target with every child still to do,
// and a childless or not-yet-started epic derives open, so `open` matches there
// too. None of the three may answer "nothing to do" at ExitNoWork — the code
// that exists so a caller can stop WITHOUT reading the message.
func TestEveryContainerRejectionCellHasItsOwnReasonAndExit(t *testing.T) {
	cells := []struct {
		shape            string
		children, closed int
		action           string
		spec             transitionSpec
		wantReason       string
		wantExit         int
	}{
		{"childless", 0, 0, "done", doneSpec, reasonRefused, ExitValidation},
		{"childless", 0, 0, "close", closeSpec, reasonRefused, ExitValidation},
		{"childless", 0, 0, "start", startSpec, reasonRefused, ExitValidation},
		{"childless", 0, 0, "open", openSpec, reasonRefused, ExitValidation},

		{"no child started", 2, 0, "done", doneSpec, reasonRefused, ExitValidation},
		{"no child started", 2, 0, "close", closeSpec, reasonRefused, ExitValidation},
		{"no child started", 2, 0, "start", startSpec, reasonRefused, ExitValidation},
		{"no child started", 2, 0, "open", openSpec, reasonRefused, ExitValidation},

		{"part done", 2, 1, "done", doneSpec, reasonRefused, ExitValidation},
		{"part done", 2, 1, "close", closeSpec, reasonRefused, ExitValidation},
		{"part done", 2, 1, "start", startSpec, reasonRefused, ExitValidation},
		{"part done", 2, 1, "open", openSpec, reasonRefused, ExitValidation},

		{"fully closed", 2, 2, "done", doneSpec, reasonSatisfied, ExitNoWork},
		{"fully closed", 2, 2, "close", closeSpec, reasonSatisfied, ExitNoWork},
		{"fully closed", 2, 2, "start", startSpec, reasonRefused, ExitValidation},
		{"fully closed", 2, 2, "open", openSpec, reasonRefused, ExitValidation},
	}
	for _, cell := range cells {
		t.Run(cell.shape+"/"+cell.action, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epic := h.epicFixture(cell.children, cell.closed)

			err := h.runTransitionErr(epic, cell.spec, actionFlags[cell.action]...)
			if err == nil {
				t.Fatalf("`%s` on a %s epic error = nil, want a container answer", cell.action, cell.shape)
			}
			if got := commandErrorReason(err); got != cell.wantReason {
				t.Errorf("reason = %q, want %q", got, cell.wantReason)
			}
			if got := ExitCode(err); got != cell.wantExit {
				t.Errorf("exit = %d, want %d", got, cell.wantExit)
			}
			rendered := renderCommandError(t, err)
			phrases := renderedPhrases[cell.wantReason]
			for _, want := range phrases.want {
				if !strings.Contains(rendered, want) {
					t.Errorf("rendered error missing %q:\n%s", want, rendered)
				}
			}
			for _, forbidden := range phrases.forbidden {
				if strings.Contains(rendered, forbidden) {
					t.Errorf("rendered error contains %q:\n%s", forbidden, rendered)
				}
			}
		})
	}
}

// The refusal renders the derived state for a reader. A part-done epic is the
// only shape whose state has two spellings, and the underscored one belongs to
// storage and the wire; printing it mid-sentence leaks a wire format into the
// prose an agent reads.
func TestContainerRefusalRendersTheDerivedStateForReading(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.epicFixture(2, 1)

	err := h.runTransitionErr(epic, doneSpec)
	if err == nil {
		t.Fatalf("done on a part-done epic error = nil, want a refusal")
	}
	if got := err.Error(); !strings.Contains(got, "it is in progress") {
		t.Errorf("refusal does not say the epic is in progress: %s", got)
	}
	if got := err.Error(); strings.Contains(got, "in_progress") {
		t.Errorf("refusal leaks the wire spelling of the derived state: %s", got)
	}
}

// Satisfied is the one predicate both the message and the CLI mappings read, so
// it is pinned directly on hand-built values — the combinations the store can
// produce are covered end to end by the grid above, and this covers the rule
// itself. Matching the target is necessary and not sufficient: two derived
// states match a target while work remains, and the counts are what tell them
// apart.
func TestContainerActionErrorSatisfiedRequiresNoWorkLeft(t *testing.T) {
	cases := []struct {
		name string
		err  model.ContainerActionError
		want bool
	}{{
		name: "done on an epic whose children are all closed",
		err: model.ContainerActionError{
			Action: model.ActionDone, Target: model.StateClosed, State: model.StateClosed,
			Progress: model.Progress{Total: 2, Closed: 2},
		},
		want: true,
	}, {
		name: "start on a part-done epic matches its own target with every child still to do",
		err: model.ContainerActionError{
			Action: model.ActionStart, Target: model.StateInProgress, State: model.StateInProgress,
			Progress: model.Progress{Total: 2, Closed: 1},
		},
		want: false,
	}, {
		name: "open on a childless epic matches a fallback state carrying no information",
		err: model.ContainerActionError{
			Action: model.ActionReopen, Target: model.StateOpen, State: model.StateOpen,
			Progress: model.Progress{},
		},
		want: false,
	}, {
		name: "start on an epic whose children are all closed",
		err: model.ContainerActionError{
			Action: model.ActionStart, Target: model.StateInProgress, State: model.StateClosed,
			Progress: model.Progress{Total: 2, Closed: 2},
		},
		want: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.err.ID = "test-epics-abcd"
			if got := tc.err.Satisfied(); got != tc.want {
				t.Errorf("Satisfied() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A refusal exists to tell an agent what it just asked for, and an agent reads
// a backticked word as a command, so that word has to be the one it typed.
// `lit open` dispatches model.Reopen, whose Name is the persisted event
// encoding "reopen"; a message printing that encoding would answer `lit open`
// by naming a command lit does not have.
//
// Every status spec is driven rather than `open` alone, because a table listing
// only the known-broken verb passes again the first time a second command's
// name diverges from its action's. The expectation is read from the SPEC the
// command was dispatched with, so the assertion is "the refusal quotes the
// command that produced it" rather than a second copy of the verb list, which
// could agree with a wrong one as readily as a right one.
// [LAW:verifiable-goals]
func TestContainerRefusalNamesTheVerbTheAgentTyped(t *testing.T) {
	for _, spec := range []transitionSpec{startSpec, doneSpec, closeSpec, openSpec} {
		t.Run(spec.name, func(t *testing.T) {
			h := newReadyTestHarness(t)
			epic := h.epicFixture(2, 0)
			err := h.runTransitionErr(epic, spec, actionFlags[spec.name]...)
			if err == nil {
				t.Fatalf("`lit %s` on an epic with open children = nil error, want a container rejection", spec.name)
			}
			rendered := renderCommandError(t, err)
			if !strings.Contains(rendered, "`"+spec.name+"`") {
				t.Errorf("`lit %s` is refused without naming `%s`, so the agent cannot copy the command back:\n%s", spec.name, spec.name, rendered)
			}
		})
	}
}

// TestOpenRefusalNeverNamesThePersistedEncoding is the negative half. The
// positive assertion above passes for three of the four verbs whatever the
// code does, because their two names coincide; only `open` can tell a right
// answer from a wrong one, and only by looking for the word that must NOT be
// there.
func TestOpenRefusalNeverNamesThePersistedEncoding(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.epicFixture(2, 0)
	err := h.runTransitionErr(epic, openSpec)
	if err == nil {
		t.Fatal("`lit open` on an epic with open children = nil error, want a container rejection")
	}
	rendered := renderCommandError(t, err)
	if strings.Contains(rendered, string(model.ActionReopen)) {
		t.Errorf("`lit open` is refused by naming %q, the events-table encoding; there is no `lit %s` to run:\n%s", string(model.ActionReopen), string(model.ActionReopen), rendered)
	}
}

// TestEveryVerbAMessageCanPrintIsACommandThatExists states the claim the
// refusals actually rest on, and reads it from the command registry itself.
//
// The registry row takes its name from the spec, so there is one spelling of
// each word, and this reads the registry. [LAW:one-source-of-truth]
func TestEveryVerbAMessageCanPrintIsACommandThatExists(t *testing.T) {
	registered := map[string]bool{}
	for _, spec := range commandSpecs(context.Background(), io.Discard, io.Discard) {
		// A retired row is a pointer to a replacement, not a command that runs:
		// its Run returns RetiredCommandError. Counting it would let `open` be
		// retired the way `assign` was while refusals kept printing `open` and
		// this test kept passing -- the row is still in the registry, so the
		// word would still be found. [LAW:verifiable-goals]
		if spec.Retired {
			continue
		}
		registered[spec.Name] = true
	}
	if len(registered) == 0 {
		t.Fatal("the command registry came back empty, so this test checks nothing")
	}
	for _, action := range model.Actions() {
		verb := action.Verb()
		if !registered[verb] {
			t.Errorf("action %q renders as `%s` in agent-facing refusals, but `lit %s` is not a registered command", action, verb, verb)
		}
	}
}

// TestSatisfiedSentenceNamesTheTypedVerbToo pins the OTHER sentence
// ContainerActionError can produce. No CLI call reaches it with an action whose
// two names differ: Satisfied() requires the children to already establish the
// target, and a container whose children are all done derives Closed, never
// Open, so `lit open` cannot arrive here. That unreachability is exactly why the
// sentence is pinned as a value rather than driven through a command — nothing
// in the reachable set would notice the persisted encoding coming back.
func TestSatisfiedSentenceNamesTheTypedVerbToo(t *testing.T) {
	err := model.ContainerActionError{
		ID: "test-epics-1", Action: model.ActionReopen,
		Target: model.StateOpen, State: model.StateOpen,
		Progress: model.Progress{Total: 1, Closed: 1},
	}
	if !err.Satisfied() {
		t.Fatalf("fixture does not produce the satisfied sentence, so this test checks nothing: %#v", err)
	}
	// "open" is a substring of "reopen", so the absence of the persisted
	// encoding is the half of this that can fail.
	if strings.Contains(err.Error(), string(model.ActionReopen)) {
		t.Errorf("the satisfied sentence names %q, the events-table encoding, where the reader expects the command it typed: %s", string(model.ActionReopen), err.Error())
	}
	if !strings.Contains(err.Error(), "`"+model.ActionReopen.Verb()+"`") {
		t.Errorf("the satisfied sentence does not quote the verb the caller typed (%q): %s", model.ActionReopen.Verb(), err.Error())
	}
}

// TestBulkUsageNamesTheTypedVerb pins the one site of this class that the
// mechanical gate in internal/model/lifecycle cannot see. That gate scans fmt
// calls; this site builds its string by concatenation.
//
// No bulk row routes a Reopen today — `lit bulk` serves the retention verbs,
// whose two names agree — so nothing reachable through the CLI can tell a right
// answer here from a wrong one. The leaf builder takes the action as a
// parameter, though, so the case is constructible, and constructing it is the
// only way this site gets a check that can fail. Same reasoning as
// TestSatisfiedSentenceNamesTheTypedVerbToo.
func TestBulkUsageNamesTheTypedVerb(t *testing.T) {
	leaf := bulkTransitionLeaf(model.Reopen{})()
	help := leaf.fs.cmd.Flags().Lookup("help")
	if help == nil {
		t.Fatal("no help flag on the bulk leaf's flag set, so its usage string cannot be read")
	}
	if strings.Contains(help.Usage, string(model.ActionReopen)) {
		t.Errorf("bulk usage names %q, the events-table encoding: %q", string(model.ActionReopen), help.Usage)
	}
	want := "help for bulk " + model.ActionReopen.Verb()
	if help.Usage != want {
		t.Errorf("bulk usage = %q, want %q", help.Usage, want)
	}
}
