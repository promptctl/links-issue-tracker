package cli

import (
	"reflect"
	"testing"
)

// Every command that fires a workflow occasion names it on its event: the
// event, the states it moved (none, outside a transition) and the definitions
// that fired, an empty list when none did. The transitions are covered by
// TestPhaseMovesFireStateBoundWorkflows.
func TestEveryDispatchingCommandRecordsItsOccasion(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, first, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "dispatch epic")

	for _, step := range []struct {
		args  []string
		event string
		fired []any
	}{
		{[]string{"new", "--title", "Third", "--topic", "demo", "--type", "task", "--parent", epic}, "ticket_created", []any{}},
		{[]string{"followup", "--on", first, "--title", "Fourth"}, "ticket_created", []any{}},
		{[]string{"show", first}, "show_ticket", []any{}},
		{[]string{"update", first, "--title", "First, renamed"}, "ticket_updated", []any{}},
		{[]string{"comment", "add", first, "--body", "a note"}, "comment_added", []any{}},
		// The shipped `next` definition is the command's answer, so it fires.
		{[]string{"next"}, "next", []any{"next"}},
		{[]string{"backlog"}, "show_backlog", []any{}},
	} {
		runCLIInDir(t, dir, step.args...)
		want := map[string]any{"workflows.event": step.event, "workflows.exited": []any{}, "workflows.entered": []any{}, "workflows.fired": step.fired}
		if got := workflowFacts(t); !reflect.DeepEqual(got, want) {
			t.Errorf("lit %v event facts = %v, want %v", step.args, got, want)
		}
	}
}
