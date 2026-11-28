package workflows

import "github.com/promptctl/links-issue-tracker/internal/model"

// stateNode is the lifecycle's state node named name, for a test that spells
// one; a name the lifecycle lacks stops the test binary.
func stateNode(name string) model.StateNode {
	node, err := model.ParseStateNode(name)
	if err != nil {
		panic(err)
	}
	return node
}

func entering(state string) Crossing { return Crossing{Side: Enter, State: stateNode(state)} }
func exiting(state string) Crossing  { return Crossing{Side: Exit, State: stateNode(state)} }

// firesOn is the activation the key reader builds for keys listing when and
// labels, nil for a key left out.
func firesOn(when []Moment, labels []string) Activation {
	return Activation{When: Key[Moment]{entries: when}, Labels: Key[string]{entries: labels}}
}

// fires reports whether def fires on o, asked as a command asks: through the
// matching of a set holding it.
func fires(def Definition, o Occasion) bool {
	return len(Set{Definitions: []Definition{def}}.Matching(o)) == 1
}
