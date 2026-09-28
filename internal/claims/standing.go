package claims

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Standing is what a lane's evidence says about who is working it. The set is
// sealed to the two variants below by the unexported marker, and it is a sum
// rather than one struct with a kind field because the variants genuinely carry
// different data: an unclaimed lane has no holder to describe, and only a held
// one can be contested. A struct with nullable holder fields would make "no
// holder, but here is when they last acted" representable, and every consumer
// would have to know which combinations were real.
// [LAW:types-are-the-program]
//
// There is no third variant for a claim whose evidence has aged out. An
// expired claim is not a weaker claim or the record of one; it is over, and
// the lane is exactly as unclaimed as one nobody ever touched (owner ruling,
// links-claims-y6yz, 2026-09-26). The type says so by having nowhere to put a
// lapsed holder.
type Standing interface{ isStanding() }

// Unclaimed is a lane no checkout holds: finished, never started, or held by a
// claim whose evidence has aged past the freshness window. The three are one
// state, and the derivation collapses them to it deliberately — the readers
// of this value make one decision, whether the lane is somebody's, and nothing
// about how it came to be nobody's bears on that decision. It is the zero
// state the whole design is built around.
type Unclaimed struct{}

// Tenure is the evidence trail behind a holder. Since is when the checkout last
// took the lane — the timestamp of the establishing event that put it there,
// which is what "claim on A#1 moved to 7f3a at 14:02" reports. LastActivity is
// the most recent mutation of any kind the checkout made in the lane, which is
// what freshness is measured against: ordinary working commentary keeps a claim
// alive through a long stretch on one ticket, so the two timestamps drift apart
// on exactly the lanes someone is really working.
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

func (Unclaimed) isStanding() {}
func (Held) isStanding()      {}

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
