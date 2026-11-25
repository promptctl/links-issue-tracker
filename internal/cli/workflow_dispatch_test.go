package cli

import (
	"reflect"
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
