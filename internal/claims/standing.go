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
// the lane is finished, or nothing in it was ever started or completed by an
// identifiable checkout. It is the zero state the whole design is built around
// — a repository whose history predates attribution derives nothing but this,
// and behaves exactly as it did before claims existed.
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
// while the lane remains unfinished. It is NOT Unclaimed carrying provenance:
// the holder is still recorded, and who that holder is decides what the lane
// means to whoever is reading it. To the checkout that holds it, staleness is
// evidence it stepped away from work that is still its own, to be handed back
// and resumed. To anyone else it is an offer — "available for takeover, last
// touched by 7f3a three days ago", which is a different offer from "nobody has
// ever worked this", and the agent taking it over needs to know which one it is
// reading. Selection therefore no longer routes around a lane for staleness
// alone; what a stale lane offers past that is the row's business, not this
// variant's.
//
// That last sentence used to read "the lane is unclaimed again — nothing routes
// around it", which was the opposite of what selection did and erased the
// holder this variant exists to carry. Four consumers each decided for
// themselves whether Stale behaved like Held or like Unclaimed, and decided
// differently; the routing gates links-claims-1b0p deleted were the
// compensation for it. Read this variant against an identity and the ambiguity
// is gone — which is why exactly one place in the CLI does that reading, and
// everything else consumes its verdict.
type Stale struct {
	Tenure
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
