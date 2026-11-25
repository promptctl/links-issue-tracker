package cli

import (
	"reflect"
	"strings"
	"testing"
)

// Every command that fires a workflow occasion names it on its event: the
// moments it carried — a read's one event — the definitions that fired and
// the load's warnings, an empty list when there were none. The transitions are
// covered by TestEveryCrossingFiresOnTheTransitionThatMakesIt.
func TestEveryDispatchingCommandRecordsItsOccasion(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "dispatch epic")

	for _, step := range []struct {
		args  []string
		event string
	}{
		{[]string{"new", "--title", "Third", "--topic", "demo", "--type", "task", "--parent", epic}, "ticket_created"},
		{[]string{"followup", "--on", first, "--title", "Fourth"}, "ticket_created"},
		{[]string{"show", first}, "show_ticket"},
		{[]string{"update", first, "--title", "First, renamed"}, "ticket_updated"},
		{[]string{"comment", "add", first, "--body", "a note"}, "comment_added"},
		{[]string{"next"}, "next_pulled"},
		{[]string{"backlog"}, "show_backlog"},
	} {
		runCLIInDir(t, dir, step.args...)
		want := map[string]any{"workflows.when": []any{step.event}, "workflows.fired": []any{}, "workflows.warnings": []any{}}
		if got := workflowFacts(t); !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}

// A definition bound by labels alone fires on every verb that acts on a
// ticket carrying the label, the bulk ones among them: bulk comment fires on
// each ticket it comments on, and archive, unarchive, delete and restore
// cross no state, so they fire it and never a definition bound to a moment.
func TestLabelBoundDefinitionsFireOnEveryVerbActingOnTheTicket(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "label", "add", first, "tracked")
	writeCLIWorkflow(t, dir, "tracked.md", "---\nid: tracked-note\nlabels: [tracked]\n---\nTRACKED <id>")
	writeCLIWorkflow(t, dir, "closing.md", "---\nid: closing-note\nwhen: [enter closed, exit closed]\nlabels: [tracked]\n---\nCLOSING <id>")

	for _, step := range []struct {
		args  []string
		facts func(*testing.T) map[string]any
		when  []any
	}{
		{[]string{"bulk", "comment", "--ids", first, "--body", "a note"}, bulkItemWorkflowFacts, []any{"comment_added"}},
		{[]string{"archive", first}, workflowFacts, []any{}},
		{[]string{"unarchive", first}, workflowFacts, []any{}},
		{[]string{"delete", first}, workflowFacts, []any{}},
		{[]string{"restore", first}, workflowFacts, []any{}},
	} {
		out := runCLIInDir(t, dir, step.args...)
		if !strings.Contains(out, "TRACKED "+first+"\n") || strings.Contains(out, "CLOSING") {
			t.Errorf("lit %v printed %q; want the label-bound guidance alone", step.args, out)
		}
		got := step.facts(t)
		delete(got, "workflows.warnings")
		if want := map[string]any{"workflows.when": step.when, "workflows.fired": []any{"tracked-note"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}
