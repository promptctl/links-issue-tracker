package workflows

import (
	"reflect"
	"testing"
)

func enter(state string) Crossing { return Crossing{Side: Enter, State: state} }
func exit(state string) Crossing  { return Crossing{Side: Exit, State: state} }

func TestMatchesSingleKeys(t *testing.T) {
	// A start from open: out of open, into in_progress and the plan phase
	// inside it.
	start := Occasion{Moments: []Moment{exit("open"), enter("in_progress"), enter("plan")}}
	// A done from review: out of review and the in_progress around it, into
	// closed.
	done := Occasion{Moments: []Moment{exit("in_progress"), exit("review"), enter("closed")}}
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
			name:     "when: the fired event listed",
			def:      Definition{When: []Moment{EventShowBacklog, EventShowTicket}},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     true,
		},
		{
			name:     "when: the fired event not listed",
			def:      Definition{When: []Moment{EventShowBacklog}},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     false,
		},
		{
			name:     "when: nothing on the occasion",
			def:      Definition{When: []Moment{EventShowBacklog}},
			occasion: Occasion{},
			want:     false,
		},
		{
			name:     "labels: any listed label present",
			def:      Definition{Labels: []string{"needs-design", "blocked"}},
			occasion: Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"other", "blocked"}},
			want:     true,
		},
		{
			name:     "labels: no listed label present",
			def:      Definition{Labels: []string{"needs-design"}},
			occasion: Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"other"}},
			want:     false,
		},
		{
			name:     "labels: no ticket on the occasion",
			def:      Definition{Labels: []string{"needs-design"}},
			occasion: Occasion{Moments: []Moment{EventShowBacklog}},
			want:     false,
		},
		{
			name:     "when: enter matches a state the transition entered",
			def:      Definition{When: []Moment{enter("in_progress")}},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: enter does not match a state the transition exited",
			def:      Definition{When: []Moment{enter("open")}},
			occasion: start,
			want:     false,
		},
		{
			name:     "when: exit matches a state the transition exited",
			def:      Definition{When: []Moment{exit("open")}},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: enter matches a state nested inside another the transition entered",
			def:      Definition{When: []Moment{enter("plan")}},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: exit matches every state a transition left",
			def:      Definition{When: []Moment{exit("review")}},
			occasion: done,
			want:     true,
		},
		{
			name:     "when: a crossing on one side does not satisfy the other",
			def:      Definition{When: []Moment{exit("plan")}},
			occasion: start,
			want:     false,
		},
		{
			name:     "when: custom stage names are just names",
			def:      Definition{When: []Moment{enter("ready-to-pull")}},
			occasion: Occasion{Moments: []Moment{exit("design"), enter("ready-to-pull")}},
			want:     true,
		},
		{
			name:     "when: a read moment never satisfies a crossing",
			def:      Definition{When: []Moment{enter("open")}},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     false,
		},
		{
			name:     "when: a transition that crossed nothing satisfies no crossing",
			def:      Definition{When: []Moment{enter("plan"), exit("plan")}},
			occasion: Occasion{Labels: []string{"needs-design"}},
			want:     false,
		},
		{
			name:     "labels: a transition that crossed nothing still fires a label-only binding",
			def:      Definition{Labels: []string{"needs-design"}},
			occasion: Occasion{Labels: []string{"needs-design"}},
			want:     true,
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
// ticket with a 'need-design' label, it includes this guidance" — when: AND
// labels: must both be satisfied.
func TestMatchesAndAcrossKeys(t *testing.T) {
	def := Definition{
		When:   []Moment{EventShowTicket, enter("closed")},
		Labels: []string{"need-design"},
	}
	cases := []struct {
		name     string
		occasion Occasion
		want     bool
	}{
		{"an event and a label", Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"need-design"}}, true},
		{"a crossing and a label", Occasion{Moments: []Moment{exit("merge"), enter("closed")}, Labels: []string{"need-design"}}, true},
		{"the moment only", Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"other"}}, false},
		{"the label only", Occasion{Moments: []Moment{EventShowBacklog}, Labels: []string{"need-design"}}, false},
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

func TestSetMatchingKeepsIDOrder(t *testing.T) {
	set := Set{Definitions: []Definition{
		{ID: "a", When: []Moment{EventShowTicket}},
		{ID: "b", When: []Moment{EventShowBacklog}},
		{ID: "c", When: []Moment{EventShowTicket}},
	}}
	got := set.Matching(Occasion{Moments: []Moment{EventShowTicket}})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("Matching() = %+v, want [a c]", got)
	}
}

func TestMatchReasonsNilWhenNotMatching(t *testing.T) {
	def := Definition{When: []Moment{EventShowBacklog}}
	if got := def.MatchReasons(Occasion{Moments: []Moment{EventShowTicket}}); got != nil {
		t.Fatalf("MatchReasons() = %v, want nil for a non-matching occasion", got)
	}
}

// The reasons name what matched in the spelling the definition was written
// in, so a dry-run's or a trace's "why" reads back as its `when:` entry.
func TestMatchReasonsNamesWhatMatchedInEachKey(t *testing.T) {
	def := Definition{
		When:   []Moment{exit("plan"), enter("closed"), exit("review")},
		Labels: []string{"needs-design", "blocked"},
	}
	occasion := Occasion{
		Moments: []Moment{exit("in_progress"), exit("review"), enter("closed")},
		Labels:  []string{"blocked"},
	}
	got := def.MatchReasons(occasion)
	want := []string{"when:enter closed", "when:exit review", "label:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchReasons() = %v, want %v", got, want)
	}
}
