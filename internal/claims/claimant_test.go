package claims_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// fieldUpdate is the verb a plain field edit records: none at all. Named so the
// empty string in a case reads as the domain fact it is rather than an omission.
const fieldUpdate = model.ActionName("")

// ticket is the issue ClaimantOf reads. An id and an assignee is the whole of
// it — deliberately unhydrated, because ClaimantOf touches no lifecycle state:
// it reads the assignee column and the events handed to it, and nothing else.
func ticket(assignee string) model.Issue {
	return model.Issue{ID: "T", Assignee: assignee}
}

func TestClaimantOf(t *testing.T) {
	cases := []struct {
		name   string
		issue  model.Issue
		events []model.IssueEvent
		want   claims.Claimant
	}{
		{
			// `lit new --assignee ada`, never started. The assignee column is
			// filled and nobody has taken the ticket; reading a holder off that
			// column alone is the false "claim transferred: ada -> ada" on a
			// ticket's first-ever start.
			name:  "an assignee nobody started is not a holder",
			issue: ticket("ada"),
			want:  claims.Claimant{Assignee: "ada"},
		},
		{
			// An edit is not a transition, so a drive-by field change from
			// another checkout cannot capture a ticket nobody has taken.
			name:   "a field edit establishes nothing",
			issue:  ticket("ada"),
			events: []model.IssueEvent{event("e1", "T", fieldUpdate, ago(time.Hour), streamA)},
			want:   claims.Claimant{Assignee: "ada"},
		},
		{
			// The divergence from Derive, stated directly rather than reached
			// through six layers of machinery. A start recorded where no stream
			// token existed carries no attribution, and the record still says
			// somebody took this. Derive calls the same history Unclaimed
			// because it routes lanes and cannot address a holder it has no
			// token for; announcing a hand-off only needs a predecessor to have
			// existed. Reading the holder off the checkout half alone makes a
			// genuine ada->bob transfer read as nobody, which is the mistake
			// the first attempt at this fix shipped.
			name:   "an establishing event predating attribution still establishes",
			issue:  ticket("ada"),
			events: []model.IssueEvent{event("e1", "T", model.ActionStart, ago(time.Hour), model.Attribution{})},
			want:   claims.Claimant{Established: true, Assignee: "ada"},
		},
		{
			name:   "an establishing event names the checkout that took it",
			issue:  ticket("ada"),
			events: []model.IssueEvent{event("e1", "T", model.ActionStart, ago(time.Hour), streamA)},
			want:   claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claims.ClaimantOf(c.issue, c.events); got != c.want {
				t.Errorf("ClaimantOf() = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestClaimantOfReadsTheNewestEstablisherOfUnorderedEvents pins the property
// that lets the write side ask this question at all. The CLI and both engines
// hand over one issue's events straight out of the store, in whatever order
// they read them — there is no Evidence to have sorted them first — so "newest"
// has to be found rather than taken off the end of the slice.
func TestClaimantOfReadsTheNewestEstablisherOfUnorderedEvents(t *testing.T) {
	events := []model.IssueEvent{
		event("e3", "T", model.ActionStart, ago(time.Hour), streamB),
		event("e1", "T", model.ActionStart, ago(3*time.Hour), streamA),
		// Newer than either start, and an edit — so it must not win.
		event("e2", "T", fieldUpdate, ago(time.Minute), foreign),
	}

	want := claims.Claimant{Established: true, Assignee: "ada", Checkout: streamB}
	if got := claims.ClaimantOf(ticket("ada"), events); got != want {
		t.Errorf("ClaimantOf() = %+v, want %+v", got, want)
	}
}

// TestClaimantOfBreaksASameInstantTieOnID: a coarse clock puts two starts in
// one tick routinely, and recency settles that on the event id. The answer must
// therefore be the same whichever order the store handed the pair over in — an
// engine free to pick would make the claimant an engine detail.
func TestClaimantOfBreaksASameInstantTieOnID(t *testing.T) {
	tick := ago(time.Hour)
	first := event("e1", "T", model.ActionStart, tick, streamA)
	second := event("e2", "T", model.ActionStart, tick, streamB)

	for _, order := range [][]model.IssueEvent{{first, second}, {second, first}} {
		if got := claims.ClaimantOf(ticket("ada"), order); got.Checkout != streamB {
			t.Errorf("ClaimantOf() over %s, %s named checkout %q, want the higher id's %q",
				order[0].ID, order[1].ID, got.Checkout.Stream(), streamB.Stream())
		}
	}
}

func TestHeld(t *testing.T) {
	cases := []struct {
		name     string
		claimant claims.Claimant
		want     bool
	}{
		{"a holder whose start predates attribution", claims.Claimant{Established: true}, true},
		{"a holder the record can address", claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA}, true},
		{"an assignee nobody started", claims.Claimant{Assignee: "ada"}, false},
		{"a ticket with no record at all", claims.Claimant{}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.claimant.Held(); got != c.want {
				t.Errorf("Claimant%+v.Held() = %v, want %v", c.claimant, got, c.want)
			}
		})
	}
}

// everyAction is one sample value per verb in the sealed lifecycle sum, keyed by
// the verb it persists as. After asks the sum exactly one question — is this a
// Start — and answers "ownership does not move" for every other variant through
// a pass-through arm no compiler checks. Driving the cases off this table, and
// the table off model.Actions(), is what makes a ninth variant decide rather
// than inherit. [LAW:no-silent-failure]
var everyAction = map[model.ActionName]model.Action{
	model.ActionStart:     model.Start{Assignee: "bob"},
	model.ActionDone:      model.Done{},
	model.ActionClose:     model.Close{Outcome: model.Wontfix{}},
	model.ActionReopen:    model.Reopen{},
	model.ActionArchive:   model.Archive{},
	model.ActionUnarchive: model.Unarchive{},
	model.ActionDelete:    model.Delete{},
	model.ActionRestore:   model.Restore{},
}

func TestEveryActionHasASample(t *testing.T) {
	for _, name := range model.Actions() {
		sample, covered := everyAction[name]
		if !covered {
			t.Errorf("lifecycle action %q has no sample in everyAction: decide whether performing it moves the claimant, then add it", name)
			continue
		}
		if sample.Name() != name {
			t.Errorf("everyAction[%q] is a %q: a sample must be the variant its key names", name, sample.Name())
		}
	}
	if len(everyAction) != len(model.Actions()) {
		t.Errorf("everyAction holds %d samples, but the sealed set has %d verbs: it names one that no longer exists", len(everyAction), len(model.Actions()))
	}
}

// TestAfterPreservesOwnershipForEveryStatusVerbButStart walks the sum rather
// than a hand-picked couple. Taking work is `start`; every other transition
// drives status on an axis orthogonal to who owns the row, so a `done` that
// quietly rewrote the claimant would hand a lane to whoever happened to close a
// ticket in it — and it is exactly that rewrite the engines' no-op comparison
// would then read as a change worth recording.
func TestAfterPreservesOwnershipForEveryStatusVerbButStart(t *testing.T) {
	heldByA := claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA}

	for _, name := range model.Actions() {
		action, drivesStatus := everyAction[name].(model.StatusAction)
		_, takes := action.(model.Start)
		if !drivesStatus || takes {
			// Retention verbs act on the orthogonal axis and are not
			// StatusActions at all, so After cannot be handed one; start is the
			// one verb that does move the claimant, and has its own cases below.
			continue
		}
		if got := heldByA.After(action, streamB); got != heldByA {
			t.Errorf("After(%q, %s) = %+v, want ownership unchanged at %+v", name, streamB.Stream(), got, heldByA)
		}
	}
}

// TestAfterStartInstallsTheTakingCheckout: the checkout After writes is the one
// performing the action, not the one already on the record. That substitution is
// the fix this package exists for — before it, the taker's identity never
// reached the comparison at all, so a takeover between two checkouts sharing one
// assignee compared equal and recorded nothing.
func TestAfterStartInstallsTheTakingCheckout(t *testing.T) {
	heldByA := claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA}

	// Surrounding space on the way in, because `--assignee` is a flag value a
	// shell can pad and the claimant is compared for equality: an untrimmed
	// " bob " would read as a transfer away from "bob" on every re-run.
	want := claims.Claimant{Established: true, Assignee: "bob", Checkout: streamB}
	if got := heldByA.After(model.Start{Assignee: "  bob  "}, streamB); got != want {
		t.Errorf("After(start, %s) = %+v, want %+v", streamB.Stream(), got, want)
	}
}

// TestAfterStartEstablishesAnUnheldClaimant is the row a comparison on the two
// identity halves alone cannot see: `lit new --assignee ada`, then ada starting
// it from a checkout that resolves no stream token. The assignee does not move
// and the checkout does not move, and it is still the ticket's first-ever take —
// a real claim that owes a write.
func TestAfterStartEstablishesAnUnheldClaimant(t *testing.T) {
	preset := claims.Claimant{Assignee: "ada"}

	got := preset.After(model.Start{Assignee: "ada"}, model.Attribution{})
	want := claims.Claimant{Established: true, Assignee: "ada"}
	if got != want {
		t.Errorf("After(start, absent) = %+v, want %+v", got, want)
	}
	if got == preset {
		t.Error("a first start compared equal to the preset assignee it started from: neither identity half moved, and Established is the fact that did")
	}
}

// TestClaimantEqualityDistinguishesEveryField pins the comparison both storage
// engines' no-op decision and the CLI's transfer notice are now made of — they
// ask "is the claimant this action installs the one already standing" and
// nothing else, so a field the comparison cannot see is a transfer that records
// nothing and exits 0. The checkout row is this PR's bug: two checkouts under
// one assignee, differing only on the half the old write side never read.
func TestClaimantEqualityDistinguishesEveryField(t *testing.T) {
	base := claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA}

	cases := []struct {
		field string
		other claims.Claimant
	}{
		{"established", claims.Claimant{Assignee: "ada", Checkout: streamA}},
		{"assignee", claims.Claimant{Established: true, Assignee: "bob", Checkout: streamA}},
		{"checkout", claims.Claimant{Established: true, Assignee: "ada", Checkout: streamB}},
	}
	for _, c := range cases {
		if base == c.other {
			t.Errorf("claimants differing only in %s compared equal: a hand-off on that half alone would record nothing", c.field)
		}
	}

	if base != (claims.Claimant{Established: true, Assignee: "ada", Checkout: streamA}) {
		t.Error("identical claimants compared unequal: every re-run of a held ticket would record a transfer to itself")
	}
}
