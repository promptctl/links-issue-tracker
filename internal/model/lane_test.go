package model_test

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func epic(id string) *model.Issue {
	return &model.Issue{ID: id, IssueType: model.TypeEpic}
}

func task(id, lane string) model.Issue {
	return model.Issue{ID: id, Lane: lane, IssueType: model.TypeTask}
}

// TestLaneOfGroupsSiblingsBySpelling: same epic, same lane string — including
// the empty spelling, which is why an epic that declares no lanes is one lane
// rather than a case anyone has to special-case.
func TestLaneOfGroupsSiblingsBySpelling(t *testing.T) {
	parent := epic("E")
	cases := []struct {
		name  string
		left  model.Issue
		right model.Issue
		same  bool
	}{
		{name: "same declared lane", left: task("A", "bugs"), right: task("B", "bugs"), same: true},
		{name: "the unnamed default lane", left: task("A", ""), right: task("B", ""), same: true},
		{name: "different lanes", left: task("A", "bugs"), right: task("B", "docs"), same: false},
		{name: "named vs unnamed", left: task("A", "bugs"), right: task("B", ""), same: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if same := model.LaneOf(tc.left, parent) == model.LaneOf(tc.right, parent); same != tc.same {
				t.Fatalf("same lane = %v, want %v", same, tc.same)
			}
		})
	}
}

// TestLaneOfScopesToTheEpic: Issue.Lane is a bare string, so two unrelated epics
// may spell a lane identically without sharing it. Half of a lane's name is the
// epic, and that is what stops one epic's "bugs" from being held by a checkout
// working another's.
func TestLaneOfScopesToTheEpic(t *testing.T) {
	if model.LaneOf(task("A", "bugs"), epic("E1")) == model.LaneOf(task("B", "bugs"), epic("E2")) {
		t.Fatal("lanes spelled alike under different epics collided")
	}
}

// TestLaneOfWithoutAnEpicIsALaneOfOne covers the two shapes that scope nothing:
// no parent at all, and a parent that is not a container. Lanes partition an
// epic's children, so an issue parented to a leaf is in its own lane exactly as
// a parentless one is.
func TestLaneOfWithoutAnEpicIsALaneOfOne(t *testing.T) {
	solo := task("A", "ignored")
	leafParent := task("P", "")

	if model.LaneOf(solo, nil) != model.LaneOf(solo, &leafParent) {
		t.Fatal("a non-container parent scoped a lane")
	}
	if model.LaneOf(solo, nil) == model.LaneOf(task("B", "ignored"), nil) {
		t.Fatal("two parentless tickets shared a lane")
	}
}

// TestSoloLaneCannotCollideWithAnEpicLane: a lane of one is keyed by an issue id
// under no epic, and an epic-scoped lane always names a real epic, so the two
// namespaces cannot meet however a lane happens to be spelled.
func TestSoloLaneCannotCollideWithAnEpicLane(t *testing.T) {
	if model.LaneOf(task("E", ""), nil) == model.LaneOf(task("A", ""), epic("E")) {
		t.Fatal("a parentless ticket named E collided with epic E's default lane")
	}
	if model.LaneOf(task("A", ""), nil) == model.LaneOf(task("B", "A"), epic("")) {
		t.Fatal("a parentless ticket collided with a lane spelled after it")
	}
}

// TestLaneStringDistinguishesTheEpicFromItsDefaultLane: an epic's unnamed lane
// must not render as the bare epic id, or a log line about a lane would read as
// one about the epic that contains it.
func TestLaneStringDistinguishesTheEpicFromItsDefaultLane(t *testing.T) {
	defaultLane := model.LaneOf(task("A", ""), epic("E")).String()
	if defaultLane == "E" {
		t.Fatal("epic E's default lane rendered as the epic itself")
	}
	if got, want := model.LaneOf(task("A", "bugs"), epic("E")).String(), "E#bugs"; got != want {
		t.Fatalf("lane string = %q, want %q", got, want)
	}
	if got, want := model.LaneOf(task("A", "ignored"), nil).String(), "A"; got != want {
		t.Fatalf("solo lane string = %q, want %q", got, want)
	}
}

// TestLaneAccessorsReportBothHalves keeps Epic/Key honest for the renderers that
// will read them.
func TestLaneAccessorsReportBothHalves(t *testing.T) {
	lane := model.LaneOf(task("A", "bugs"), epic("E"))
	if lane.Epic() != "E" || lane.Key() != "bugs" {
		t.Fatalf("lane halves = (%q, %q), want (E, bugs)", lane.Epic(), lane.Key())
	}
	solo := model.LaneOf(task("A", "bugs"), nil)
	if solo.Epic() != "" || solo.Key() != "A" {
		t.Fatalf("solo lane halves = (%q, %q), want (\"\", A)", solo.Epic(), solo.Key())
	}
}

// TestInPlayIsTheOneUnfinishedRule: closed leaves the flow, and so does freezing
// an open issue by archiving or deleting it.
func TestInPlayIsTheOneUnfinishedRule(t *testing.T) {
	hydrate := func(t *testing.T, state model.State) model.Issue {
		t.Helper()
		issue, err := model.HydrateStatus(task("A", ""), model.StatusView{Value: state})
		if err != nil {
			t.Fatalf("hydrate: %v", err)
		}
		return issue
	}

	if !hydrate(t, model.StateOpen).InPlay() {
		t.Error("an open issue is not in play")
	}
	if !hydrate(t, model.StateInProgress).InPlay() {
		t.Error("an in-progress issue is not in play")
	}
	if hydrate(t, model.StateClosed).InPlay() {
		t.Error("a closed issue is still in play")
	}
	for name, frozen := range map[string]model.Retention{"archived": model.Archived{}, "deleted": model.Deleted{}} {
		issue := hydrate(t, model.StateOpen)
		issue.SetRetention(frozen)
		if issue.InPlay() {
			t.Errorf("an open but %s issue is still in play", name)
		}
	}
}
