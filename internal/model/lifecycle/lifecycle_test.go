package lifecycle

import (
	"testing"
	"time"
)

func TestOwnedStatusStateMirrorsValue(t *testing.T) {
	for _, state := range []State{Open, InProgress, Closed} {
		if got := (OwnedStatus{Value: state}).State(); got != state {
			t.Fatalf("State() = %q, want %q", got, state)
		}
	}
}

// TestOwnedStatusApplyTargetStateMatrix exercises every (from, action) pair in
// the 3x4 matrix. Each cell asserts the post-state matches the action's target
// state — same-state cells (start@InProgress, done@Closed, close@Closed,
// reopen@Open) are no-ops that preserve the receiver. There are no rejection
// cells: every cell here is a legal call.
func TestOwnedStatusApplyTargetStateMatrix(t *testing.T) {
	allActions := []ActionName{ActionStart, ActionDone, ActionClose, ActionReopen}
	for _, from := range []State{Open, InProgress, Closed} {
		for _, action := range allActions {
			target, _ := ActionTargetState(action)
			t.Run(string(from)+"_"+string(action), func(t *testing.T) {
				next, err := OwnedStatus{Value: from}.Apply(action, "tester", "")
				if err != nil {
					t.Fatalf("Apply(%s on %s) error = %v, want success", action, from, err)
				}
				if got := next.State(); got != target {
					t.Fatalf("Apply(%s on %s).State() = %q, want %q", action, from, got, target)
				}
			})
		}
	}
}

// TestOwnedStatusApplySameStateReturnsReceiverUnchanged pins the no-op contract
// that downstream store layers depend on: when the action's target equals the
// current state, Apply returns the receiver verbatim so writeStatusTransition
// can recognize same-state calls without re-deriving them.
func TestOwnedStatusApplySameStateReturnsReceiverUnchanged(t *testing.T) {
	closedAt := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		from   State
		action ActionName
	}{
		{Open, ActionReopen},
		{InProgress, ActionStart},
		{Closed, ActionDone},
		{Closed, ActionClose},
	}
	for _, tc := range cases {
		original := OwnedStatus{Value: tc.from, Assignee: "alice", ClosedAt: &closedAt}
		next, err := original.Apply(tc.action, "tester", "")
		if err != nil {
			t.Fatalf("Apply(%s on %s) error = %v", tc.action, tc.from, err)
		}
		got, ok := next.(OwnedStatus)
		if !ok {
			t.Fatalf("Apply(%s on %s) returned %T, want OwnedStatus", tc.action, tc.from, next)
		}
		if got != original {
			t.Fatalf("Apply(%s on %s) mutated receiver: got %#v, want %#v", tc.action, tc.from, got, original)
		}
	}
}

// TestOwnedStatusApplyClosedAtBookkeeping locks in the ClosedAt invariant:
// transitions into Closed stamp a timestamp; transitions into Open clear it;
// transitions into InProgress leave it as-is.
func TestOwnedStatusApplyClosedAtBookkeeping(t *testing.T) {
	priorClosed := time.Unix(1_700_000_000, 0).UTC()

	openToClosed, err := OwnedStatus{Value: Open}.Apply(ActionClose, "tester", "")
	if err != nil {
		t.Fatalf("Apply(close on open) error = %v", err)
	}
	if closedAt := openToClosed.(OwnedStatus).ClosedAt; closedAt == nil {
		t.Fatal("Apply(close on open).ClosedAt = nil, want stamped")
	}

	inProgressToClosed, err := OwnedStatus{Value: InProgress}.Apply(ActionDone, "tester", "")
	if err != nil {
		t.Fatalf("Apply(done on in_progress) error = %v", err)
	}
	if closedAt := inProgressToClosed.(OwnedStatus).ClosedAt; closedAt == nil {
		t.Fatal("Apply(done on in_progress).ClosedAt = nil, want stamped")
	}

	closedToOpen, err := OwnedStatus{Value: Closed, ClosedAt: &priorClosed}.Apply(ActionReopen, "tester", "")
	if err != nil {
		t.Fatalf("Apply(reopen on closed) error = %v", err)
	}
	if closedAt := closedToOpen.(OwnedStatus).ClosedAt; closedAt != nil {
		t.Fatalf("Apply(reopen on closed).ClosedAt = %v, want nil", closedAt)
	}

	closedToInProgress, err := OwnedStatus{Value: Closed, ClosedAt: &priorClosed}.Apply(ActionStart, "tester", "")
	if err != nil {
		t.Fatalf("Apply(start on closed) error = %v", err)
	}
	// start does not target Open or Closed; ClosedAt is preserved untouched.
	if closedAt := closedToInProgress.(OwnedStatus).ClosedAt; closedAt == nil || !closedAt.Equal(priorClosed) {
		t.Fatalf("Apply(start on closed).ClosedAt = %v, want preserved %v", closedAt, priorClosed)
	}
}

func TestOwnedStatusApplyRejectsParseBypass(t *testing.T) {
	if _, err := (OwnedStatus{Value: Open}).Apply(ActionName("bogus"), "tester", ""); err == nil {
		t.Fatal("Apply(bogus) error = nil, want unsupported-action error")
	}
}

