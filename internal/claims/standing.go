package claims

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Standing is what a lane's evidence says about who is working it. The set is
// sealed to the three variants below by the unexported marker, and it is a sum
// rather than one struct with a kind field because the variants genuinely carry
// different data: an unclaimed lane has no holder to describe, and only a held
// one can be contested. A struct with nullable holder fields would make "no
// holder, but here is when they last acted" representable, and every consumer
// would have to know which combinations were real.
// [LAW:types-are-the-program]
type Standing interface{ isStanding() }

// Unclaimed is a lane no checkout holds and none is recorded as having held:
// the lane is finished, or nothing in it was ever started or completed at all.
// It is the zero state the whole design is built around.
//
// Note the second condition is about the ABSENCE OF AN EVENT, not the absence
// of an identity. An establishing event that names no checkout still produces a
// holder — the public checkout — so a repository whose history predates
// attribution derives Stale here, not Unclaimed: somebody worked this lane and
// walked away, which is a different offer from "nobody has ever worked this"
// and is exactly the distinction Stale exists to carry.
type Unclaimed struct{}

// Tenure is the evidence trail behind a holder, shared by the two variants that
// have one. Since is when the checkout last took the lane — the timestamp of the
// establishing event that put it there, which is what "claim on A#1 moved to
// 7f3a at 14:02" reports. LastActivity is the most recent mutation of any kind
// the checkout made in the lane, which is what freshness is measured against:
// ordinary working commentary keeps a claim alive through a long stretch on one
// ticket, so the two timestamps drift apart on exactly the lanes someone is
// really working.
type Tenure struct {
	By           model.Attribution
	Since        time.Time
	LastActivity time.Time
}

// Held is a lane a checkout holds right now: all four legs of the predicate
// pass. Contested lists the other checkouts that also have live evidence here —
// an offline race, or a takeover the previous holder has not yet seen. It is an
// annotation and not a state, so routing is unaffected by it: the holder is
// still the holder, and the list exists to be surfaced to both sides. Empty is
// the ordinary case, which is why contest is a slice and not a flag beside a
// separate list. [LAW:dataflow-not-control-flow]
type Held struct {
	Tenure
	Contested []model.Attribution
}

// Stale is a lane whose holder's evidence has aged past the freshness window
// while the lane remains unfinished. It is not a claim. It is the record that
// a claim existed — a line on the manifest, not a passenger — and it is kept
// because the record is an offer with provenance: "available, last touched by
// 7f3a three days ago" is a different offer from "nobody has ever worked
// this", and the agent taking the lane needs to know which one it is reading.
// Who the lapsed holder was changes what the offer SAYS and never who may
// take it: the checkout that let the lane lapse reads it exactly as any other
// checkout does, and gets it back by rank, not by residue.
//
// This variant once read "to the checkout that holds it, staleness is evidence
// it stepped away from work that is still its own, to be handed back and
// resumed". Under that reading a checkout that finished one ticket of an epic
// and walked away was routed back to that epic ahead of the entire backlog
// for as long as the epic stayed open — with one checkout in the repository,
// forever, since nothing it could do released a claim that no longer existed
// (links-claims-em7h). Exactly one place in the CLI reads this variant against
// an identity, and everything else consumes its verdict.
//
// Holder is what this machine can still see of the checkout named by Tenure.By,
// and it is on this variant because expiry is exactly where it starts to
// matter: a held lane routes around its holder whatever the disk says, while a
// stale one is an offer whose terms this value sets. An expired clock says only
// that the lane went quiet; it never said the holder left, and reporting one as
// the other is what let a locked worktree on an open PR read as free work
// (links-claims-2wk2).
//
// Gone is unreachable here, and structurally so rather than by convention: leg
// 4 voids a proven-absent checkout's events before leg 2 looks for a holder, so
// a holder that reaches leg 3 is one whose evidence survived that filter. A
// lane whose only holder was gone derives Unclaimed, which is the standing that
// carries "somebody walked away" — and it is the reason this field's absence
// was invisible for so long: the abandoned case really did exist, just never in
// this variant.
type Stale struct {
	Tenure
	Holder Presence
}

func (Unclaimed) isStanding() {}
func (Held) isStanding()      {}
func (Stale) isStanding()     {}

// Standings is every lane's derived standing. Absence and Unclaimed are one
// fact, so Of resolves both to the same value: a caller that looks up a lane the
// derivation never saw gets the zero state rather than a nil interface that
// panics the moment it reaches a type switch. Reading through Of is what makes
// the map total. [LAW:no-silent-failure]
type Standings map[model.LaneID]Standing

func (s Standings) Of(lane model.LaneID) Standing {
	if standing, ok := s[lane]; ok {
		return standing
	}
	return Unclaimed{}
}
