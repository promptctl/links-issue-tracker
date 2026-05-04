package lifecycle

import "testing"

func TestOwnedStatusStateAndActions(t *testing.T) {
	tests := []struct {
		name    string
		status  OwnedStatus
		actions []ActionName
	}{
		{name: "open", status: OwnedStatus{Value: Open}, actions: []ActionName{ActionStart, ActionClose}},
		{name: "in progress", status: OwnedStatus{Value: InProgress}, actions: []ActionName{ActionDone, ActionClose}},
		{name: "closed", status: OwnedStatus{Value: Closed}, actions: []ActionName{ActionReopen}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.status.State() != tt.status.Value {
				t.Fatalf("State() = %q, want %q", tt.status.State(), tt.status.Value)
			}
			assertActions(t, tt.status.AvailableActions(), tt.actions)
		})
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
	if actions := all.AvailableActions(); len(actions) != 0 {
		t.Fatalf("AvailableActions() = %#v, want empty", actions)
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

func assertActions(t *testing.T, got []ActionName, want []ActionName) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("actions = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("actions = %#v, want %#v", got, want)
		}
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
