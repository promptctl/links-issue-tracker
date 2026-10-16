package workflows

import "testing"

func TestMatchesSingleDimensions(t *testing.T) {
	cases := []struct {
		name     string
		def      Definition
		occasion Occasion
		want     bool
	}{
		{
			name: "inert never fires, even on an empty occasion",
			def:  Definition{ID: "inert"},
			want: false,
		},
		{
			name:     "events: fired event listed",
			def:      Definition{Events: []Event{EventShowBacklog, EventShowTicket}},
			occasion: Occasion{Event: EventShowTicket},
			want:     true,
		},
		{
			name:     "events: fired event not listed",
			def:      Definition{Events: []Event{EventShowBacklog}},
			occasion: Occasion{Event: EventShowTicket},
			want:     false,
		},
		{
			name:     "events: no event on the occasion",
			def:      Definition{Events: []Event{EventShowBacklog}},
			occasion: Occasion{Labels: []string{"x"}},
			want:     false,
		},
		{
			name:     "labels: OR within the dimension",
			def:      Definition{Labels: []string{"needs-design", "blocked"}},
			occasion: Occasion{Event: EventShowTicket, Labels: []string{"other", "blocked"}},
			want:     true,
		},
		{
			name:     "labels: no overlap",
			def:      Definition{Labels: []string{"needs-design"}},
			occasion: Occasion{Event: EventShowTicket, Labels: []string{"other"}},
			want:     false,
		},
		{
			name:     "labels: occasion carries no ticket",
			def:      Definition{Labels: []string{"needs-design"}},
			occasion: Occasion{Event: EventShowBacklog},
			want:     false,
		},
		{
			name:     "states: enter matches entered",
			def:      Definition{States: []StateActivation{{State: "in_progress", When: WhenEnter}}},
			occasion: Occasion{Entered: []string{"in_progress"}, Exited: []string{"open"}},
			want:     true,
		},
		{
			name:     "states: enter does not match exited",
			def:      Definition{States: []StateActivation{{State: "open", When: WhenEnter}}},
			occasion: Occasion{Entered: []string{"in_progress"}, Exited: []string{"open"}},
			want:     false,
		},
		{
			name:     "states: exit matches exited",
			def:      Definition{States: []StateActivation{{State: "open", When: WhenExit}}},
			occasion: Occasion{Entered: []string{"in_progress"}, Exited: []string{"open"}},
			want:     true,
		},
		{
			name:     "states: custom stage names are just names",
			def:      Definition{States: []StateActivation{{State: "ready-to-pull", When: WhenEnter}}},
			occasion: Occasion{Entered: []string{"ready-to-pull"}, Exited: []string{"design"}},
			want:     true,
		},
		{
			name:     "states: enter matches a state nested inside another the transition entered",
			def:      Definition{States: []StateActivation{{State: "plan", When: WhenEnter}}},
			occasion: Occasion{Entered: []string{"in_progress", "plan"}, Exited: []string{"open"}},
			want:     true,
		},
		{
			name:     "states: enter matches the outer state of a nested entry",
			def:      Definition{States: []StateActivation{{State: "in_progress", When: WhenEnter}}},
			occasion: Occasion{Entered: []string{"in_progress", "plan"}, Exited: []string{"open"}},
			want:     true,
		},
		{
			name:     "states: exit matches every state a transition left",
			def:      Definition{States: []StateActivation{{State: "review", When: WhenExit}}},
			occasion: Occasion{Entered: []string{"closed"}, Exited: []string{"in_progress", "review"}},
			want:     true,
		},
		{
			name:     "states: a nested state on one side does not satisfy the other",
			def:      Definition{States: []StateActivation{{State: "plan", When: WhenExit}}},
			occasion: Occasion{Entered: []string{"in_progress", "plan"}, Exited: []string{"open"}},
			want:     false,
		},
		{
			name:     "states: no transition on the occasion never satisfies a state binding",
			def:      Definition{States: []StateActivation{{State: "open", When: WhenEnter}}},
			occasion: Occasion{Event: EventShowTicket},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.def.Matches(tc.occasion); got != tc.want {
				t.Fatalf("Matches(%+v) = %v, want %v", tc.occasion, got, tc.want)
			}
		})
	}
}

