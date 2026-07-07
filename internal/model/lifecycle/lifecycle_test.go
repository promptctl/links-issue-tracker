package lifecycle

import (
	"testing"
	"time"
)

func TestNewStatusStateMirrorsValue(t *testing.T) {
	for _, state := range []State{Open, InProgress, Closed} {
		if got := NewStatus(state, nil, nil, nil).State(); got != state {
			t.Fatalf("State() = %q, want %q", got, state)
		}
	}
}

// TestNewStatusClosedAtBelongsOnlyToClosed pins the sum-type invariant: a close
// timestamp is carried only by the closed variant. The non-closed variants have
// no field to hold it, so ClosedAt() is nil regardless of what gets passed in.
func TestNewStatusClosedAtBelongsOnlyToClosed(t *testing.T) {
	stamp := time.Unix(1_700_000_000, 0).UTC()
	if got := NewStatus(Open, &stamp, nil, nil).ClosedAt(); got != nil {
		t.Fatalf("open ClosedAt() = %v, want nil — open carries no close time", got)
	}
	if got := NewStatus(InProgress, &stamp, nil, nil).ClosedAt(); got != nil {
		t.Fatalf("in_progress ClosedAt() = %v, want nil — in_progress carries no close time", got)
	}
	if got := NewStatus(Closed, &stamp, nil, nil).ClosedAt(); got == nil || !got.Equal(stamp) {
		t.Fatalf("closed ClosedAt() = %v, want %v", got, stamp)
	}
}

// TestApplyTargetStateMatrix exercises every (from, action) pair in the 3x4
// matrix. Each cell asserts the post-state matches the action's target state —
// same-state cells (start@InProgress, done@Closed, close@Closed, reopen@Open)
// are no-ops that preserve the receiver. There are no rejection cells: every
// cell here is a legal call, and Apply cannot fail by construction.
func TestApplyTargetStateMatrix(t *testing.T) {
	allActions := []StatusAction{Start{}, Done{}, Close{Outcome: Wontfix{}}, Reopen{}}
	for _, from := range []State{Open, InProgress, Closed} {
		for _, action := range allActions {
			target := action.Target()
			t.Run(string(from)+"_"+string(action.Name()), func(t *testing.T) {
				next := NewStatus(from, nil, nil, nil).Apply(action)
				if got := next.State(); got != target {
					t.Fatalf("Apply(%s on %s).State() = %q, want %q", action.Name(), from, got, target)
				}
			})
		}
	}
}

// TestApplySameStateReturnsReceiverUnchanged pins the no-op contract that
// downstream store layers depend on: when the action's target equals the
// current state, Apply returns the receiver verbatim — including a re-close,
// whose new outcome is deliberately NOT rewritten over the existing one — so
// the store's plan can recognize same-state calls without re-deriving them.
func TestApplySameStateReturnsReceiverUnchanged(t *testing.T) {
	closedAt := time.Unix(1_700_000_000, 0).UTC()
	obsolete := ResolutionObsolete
	cases := []struct {
		from   StatusPrimitive
		action StatusAction
	}{
		{NewStatus(Open, nil, nil, nil), Reopen{}},
		{NewStatus(InProgress, nil, nil, nil), Start{Assignee: "someone"}},
		{NewStatus(Closed, &closedAt, &obsolete, nil), Done{}},
		{NewStatus(Closed, &closedAt, &obsolete, nil), Close{Outcome: Wontfix{}}},
	}
	for _, tc := range cases {
		next := tc.from.Apply(tc.action)
		if next.State() != tc.from.State() {
			t.Fatalf("Apply(%s on %s) changed state to %q", tc.action.Name(), tc.from.State(), next.State())
		}
		if !timePtrEqual(next.(StatusPrimitive).ClosedAt(), tc.from.ClosedAt()) {
			t.Fatalf("Apply(%s on %s) mutated ClosedAt: got %v, want %v", tc.action.Name(), tc.from.State(), next.(StatusPrimitive).ClosedAt(), tc.from.ClosedAt())
		}
		if !resolutionPtrEqual(next.(StatusPrimitive).Resolution(), tc.from.Resolution()) {
			t.Fatalf("Apply(%s on %s) mutated Resolution: got %v, want %v", tc.action.Name(), tc.from.State(), next.(StatusPrimitive).Resolution(), tc.from.Resolution())
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

	openToClosed := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Wontfix{}})
	if closedAt := openToClosed.(StatusPrimitive).ClosedAt(); closedAt == nil {
		t.Fatal("Apply(close on open).ClosedAt() = nil, want stamped")
	}

	inProgressToClosed := NewStatus(InProgress, nil, nil, nil).Apply(Done{})
	if closedAt := inProgressToClosed.(StatusPrimitive).ClosedAt(); closedAt == nil {
		t.Fatal("Apply(done on in_progress).ClosedAt() = nil, want stamped")
	}

	closedToOpen := NewStatus(Closed, &priorClosed, nil, nil).Apply(Reopen{})
	if closedAt := closedToOpen.(StatusPrimitive).ClosedAt(); closedAt != nil {
		t.Fatalf("Apply(reopen on closed).ClosedAt() = %v, want nil", closedAt)
	}

	closedToInProgress := NewStatus(Closed, &priorClosed, nil, nil).Apply(Start{})
	if closedAt := closedToInProgress.(StatusPrimitive).ClosedAt(); closedAt != nil {
		t.Fatalf("Apply(start on closed).ClosedAt() = %v, want nil — in_progress carries no close time", closedAt)
	}
}

