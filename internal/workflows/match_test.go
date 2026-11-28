package workflows

import (
	"reflect"
	"testing"
)

func TestMatchesSingleKeys(t *testing.T) {
	// A start from open: out of open, into in_progress and the plan phase
	// inside it.
	start := Occasion{Moments: []Moment{exiting("open"), entering("in_progress"), entering("plan")}}
	// A done from review: out of review and the in_progress around it, into
	// closed.
	done := Occasion{Moments: []Moment{exiting("review"), exiting("in_progress"), entering("closed")}}
	cases := []struct {
		name     string
		def      Definition
		occasion Occasion
		want     bool
	}{
		{
			name: "never fires, even on an empty occasion",
			def:  Definition{ID: "never"},
			want: false,
		},
		{
			name:     "when: the fired event listed",
			def:      Definition{Activation: firesOn([]Moment{EventShowBacklog, EventShowTicket}, nil)},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     true,
		},
		{
			name:     "when: the fired event not listed",
			def:      Definition{Activation: firesOn([]Moment{EventShowBacklog}, nil)},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     false,
		},
		{
			name:     "when: nothing on the occasion",
			def:      Definition{Activation: firesOn([]Moment{EventShowBacklog}, nil)},
			occasion: Occasion{},
			want:     false,
		},
		{
			name:     "labels: any listed label present",
			def:      Definition{Activation: firesOn(nil, []string{"needs-design", "blocked"})},
			occasion: Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"other", "blocked"}},
			want:     true,
		},
		{
			name:     "labels: no listed label present",
			def:      Definition{Activation: firesOn(nil, []string{"needs-design"})},
			occasion: Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"other"}},
			want:     false,
		},
		{
			name:     "labels: no ticket on the occasion",
			def:      Definition{Activation: firesOn(nil, []string{"needs-design"})},
			occasion: Occasion{Moments: []Moment{EventShowBacklog}},
			want:     false,
		},
		{
			name:     "when: enter matches a state the transition entered",
			def:      Definition{Activation: firesOn([]Moment{entering("in_progress")}, nil)},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: enter does not match a state the transition exited",
			def:      Definition{Activation: firesOn([]Moment{entering("open")}, nil)},
			occasion: start,
			want:     false,
		},
		{
			name:     "when: exit matches a state the transition exited",
			def:      Definition{Activation: firesOn([]Moment{exiting("open")}, nil)},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: enter matches a state nested inside another the transition entered",
			def:      Definition{Activation: firesOn([]Moment{entering("plan")}, nil)},
			occasion: start,
			want:     true,
		},
		{
			name:     "when: exit matches every state a transition left",
			def:      Definition{Activation: firesOn([]Moment{exiting("review")}, nil)},
			occasion: done,
			want:     true,
		},
		{
			name:     "when: a crossing on one side does not satisfy the other",
			def:      Definition{Activation: firesOn([]Moment{exiting("plan")}, nil)},
			occasion: start,
			want:     false,
		},
		{
			name:     "when: a read moment never satisfies a crossing",
			def:      Definition{Activation: firesOn([]Moment{entering("open")}, nil)},
			occasion: Occasion{Moments: []Moment{EventShowTicket}},
			want:     false,
		},
		{
			name:     "when: a transition that crossed nothing satisfies no crossing",
			def:      Definition{Activation: firesOn([]Moment{entering("plan"), exiting("plan")}, nil)},
			occasion: Occasion{Labels: []string{"needs-design"}},
			want:     false,
		},
		{
			name:     "labels: a transition that crossed nothing still fires a label-only binding",
			def:      Definition{Activation: firesOn(nil, []string{"needs-design"})},
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
	def := Definition{Activation: firesOn([]Moment{EventShowTicket, entering("closed")}, []string{"need-design"})}
	cases := []struct {
		name     string
		occasion Occasion
		want     bool
	}{
		{"an event and a label", Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"need-design"}}, true},
		{"a crossing and a label", Occasion{Moments: []Moment{exiting("merge"), entering("closed")}, Labels: []string{"need-design"}}, true},
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
		{ID: "a", Activation: firesOn([]Moment{EventShowTicket}, nil)},
		{ID: "b", Activation: firesOn([]Moment{EventShowBacklog}, nil)},
		{ID: "c", Activation: firesOn([]Moment{EventShowTicket}, nil)},
	}}
	got := set.Matching(Occasion{Moments: []Moment{EventShowTicket}})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("Matching() = %+v, want [a c]", got)
	}
}

func TestMatchReasonsNilWhenNotMatching(t *testing.T) {
	def := Definition{Activation: firesOn([]Moment{EventShowBacklog}, nil)}
	if got := def.MatchReasons(Occasion{Moments: []Moment{EventShowTicket}}); got != nil {
		t.Fatalf("MatchReasons() = %v, want nil for a non-matching occasion", got)
	}
}

// The reasons name what matched in the spelling the definition was written
// in, so a dry-run's or a trace's "why" reads back as its `when:` entry.
func TestMatchReasonsNamesWhatMatchedInEachKey(t *testing.T) {
	def := Definition{Activation: firesOn([]Moment{exiting("plan"), entering("closed"), exiting("review")}, []string{"needs-design", "blocked"})}
	occasion := Occasion{
		Moments: []Moment{exiting("review"), exiting("in_progress"), entering("closed")},
		Labels:  []string{"blocked"},
	}
	got := def.MatchReasons(occasion)
	want := []string{"when:enter closed", "when:exit review", "label:blocked"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MatchReasons() = %v, want %v", got, want)
	}
}
