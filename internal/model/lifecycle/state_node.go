package lifecycle

import (
	"fmt"
	"slices"
	"strings"
)

// StateNode is one state of a ticket's lifecycle, as a statechart names its
// states: a status, a phase (a state nested inside PhasedState), or a
// retention (a region of its own, beside the status). A ticket is in one node
// of each region at once, plus its phase while it has one, and a change of
// the ticket exits the nodes it left and enters the ones it reached.
// [LAW:types-are-the-program] the name is unexported, so every node but the
// zero value, which nothing produces, is built here from a status, a phase or
// a retention, and a parsed node is always one of StateNodes.
type StateNode struct{ name string }

// String is the node's name: its status, phase or retention as that axis
// spells it.
func (n StateNode) String() string { return n.name }

// StatusNode is the node of a status.
func StatusNode(s State) StateNode { return StateNode{string(s)} }

// PhaseNode is the node of a phase. It takes the phase as given, so a phase
// read back from history under a name a newer lit wrote is that phase's node.
func PhaseNode(p Phase) StateNode { return StateNode{string(p)} }

// RetentionNode is the node of a retention, named by RetentionName.
func RetentionNode(r Retention) StateNode { return StateNode{RetentionName(r)} }

// StateNodes is every node, in the order the work passes through them: the
// statuses, with the phases after the status they nest inside, then the
// retentions. [LAW:one-source-of-truth] each axis lists itself (States,
// Phases, Retentions), so a state the lifecycle adds is a node with no edit
// here.
func StateNodes() []StateNode {
	var nodes []StateNode
	for _, status := range States() {
		nodes = append(nodes, StatusNode(status))
		if status == PhasedState {
			for _, phase := range Phases() {
				nodes = append(nodes, PhaseNode(phase))
			}
		}
	}
	for _, retention := range Retentions() {
		nodes = append(nodes, RetentionNode(retention))
	}
	return nodes
}

// ParseStateNode maps an untrusted state name to the node it names, spelled
// as ParseState and ParsePhase read their names: any case, surrounding space
// ignored, and in-progress for in_progress.
// [LAW:single-enforcer] The only string-to-StateNode gate.
func ParseStateNode(value string) (StateNode, error) {
	nodes := StateNodes()
	node := StateNode{stateName(value)}
	if slices.Contains(nodes, node) {
		return node, nil
	}
	names := make([]string, len(nodes))
	for i, known := range nodes {
		names[i] = known.name
	}
	return StateNode{}, fmt.Errorf("unknown state %q: a ticket's states are %s", value, strings.Join(names, ", "))
}