// TestApplyCloseCarriesOutcomeThroughMachine pins the payload threading: a
// close's outcome arrives WITH the close, landing on the closed variant as its
// resolution, instead of being re-attached after the state machine. Done is
// the neutral success close and records none.
func TestApplyCloseCarriesOutcomeThroughMachine(t *testing.T) {
	closed := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Duplicate{Of: "links-abc1"}})
	got := closed.(StatusPrimitive).Resolution()
	if got == nil || *got != ResolutionDuplicate {
		t.Fatalf("Apply(close duplicate).Resolution() = %v, want %q", got, ResolutionDuplicate)
	}
	if target := closed.(StatusPrimitive).RedirectTarget(); target == nil || *target != "links-abc1" {
		t.Fatalf("Apply(close duplicate).RedirectTarget() = %v, want links-abc1 — the outcome's target travels through the machine", target)
	}

	superseded := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Superseded{By: "links-new1"}})
	if target := superseded.(StatusPrimitive).RedirectTarget(); target == nil || *target != "links-new1" {
		t.Fatalf("Apply(close superseded).RedirectTarget() = %v, want links-new1", target)
	}

	wontfix := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Wontfix{}})
	if target := wontfix.(StatusPrimitive).RedirectTarget(); target != nil {
		t.Fatalf("Apply(close wontfix).RedirectTarget() = %v, want nil — terminal outcomes carry no target", target)
	}

	done := NewStatus(InProgress, nil, nil, nil).Apply(Done{})
	if got := done.(StatusPrimitive).Resolution(); got != nil {
		t.Fatalf("Apply(done).Resolution() = %v, want nil — done records no resolution", got)
	}
	if target := done.(StatusPrimitive).RedirectTarget(); target != nil {
		t.Fatalf("Apply(done).RedirectTarget() = %v, want nil", target)
	}
}

// TestApplyCloseNormalizesBlankRedirectTarget pins the empty-target
// normalization: a redirecting close whose target is blank lands as absent
// (nil), the one representation the store's validation floor keys on.
func TestApplyCloseNormalizesBlankRedirectTarget(t *testing.T) {
	closed := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Duplicate{Of: "   "}})
	if target := closed.(StatusPrimitive).RedirectTarget(); target != nil {
		t.Fatalf("Apply(close duplicate of blank).RedirectTarget() = %v, want nil", target)
	}
	padded := NewStatus(Open, nil, nil, nil).Apply(Close{Outcome: Duplicate{Of: "  links-abc1  "}})
	if target := padded.(StatusPrimitive).RedirectTarget(); target == nil || *target != "links-abc1" {
		t.Fatalf("Apply(close duplicate of padded).RedirectTarget() = %v, want trimmed links-abc1", target)
	}
}

