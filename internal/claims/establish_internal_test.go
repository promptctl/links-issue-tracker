package claims

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestEstablishingCoversEveryAction is the reason establishing is a map and not
// a switch. A ninth lifecycle verb is a claims decision — does performing it
// take the lane? — and a default arm would answer "no" on the author's behalf,
// silently, in a package they were not editing. Here it fails instead, naming
// the verb nobody classified.
func TestEstablishingCoversEveryAction(t *testing.T) {
	for _, action := range model.Actions() {
		if _, classified := establishing[action]; !classified {
			t.Errorf("lifecycle action %q is not classified in establishing: decide whether performing it takes a lane", action)
		}
	}
	if len(establishing) != len(model.Actions()) {
		t.Errorf("establishing classifies %d actions, but the sealed set has %d: it names a verb that no longer exists", len(establishing), len(model.Actions()))
	}
}

// TestOnlyStartAndDoneEstablish pins the classification itself, so widening it
// is a deliberate edit to a test that says why the set is narrow rather than an
// unremarked flip of a boolean.
func TestOnlyStartAndDoneEstablish(t *testing.T) {
	want := map[model.ActionName]bool{
		model.ActionStart: true,
		model.ActionDone:  true,
	}
	for _, action := range model.Actions() {
		if got := establishing[action]; got != want[action] {
			t.Errorf("establishing[%q] = %v, want %v", action, got, want[action])
		}
	}
}

// TestAbsentVerbDoesNotEstablish: plain field updates record no verb at all, and
// an edit is not a transition.
func TestAbsentVerbDoesNotEstablish(t *testing.T) {
	if establishes(model.IssueEvent{Action: ""}) {
		t.Error("an event with no lifecycle verb established a claim")
	}
	if establishes(model.IssueEvent{Action: "nonsense"}) {
		t.Error("an unrecognized lifecycle verb established a claim")
	}
}
