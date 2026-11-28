package workflows

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

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
	return Activation{keys: &Keys{When: Key[Moment]{entries: when}, Labels: Key[string]{entries: labels}}}
}

// keysOf is the keys def fires on; a definition that never fires fails the
// test.
func keysOf(t *testing.T, def Definition) Keys {
	t.Helper()
	keys, fires := def.Activation.Keys()
	if !fires {
		t.Fatalf("definition %+v never fires, want one firing on its keys", def)
	}
	return keys
}