// TestNewStatusResolutionBelongsOnlyToClosed is the resolution analogue of the
// closed-at invariant: a resolution is carried only by the closed variant. The
// non-closed variants have no field to hold it, so Resolution() is nil no matter
// what gets passed in — a resolution on a non-closed state is unrepresentable.
func TestNewStatusResolutionBelongsOnlyToClosed(t *testing.T) {
	wontfix := ResolutionWontfix
	if got := NewStatus(Open, nil, &wontfix, nil).Resolution(); got != nil {
		t.Fatalf("open Resolution() = %v, want nil — open carries no resolution", got)
	}
	if got := NewStatus(InProgress, nil, &wontfix, nil).Resolution(); got != nil {
		t.Fatalf("in_progress Resolution() = %v, want nil — in_progress carries no resolution", got)
	}
	got := NewStatus(Closed, nil, &wontfix, nil).Resolution()
	if got == nil || *got != ResolutionWontfix {
		t.Fatalf("closed Resolution() = %v, want %q", got, ResolutionWontfix)
	}
}

// TestApplyReopenClearsResolution pins that transitioning out of closed lands on
// a variant that structurally cannot hold a resolution, so reopening drops it —
// the same bookkeeping the close timestamp gets. The redirect target is the
// resolution's payload and drops with it.
func TestApplyReopenClearsResolution(t *testing.T) {
	duplicate := ResolutionDuplicate
	target := "links-abc1"
	closed := NewStatus(Closed, nil, &duplicate, &target)
	if got := closed.Resolution(); got == nil || *got != ResolutionDuplicate {
		t.Fatalf("precondition: closed Resolution() = %v, want %q", got, ResolutionDuplicate)
	}
	if got := closed.RedirectTarget(); got == nil || *got != target {
		t.Fatalf("precondition: closed RedirectTarget() = %v, want %q", got, target)
	}
	reopened := closed.Apply(Reopen{})
	if got := reopened.(StatusPrimitive).Resolution(); got != nil {
		t.Fatalf("Apply(reopen on closed).Resolution() = %v, want nil — open carries no resolution", got)
	}
	if got := reopened.(StatusPrimitive).RedirectTarget(); got != nil {
		t.Fatalf("Apply(reopen on closed).RedirectTarget() = %v, want nil — open carries no redirect", got)
	}
}