// The motivating example from the spec: "when an agent calls 'lit show' on a
// ticket with a 'need-design' label, it includes this guidance" — events AND
// labels must both be satisfied.
func TestMatchesAndAcrossDimensions(t *testing.T) {
	def := Definition{
		Events: []Event{EventShowTicket},
		Labels: []string{"need-design"},
	}
	cases := []struct {
		name     string
		occasion Occasion
		want     bool
	}{
		{"both satisfied", Occasion{Event: EventShowTicket, Labels: []string{"need-design"}}, true},
		{"event only", Occasion{Event: EventShowTicket, Labels: []string{"other"}}, false},
		{"label only", Occasion{Event: EventShowBacklog, Labels: []string{"need-design"}}, false},
		{"neither", Occasion{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := def.Matches(tc.occasion); got != tc.want {
				t.Fatalf("Matches(%+v) = %v, want %v", tc.occasion, got, tc.want)
			}
		})
	}
}

func TestMatchesAllThreeDimensions(t *testing.T) {
	def := Definition{
		Events: []Event{EventWorkStarted},
		Labels: []string{"epic-child"},
		States: []StateActivation{{State: "in_progress", When: WhenEnter}},
	}
	full := Occasion{
		Event:   EventWorkStarted,
		Labels:  []string{"epic-child"},
		Entered: []string{"in_progress"},
		Exited:  []string{"open"},
	}
	if !def.Matches(full) {
		t.Fatalf("Matches(%+v) = false, want all three dimensions satisfied", full)
	}
	missingTransition := full
	missingTransition.Entered, missingTransition.Exited = nil, nil
	if def.Matches(missingTransition) {
		t.Fatal("Matches() = true with the states dimension unsatisfied")
	}
}

func TestSetMatchingKeepsIDOrder(t *testing.T) {
	set := Set{Definitions: []Definition{
		{ID: "a", Events: []Event{EventShowTicket}},
		{ID: "b", Events: []Event{EventShowBacklog}},
		{ID: "c", Events: []Event{EventShowTicket}},
	}}
	got := set.Matching(Occasion{Event: EventShowTicket})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("Matching() = %+v, want [a c]", got)
	}
}

func TestMatchReasonsNilWhenNotMatching(t *testing.T) {
	def := Definition{Events: []Event{EventShowBacklog}}
	if got := def.MatchReasons(Occasion{Event: EventShowTicket}); got != nil {
		t.Fatalf("MatchReasons() = %v, want nil for a non-matching occasion", got)
	}
}

func TestMatchReasonsNamesEveryDeclaredDimensionThatMatched(t *testing.T) {
	def := Definition{
		Events: []Event{EventWorkFinished},
		Labels: []string{"needs-design", "blocked"},
		States: []StateActivation{{State: "closed", When: WhenEnter}},
	}
	occasion := Occasion{
		Event:   EventWorkFinished,
		Labels:  []string{"needs-design"},
		Entered: []string{"closed"},
	}
	got := def.MatchReasons(occasion)
	want := []string{"event:work_finished", "label:needs-design", "state:closed(enter)"}
	if len(got) != len(want) {
		t.Fatalf("MatchReasons() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MatchReasons() = %v, want %v", got, want)
		}
	}
}

func TestMatchReasonsOnlyListsLabelsThatActuallyOverlap(t *testing.T) {
	def := Definition{Labels: []string{"needs-design", "blocked"}, Events: []Event{EventShowTicket}}
	occasion := Occasion{Event: EventShowTicket, Labels: []string{"other", "blocked"}}
	got := def.MatchReasons(occasion)
	want := []string{"event:show_ticket", "label:blocked"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("MatchReasons() = %v, want %v", got, want)
	}
}
