package lifecycle

import (
	"testing"
	"time"
)

func TestNewStatusStateMirrorsValue(t *testing.T) {
	for _, state := range []State{Open, InProgress, Closed} {
		if got := NewStatus(state, nil, nil).State(); got != state {
			t.Fatalf("State() = %q, want %q", got, state)
		}
	}
}

// TestNewStatusClosedAtBelongsOnlyToClosed pins the sum-type invariant: a close
// timestamp is carried only by the closed variant. The non-closed variants have
// no field to hold it, so ClosedAt() is nil regardless of what gets passed in.
func TestNewStatusClosedAtBelongsOnlyToClosed(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0).UTC()
	if got := NewStatus(Open, &stamp, nil).ClosedAt(); got != nil {
		t.Fatalf("open ClosedAt() = %v, want nil — open carries no close time", got)
	}
	if got := NewStatus(InProgress, &stamp, nil).ClosedAt(); got != nil {
		t.Fatalf("in_progress ClosedAt() = %v, want nil — in_progress carries no close time", got)
	}
	if got := NewStatus(Closed, &stamp, nil).ClosedAt(); got == nil || !got.Equal(stamp) {
		t.Fatalf("closed ClosedAt() = %v, want %v", got, stamp)
	}
}

// TestApplyTargetStateMatrix exercises every (from, action) pair in the 3x4
// matrix. Each cell asserts the post-state matches the action's target state —
// same-state cells (start@InProgress, done@Closed, close@Closed, reopen@Open)
// are no-ops that preserve the receiver. There are no rejection cells: every
// cell here is a legal call.
func TestApplyTargetStateMatrix(t *testing.T) {
	allActions := []ActionName{ActionStart, ActionDone, ActionClose, ActionReopen}
	for _, from := range []State{Open, InProgress, Closed} {
		for _, action := range allActions {
			target, _ := ActionTargetState(action)
			t.Run(string(from)+"_"+string(action), func(t *testing.T) {
				next, err := NewStatus(from, nil, nil).Apply(action, "tester", "")
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

// TestApplySameStateReturnsReceiverUnchanged pins the no-op contract that
// downstream store layers depend on: when the action's target equals the
// current state, Apply returns the receiver verbatim so writeStatusTransition
// can recognize same-state calls without re-deriving them.
func TestApplySameStateReturnsReceiverUnchanged(t *testing.T) {
	closedAt := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		from   StatusPrimitive
		action ActionName
	}{
		{NewStatus(Open, nil, nil), ActionReopen},
		{NewStatus(InProgress, nil, nil), ActionStart},
		{NewStatus(Closed, &closedAt, nil), ActionDone},
		{NewStatus(Closed, &closedAt, nil), ActionClose},
	}
	for _, tc := range cases {
		next, err := tc.from.Apply(tc.action, "tester", "")
		if err != nil {
			t.Fatalf("Apply(%s on %s) error = %v", tc.action, tc.from.State(), err)
		}
		if next.State() != tc.from.State() {
			t.Fatalf("Apply(%s on %s) changed state to %q", tc.action, tc.from.State(), next.State())
		}
		if !timePtrEqual(next.(StatusPrimitive).ClosedAt(), tc.from.ClosedAt()) {
			t.Fatalf("Apply(%s on %s) mutated ClosedAt: got %v, want %v", tc.action, tc.from.State(), next.(StatusPrimitive).ClosedAt(), tc.from.ClosedAt())
		}
	}
}

// TestApplyClosedAtBookkeeping locks in the close-timestamp invariant under the
// sum type: a transition into Closed stamps a timestamp; every transition out of
// Closed lands on a variant that structurally cannot hold one, so the timestamp
// is gone. The last case is the illegal-state removal — under the old flat
// struct, start-on-closed left a stale close time on an in_progress row; the
// in_progress variant has no such field, so it cannot.
func TestApplyClosedAtBookkeeping(t *testing.T) {
	priorClosed := time.Unix(1_700_000_000, 0).UTC()

	openToClosed, err := NewStatus(Open, nil, nil).Apply(ActionClose, "tester", "")
	if err != nil {
		t.Fatalf("Apply(close on open) error = %v", err)
	}
	if closedAt := openToClosed.(StatusPrimitive).ClosedAt(); closedAt == nil {
		t.Fatal("Apply(close on open).ClosedAt() = nil, want stamped")
	}

	inProgressToClosed, err := NewStatus(InProgress, nil, nil).Apply(ActionDone, "tester", "")
	if err != nil {
		t.Fatalf("Apply(done on in_progress) error = %v", err)
	}
	if closedAt := inProgressToClosed.(StatusPrimitive).ClosedAt(); closedAt == nil {
		t.Fatal("Apply(done on in_progress).ClosedAt() = nil, want stamped")
	}

	closedToOpen, err := NewStatus(Closed, &priorClosed, nil).Apply(ActionReopen, "tester", "")
	if err != nil {
		t.Fatalf("Apply(reopen on closed) error = %v", err)
	}
	if closedAt := closedToOpen.(StatusPrimitive).ClosedAt(); closedAt != nil {
		t.Fatalf("Apply(reopen on closed).ClosedAt() = %v, want nil", closedAt)
	}

	closedToInProgress, err := NewStatus(Closed, &priorClosed, nil).Apply(ActionStart, "tester", "")
	if err != nil {
		t.Fatalf("Apply(start on closed) error = %v", err)
	}
	if closedAt := closedToInProgress.(StatusPrimitive).ClosedAt(); closedAt != nil {
		t.Fatalf("Apply(start on closed).ClosedAt() = %v, want nil — in_progress carries no close time", closedAt)
	}
}

// TestNewStatusResolutionBelongsOnlyToClosed is the resolution analogue of the
// closed-at invariant: a resolution is carried only by the closed variant. The
// non-closed variants have no field to hold it, so Resolution() is nil no matter
// what gets passed in — a resolution on a non-closed state is unrepresentable.
func TestNewStatusResolutionBelongsOnlyToClosed(t *testing.T) {
	wontfix := ResolutionWontfix
	if got := NewStatus(Open, nil, &wontfix).Resolution(); got != nil {
		t.Fatalf("open Resolution() = %v, want nil — open carries no resolution", got)
	}
	if got := NewStatus(InProgress, nil, &wontfix).Resolution(); got != nil {
		t.Fatalf("in_progress Resolution() = %v, want nil — in_progress carries no resolution", got)
	}
	got := NewStatus(Closed, nil, &wontfix).Resolution()
	if got == nil || *got != ResolutionWontfix {
		t.Fatalf("closed Resolution() = %v, want %q", got, ResolutionWontfix)
	}
}

// TestApplyReopenClearsResolution pins that transitioning out of closed lands on
// a variant that structurally cannot hold a resolution, so reopening drops it —
// the same bookkeeping the close timestamp gets.
func TestApplyReopenClearsResolution(t *testing.T) {
	duplicate := ResolutionDuplicate
	closed := NewStatus(Closed, nil, &duplicate)
	if got := closed.Resolution(); got == nil || *got != ResolutionDuplicate {
		t.Fatalf("precondition: closed Resolution() = %v, want %q", got, ResolutionDuplicate)
	}
	reopened, err := closed.Apply(ActionReopen, "tester", "")
	if err != nil {
		t.Fatalf("Apply(reopen on closed) error = %v", err)
	}
	if got := reopened.(StatusPrimitive).Resolution(); got != nil {
		t.Fatalf("Apply(reopen on closed).Resolution() = %v, want nil — open carries no resolution", got)
	}
}

func TestParseResolutionRoundTrips(t *testing.T) {
	for _, want := range []Resolution{ResolutionDuplicate, ResolutionSuperseded, ResolutionObsolete, ResolutionWontfix} {
		got, err := ParseResolution(string(want))
		if err != nil {
			t.Fatalf("ParseResolution(%q) error = %v", want, err)
		}
		if got != want {
			t.Fatalf("ParseResolution(%q) = %q, want %q", want, got, want)
		}
	}
	if got, err := ParseResolution("  wontfix  "); err != nil || got != ResolutionWontfix {
		t.Fatalf("ParseResolution(padded) = %q, %v; want %q, nil", got, err, ResolutionWontfix)
	}
}

func TestParseResolutionRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "  ", "done", "fixed", "garbage"} {
		if _, err := ParseResolution(input); err == nil {
			t.Fatalf("ParseResolution(%q) = nil error, want rejection", input)
		}
	}
}

// TestRedirectsToCanonical pins the redirect subset: duplicate and superseded
// close in favor of a canonical ticket and carry a target; obsolete and wontfix
// are terminal. This predicate is the single source for "which resolutions take
// a target" — the `lit close` requirement and the store's redirect-edge write
// both key on it, so it must enumerate exactly the two redirect members.
func TestRedirectsToCanonical(t *testing.T) {
	want := map[Resolution]bool{
		ResolutionDuplicate:  true,
		ResolutionSuperseded: true,
		ResolutionObsolete:   false,
		ResolutionWontfix:    false,
	}
	for res, expect := range want {
		if got := res.RedirectsToCanonical(); got != expect {
			t.Fatalf("%s.RedirectsToCanonical() = %v, want %v", res, got, expect)
		}
	}
}

func TestApplyRejectsParseBypass(t *testing.T) {
	if _, err := NewStatus(Open, nil, nil).Apply(ActionName("bogus"), "tester", ""); err == nil {
		t.Fatal("Apply(bogus) error = nil, want unsupported-action error")
	}
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestAllOfState(t *testing.T) {
	tests := []struct {
		name    string
		members []Lifecycle
		want    State
	}{
		{name: "all open", members: []Lifecycle{NewStatus(Open, nil, nil), NewStatus(Open, nil, nil)}, want: Open},
		{name: "mixed closed", members: []Lifecycle{NewStatus(Open, nil, nil), NewStatus(Closed, nil, nil)}, want: InProgress},
		{name: "in progress", members: []Lifecycle{NewStatus(Open, nil, nil), NewStatus(InProgress, nil, nil)}, want: InProgress},
		{name: "all closed", members: []Lifecycle{NewStatus(Closed, nil, nil), NewStatus(Closed, nil, nil)}, want: Closed},
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
		NewStatus(Open, nil, nil),
		AllOf{Members: []Lifecycle{
			NewStatus(InProgress, nil, nil),
			NewStatus(Closed, nil, nil),
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
	var container Lifecycle = AllOf{Members: []Lifecycle{NewStatus(Open, nil, nil)}}
	if _, ok := container.(Actionable); ok {
		t.Fatal("AllOf satisfies Actionable; containers must not be actionable — their state derives from children")
	}
}

func TestWalkVisitsAllPrimitives(t *testing.T) {
	tree := AllOf{Members: []Lifecycle{
		NewStatus(Open, nil, nil),
		AllOf{Members: []Lifecycle{
			NewStatus(InProgress, nil, nil),
			NewStatus(Closed, nil, nil),
		}},
	}}
	var states []State
	Walk(tree, func(current Lifecycle) bool {
		if status, ok := current.(StatusPrimitive); ok {
			states = append(states, status.State())
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
		NewStatus(Open, nil, nil),
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

func TestParseActionRoundTrips(t *testing.T) {
	all := []ActionName{
		ActionStart, ActionDone, ActionClose, ActionReopen,
		ActionArchive, ActionUnarchive, ActionDelete, ActionRestore,
	}
	for _, a := range all {
		got, err := ParseAction(string(a))
		if err != nil {
			t.Fatalf("ParseAction(%q) unexpected error = %v", a, err)
		}
		if got != a {
			t.Fatalf("ParseAction(%q) = %q, want %q", a, got, a)
		}
	}
}

func TestParseActionRejectsUnknown(t *testing.T) {
	for _, input := range []string{"bogus", "", "transition", "lifecycle"} {
		_, err := ParseAction(input)
		if err == nil {
			t.Fatalf("ParseAction(%q) = nil error, want rejection", input)
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