// TestNewStatusRedirectTargetRequiresRedirectingResolution pins the boundary
// invariant: the redirect target is the redirecting resolution's payload, so
// NewStatus attaches it only beside duplicate/superseded. Any other pairing —
// terminal resolution, no resolution, non-closed state — is dropped at the
// boundary, making "a redirect without a redirecting close" unrepresentable
// in a hydrated leaf.
func TestNewStatusRedirectTargetRequiresRedirectingResolution(t *testing.T) {
	target := "links-abc1"
	duplicate := ResolutionDuplicate
	superseded := ResolutionSuperseded
	wontfix := ResolutionWontfix

	if got := NewStatus(Closed, nil, &duplicate, &target).RedirectTarget(); got == nil || *got != target {
		t.Fatalf("closed duplicate RedirectTarget() = %v, want %q", got, target)
	}
	if got := NewStatus(Closed, nil, &superseded, &target).RedirectTarget(); got == nil || *got != target {
		t.Fatalf("closed superseded RedirectTarget() = %v, want %q", got, target)
	}
	if got := NewStatus(Closed, nil, &wontfix, &target).RedirectTarget(); got != nil {
		t.Fatalf("closed wontfix RedirectTarget() = %v, want nil — terminal resolutions carry no target", got)
	}
	if got := NewStatus(Closed, nil, nil, &target).RedirectTarget(); got != nil {
		t.Fatalf("closed no-resolution RedirectTarget() = %v, want nil", got)
	}
	if got := NewStatus(Open, nil, &duplicate, &target).RedirectTarget(); got != nil {
		t.Fatalf("open RedirectTarget() = %v, want nil — open carries no redirect", got)
	}
	if got := NewStatus(InProgress, nil, &duplicate, &target).RedirectTarget(); got != nil {
		t.Fatalf("in_progress RedirectTarget() = %v, want nil", got)
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

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func resolutionPtrEqual(a, b *Resolution) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestAllOfState(t *testing.T) {
	tests := []struct {
		name    string
		members []Lifecycle
		want    State
	}{
		{name: "all open", members: []Lifecycle{NewStatus(Open, nil, nil, nil), NewStatus(Open, nil, nil, nil)}, want: Open},
		{name: "mixed closed", members: []Lifecycle{NewStatus(Open, nil, nil, nil), NewStatus(Closed, nil, nil, nil)}, want: InProgress},
		{name: "in progress", members: []Lifecycle{NewStatus(Open, nil, nil, nil), NewStatus(InProgress, nil, nil, nil)}, want: InProgress},
		{name: "all closed", members: []Lifecycle{NewStatus(Closed, nil, nil, nil), NewStatus(Closed, nil, nil, nil)}, want: Closed},
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
		NewStatus(Open, nil, nil, nil),
		AllOf{Members: []Lifecycle{
			NewStatus(InProgress, nil, nil, nil),
			NewStatus(Closed, nil, nil, nil),
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
	var container Lifecycle = AllOf{Members: []Lifecycle{NewStatus(Open, nil, nil, nil)}}
	if _, ok := container.(Actionable); ok {
		t.Fatal("AllOf satisfies Actionable; containers must not be actionable — their state derives from children")
	}
}

func TestWalkVisitsAllPrimitives(t *testing.T) {
	tree := AllOf{Members: []Lifecycle{
		NewStatus(Open, nil, nil, nil),
		AllOf{Members: []Lifecycle{
			NewStatus(InProgress, nil, nil, nil),
			NewStatus(Closed, nil, nil, nil),
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
		NewStatus(Open, nil, nil, nil),
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

// TestActionSumEncodings pins the persistence encodings of the sealed action
// sum: each variant's Name (the events-table verb) and, for the status subset,
// its Target — the one forward action→state map. The retention variants are
// deliberately absent from the status table: they act on the Retention axis
// and are not StatusActions.
func TestActionSumEncodings(t *testing.T) {
	names := map[ActionName]Action{
		ActionStart:     Start{},
		ActionDone:      Done{},
		ActionClose:     Close{Outcome: Wontfix{}},
		ActionReopen:    Reopen{},
		ActionArchive:   Archive{},
		ActionUnarchive: Unarchive{},
		ActionDelete:    Delete{},
		ActionRestore:   Restore{},
	}
	for want, action := range names {
		if got := action.Name(); got != want {
			t.Fatalf("%T.Name() = %q, want %q", action, got, want)
		}
	}
	targets := map[State]StatusAction{
		InProgress: Start{},
		Open:       Reopen{},
	}
	for want, action := range targets {
		if got := action.Target(); got != want {
			t.Fatalf("%T.Target() = %q, want %q", action, got, want)
		}
	}
	// Done and Close both target Closed — the two closing actions.
	if (Done{}).Target() != Closed || (Close{Outcome: Wontfix{}}).Target() != Closed {
		t.Fatal("Done/Close must target Closed")
	}
	for _, retention := range []Action{Archive{}, Unarchive{}, Delete{}, Restore{}} {
		if _, ok := retention.(StatusAction); ok {
			t.Fatalf("%T satisfies StatusAction; retention actions have no status target", retention)
		}
	}
}

// TestOutcomeEncodingsAgreeWithRedirectPredicate pins the two projections of
// "which closes redirect" against each other: the close stamps the target
// structurally off the outcome variant, while the NewStatus hydration boundary
// (which only has the persisted resolution string) admits a target via
// Resolution.RedirectsToCanonical. A variant carries a target field exactly
// when its resolution redirects, so a target the close stamps can never be
// dropped at rehydration.
func TestOutcomeEncodingsAgreeWithRedirectPredicate(t *testing.T) {
	carriesTarget := map[Outcome]bool{
		Duplicate{Of: "links-abc1"}:  true,
		Superseded{By: "links-abc1"}: true,
		Obsolete{}:                   false,
		Wontfix{}:                    false,
	}
	wantResolution := map[Resolution]Outcome{
		ResolutionDuplicate:  Duplicate{Of: "links-abc1"},
		ResolutionSuperseded: Superseded{By: "links-abc1"},
		ResolutionObsolete:   Obsolete{},
		ResolutionWontfix:    Wontfix{},
	}
	for resolution, outcome := range wantResolution {
		if got := outcome.Resolution(); got != resolution {
			t.Fatalf("%T.Resolution() = %q, want %q", outcome, got, resolution)
		}
	}
	for outcome, hasTarget := range carriesTarget {
		if got := outcome.Resolution().RedirectsToCanonical(); got != hasTarget {
			t.Fatalf("%T carries a target = %v but %s.RedirectsToCanonical() = %v — the sum's shape and the read-side predicate drifted", outcome, hasTarget, outcome.Resolution(), got)
		}
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