func TestAllOfState(t *testing.T) {
	tests := []struct {
		name    string
		members []Lifecycle
		want    State
	}{
		{name: "all open", members: []Lifecycle{OwnedStatus{Value: Open}, OwnedStatus{Value: Open}}, want: Open},
		{name: "mixed closed", members: []Lifecycle{OwnedStatus{Value: Open}, OwnedStatus{Value: Closed}}, want: InProgress},
		{name: "in progress", members: []Lifecycle{OwnedStatus{Value: Open}, OwnedStatus{Value: InProgress}}, want: InProgress},
		{name: "all closed", members: []Lifecycle{OwnedStatus{Value: Closed}, OwnedStatus{Value: Closed}}, want: Closed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AllOf{Members: tt.members}.State()
			if got != tt.want {
				t.Fatalf("State() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllOfProgressAndActions(t *testing.T) {
	all := AllOf{Members: []Lifecycle{
		OwnedStatus{Value: Open},
		AllOf{Members: []Lifecycle{
			OwnedStatus{Value: InProgress},
			OwnedStatus{Value: Closed},
		}},
	}}
	progress := all.Progress()
	if progress.Open != 1 || progress.InProgress != 1 || progress.Closed != 1 || progress.Total != 3 {
		t.Fatalf("Progress() = %#v, want 1/1/1 total 3", progress)
	}
}

// Containers are structurally non-actionable: the model dispatch boundary
// relies on this to route them to the epic-aware rejection instead.
func TestAllOfIsNotActionable(t *testing.T) {
	var container Lifecycle = AllOf{Members: []Lifecycle{OwnedStatus{Value: Open}}}
	if _, ok := container.(Actionable); ok {
		t.Fatal("AllOf satisfies Actionable; containers must not be actionable — their state derives from children")
	}
}

func TestWalkVisitsAllPrimitives(t *testing.T) {
	tree := AllOf{Members: []Lifecycle{
		OwnedStatus{Value: Open},
		AllOf{Members: []Lifecycle{
			OwnedStatus{Value: InProgress},
			OwnedStatus{Value: Closed},
		}},
	}}
	var states []State
	Walk(tree, func(current Lifecycle) bool {
		if status, ok := current.(OwnedStatus); ok {
			states = append(states, status.Value)
		}
		return true
	})
	want := []State{Open, InProgress, Closed}
	if len(states) != len(want) {
		t.Fatalf("visited states = %#v, want %#v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("visited states = %#v, want %#v", states, want)
		}
	}
}

type progressOnly struct {
	progress Progress
}

func (p progressOnly) State() State {
	return InProgress
}

func (p progressOnly) Progress() Progress {
	return p.progress
}

func TestAllOfProgressIncludesNonStatusLeafPrimitives(t *testing.T) {
	tree := AllOf{Members: []Lifecycle{
		OwnedStatus{Value: Open},
		progressOnly{progress: Progress{InProgress: 2, Total: 2}},
	}}
	progress := tree.Progress()
	if progress.Open != 1 || progress.InProgress != 2 || progress.Total != 3 {
		t.Fatalf("Progress() = %#v, want open=1 in_progress=2 total=3", progress)
	}
}

func TestParseStateNormalizes(t *testing.T) {
	tests := []struct {
		input string
		want  State
	}{
		{"open", Open},
		{"Open", Open},
		{"OPEN", Open},
		{"in_progress", InProgress},
		{"IN_PROGRESS", InProgress},
		{"in-progress", InProgress},
		{"  closed  ", Closed},
		{"Closed", Closed},
	}
	for _, tt := range tests {
		got, err := ParseState(tt.input)
		if err != nil {
			t.Fatalf("ParseState(%q) error = %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("ParseState(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseStateRejectsInvalid(t *testing.T) {
	for _, input := range []string{"todo", "unknown", "garbage"} {
		_, err := ParseState(input)
		if err == nil {
			t.Fatalf("ParseState(%q) expected error", input)
		}
	}
}

func TestParseStateRejectsBlank(t *testing.T) {
	for _, input := range []string{"", "  "} {
		_, err := ParseState(input)
		if err == nil {
			t.Fatalf("ParseState(%q) expected error", input)
		}
	}
}

func TestDefaultOpenReturnsOpenForInvalid(t *testing.T) {
	for _, input := range []string{"todo", "", "  ", "unknown", "garbage"} {
		got := DefaultOpen(input)
		if got != Open {
			t.Fatalf("DefaultOpen(%q) = %q, want %q", input, got, Open)
		}
	}
}

func TestParseActionValid(t *testing.T) {
	tests := []struct {
		input string
		want  ActionName
	}{
		{"start", ActionStart},
		{"Done", ActionDone},
		{"  close  ", ActionClose},
		{"REOPEN", ActionReopen},
	}
	for _, tt := range tests {
		got, err := ParseAction(tt.input)
		if err != nil {
			t.Fatalf("ParseAction(%q) error = %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("ParseAction(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseActionRejectsNonLifecycle(t *testing.T) {
	for _, input := range []string{"archive", "delete", "restore", "bogus"} {
		_, err := ParseAction(input)
		if err == nil {
			t.Fatalf("expected error for non-lifecycle action %q", input)
		}
	}
}

func TestActionTargetState(t *testing.T) {
	tests := []struct {
		action ActionName
		want   State
	}{
		{ActionStart, InProgress},
		{ActionDone, Closed},
		{ActionClose, Closed},
		{ActionReopen, Open},
	}
	for _, tt := range tests {
		got, ok := ActionTargetState(tt.action)
		if !ok {
			t.Fatalf("ActionTargetState(%q) ok=false; want true", tt.action)
		}
		if got != tt.want {
			t.Fatalf("ActionTargetState(%q) = %q, want %q", tt.action, got, tt.want)
		}
	}
	if _, ok := ActionTargetState(ActionName("bogus")); ok {
		t.Fatal("ActionTargetState(\"bogus\") ok=true; want false")
	}
}

func TestStateDisplay(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{Open, "open"},
		{InProgress, "in progress"},
		{Closed, "closed"},
	}
	for _, tt := range tests {
		if got := tt.state.Display(); got != tt.want {
			t.Fatalf("State(%q).Display() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
