package lifecycle

import (
	"slices"
	"strings"
	"testing"
)

// The state nodes are every status, phase and retention the lifecycle has,
// the phases after the status they nest inside, then the retentions. Each
// name is one node's: a node is its name, and `enter <state>` names no axis,
// so a name two axes shared would be one node, and crossing it in one axis
// would read as crossing it in the other.
func TestStateNodesNestThePhasesInsideThePhasedState(t *testing.T) {
	var got []string
	for _, node := range StateNodes() {
		got = append(got, node.String())
	}
	want := []string{"open", "in_progress", "plan", "impl", "review", "merge", "closed", "live", "archived", "deleted"}
	if !slices.Equal(got, want) {
		t.Fatalf("StateNodes() = %v, want %v", got, want)
	}
	if distinct := slices.Compact(slices.Sorted(slices.Values(got))); len(distinct) != len(got) {
		t.Fatalf("StateNodes() = %v, want each name once", got)
	}
}

// Every node reads back from its own name, in any case and with the
// in-progress spelling ParseState reads, and a name the lifecycle lacks is
// refused naming every state.
func TestParseStateNode(t *testing.T) {
	for _, node := range StateNodes() {
		if got, err := ParseStateNode(" " + strings.ToUpper(node.String()) + " "); err != nil || got != node {
			t.Errorf("ParseStateNode(%q) = %v, %v; want the node", node, got, err)
		}
	}
	if got, err := ParseStateNode("in-progress"); err != nil || got != StatusNode(InProgress) {
		t.Errorf("ParseStateNode(in-progress) = %v, %v; want in_progress", got, err)
	}
	for _, text := range []string{"", "in progress", "reveiw", "frozen"} {
		_, err := ParseStateNode(text)
		if err == nil || !strings.Contains(err.Error(), "open, in_progress, plan, impl, review, merge, closed, live, archived, deleted") {
			t.Errorf("ParseStateNode(%q) error = %v, want a refusal naming every state", text, err)
		}
	}
}
