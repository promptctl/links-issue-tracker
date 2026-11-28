package cli

import (
	"reflect"
	"strings"
	"testing"
)

// Every command that fires a workflow occasion names it on its event: the
// moments it carried — a read's one event, a creation's event and the states
// the new ticket entered — the definitions that fired, the load's warnings
// (an empty list when there were none) and the trace error (null when no
// trace was dropped). The transitions are covered by
// TestEveryCrossingFiresOnTheTransitionThatMakesIt.
func TestEveryDispatchingCommandRecordsItsOccasion(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "dispatch epic")

	created := []any{"ticket_created", "enter open", "enter live"}
	for _, step := range []struct {
		args []string
		when []any
	}{
		{[]string{"new", "--title", "Third", "--topic", "demo", "--type", "task", "--parent", epic}, created},
		{[]string{"followup", "--on", first, "--title", "Fourth"}, created},
		{[]string{"show", first}, []any{"show_ticket"}},
		{[]string{"update", first, "--title", "First, renamed"}, []any{"ticket_updated"}},
		{[]string{"comment", "add", first, "--body", "a note"}, []any{"comment_added"}},
		{[]string{"next"}, []any{"next_pulled"}},
		{[]string{"backlog"}, []any{"show_backlog"}},
	} {
		runCLIInDir(t, dir, step.args...)
		want := map[string]any{"workflows.when": step.when, "workflows.fired": []any{}, "workflows.warnings": []any{}, "workflows.trace_error": nil}
		if got := workflowFacts(t); !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}

// A definition bound by labels alone fires on whatever moment a ticket
// carrying the label passes, and one bound to a moment too fires only on that
// moment: bulk comment fires comment_added on each ticket it comments on, and
// archive, unarchive, delete and restore cross between live, archived and
// deleted, none of them entering or exiting closed.
func TestLabelBoundDefinitionsFireOnBulkCommentsAndRetentionMoves(t *testing.T) {
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
		{[]string{"archive", first}, workflowFacts, []any{"exit live", "enter archived"}},
		{[]string{"unarchive", first}, workflowFacts, []any{"exit archived", "enter live"}},
		{[]string{"delete", first}, workflowFacts, []any{"exit live", "enter deleted"}},
		{[]string{"restore", first}, workflowFacts, []any{"exit deleted", "enter live"}},
	} {
		out := runCLIInDir(t, dir, step.args...)
		if !strings.Contains(out, "TRACKED "+first+"\n") || strings.Contains(out, "CLOSING") {
			t.Errorf("lit %v printed %q; want the label-bound guidance alone", step.args, out)
		}
		got := step.facts(t)
		delete(got, "workflows.warnings")
		if want := map[string]any{"workflows.when": step.when, "workflows.fired": []any{"tracked-note"}, "workflows.trace_error": nil}; !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}
