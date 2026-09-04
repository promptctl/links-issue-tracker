package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// NextOutcome is what routeNext concluded, sealed to the cases the routing
// precedence in design-docs/work-claims.md — as amended by the epic-major
// GRANULARITY RULING on links-claims-1ihf.5 — can produce. A bare
// annotation.AnnotatedIssue cannot say WHICH case picked it, and the
// exhaustion case in particular must never be confused with an ordinary
// pick: it is an answer-shaped void unless the type forbids the confusion.
// [LAW:types-are-the-program] [LAW:parse-dont-validate]
//
// The served variants split on the one question the renderer must answer
// before handing a row over: does taking it establish a claim this checkout
// did not already hold? Every announcement follows from that.
type NextOutcome interface{ isNextOutcome() }

// ServedFromClaim is a ready ticket in a lane this checkout already holds —
// routing step 1. No new claim is established, so nothing is announced.
type ServedFromClaim struct{ Row annotation.AnnotatedIssue }

// ResumedOwnWork is a ticket already in flight in a lane this checkout holds,
// handed back to its holder — routing step 1 for work that is started rather
// than startable. Nothing is claimed and nothing is begun, so it announces
// "resuming" and not "starting". Being reachable at all is this ticket's
// headline: while routing gated servability on model.StateOpen, an in_progress
// row was servable to nobody, which hid every orphan (links-claims-1b0p, G2)
// and — with no staleness involved anywhere — the very ticket the checkout was
// working at that moment (N8).
type ResumedOwnWork struct{ Row annotation.AnnotatedIssue }

// ServedFromEpicLane is a ready ticket in a different lane of the same epic
// this checkout already holds a lane in — the GRANULARITY RULING's new step 2,
// epic-major before global. Starting it establishes a fresh claim on Lane,
// which is why it carries the label to announce.
type ServedFromEpicLane struct {
	Row  annotation.AnnotatedIssue
	Epic string
	Lane string
}

// ServedFromNewLane is a ready ticket in a lane this checkout does NOT hold,
// so starting it establishes a claim — announced by Lane exactly as the
// design's example spells it: "starting B.1 claims B#1". Two steps produce it:
// the global pool (step 4) and the on-path dependency gating one of our own
// blocked rows (step 1b). The dependency used to come back as ServedFromClaim,
// whose contract is that no claim is established and nothing is announced,
// which left the one pick an agent is least likely to predict as the only
// silent one (links-claims-1b0p, N3).
//
// A takeover arrives here too — of a stale lane, or of work abandoned in
// flight — and says so in its own announcement rather than reading as a fresh
// start. Whose it was stays with the row: printNextSummary prints the
// displaced holder's claim line beneath it when there is a holder to name.
type ServedFromNewLane struct {
	Row  annotation.AnnotatedIssue
	Lane string
}

// Exhausted is the checkout's own claimed epic(s) having open work with none
// of it reachable: not the held lanes, not an on-path dependency, not another
// lane of the same epic. Routing step 3 — loud and diagnostic, never a
// silent hop to a leaf outside the epic. Blocked names the open-dependency
// ids gating that work, if any (an in-progress-only lane names none).
type Exhausted struct {
	Epics   []string
	Blocked []string
}

// NoWork is the truly empty backlog: nothing ready anywhere, claimed or not
// — the pre-claims "no ready work" case, unchanged.
type NoWork struct{}

func (ServedFromClaim) isNextOutcome()    {}
func (ResumedOwnWork) isNextOutcome()     {}
func (ServedFromEpicLane) isNextOutcome() {}
func (ServedFromNewLane) isNextOutcome()  {}
func (Exhausted) isNextOutcome()          {}
func (NoWork) isNextOutcome()             {}

// capacity is the single answer to "may this checkout take this row, and as
// what?" — the eligibility verdict the owner ruling on links-claims-1b0p
// demands. It replaces three independent booleans (heldBySelf, isReadyRow,
// isUnclaimed) consulted at three points of one walk, each re-deriving a piece
// of this question and disagreeing about what a stale claim means. With the
// answer in one place a fourth capacity costs one arm of one switch instead of
// an edit to three predicates and the loop.
// [LAW:types-are-the-program] [LAW:single-enforcer]
type capacity int

const (
	// routeAround: not this checkout's to take right now.
	routeAround capacity = iota
	// serveWork: startable work this checkout may claim or already holds.
	serveWork
	// resumeWork: work already underway that belongs to this checkout. Handed
	// back rather than started fresh — staleness of your own lane is evidence
	// you stepped away, never evidence the work stopped being yours.
	resumeWork
	// takeoverWork: something is being displaced — a stale foreign claim, or
	// in-flight work abandoned in a lane nobody holds. Announced, never silent.
	takeoverWork
)

// capacityFor derives the verdict from the only three facts that bear on it:
// the row's lifecycle state, its lane's relation to this checkout, and whether
// the row is orphaned. Pure, total, and the one consumer of IsOrphaned() in
// routing — the fact was computed on every gather and discarded here before
// (links-claims-1b0p, F1).
//
// Two rules cover the whole table. Our own lane: work in flight is ours to
// resume — staleness there is evidence we stepped away, never that the work
// stopped being ours — and startable work is ours to serve. Any other lane: we
// may take what is takeable, and it counts as a takeover exactly when
// something is being displaced, whether that is a stale holder or an in-flight
// ticket somebody walked away from.
//
// Takeability is where the state asymmetry lives. An OPEN row is takeable when
// nothing blocks it. An IN-PROGRESS row is somebody's work in flight and stays
// untouchable — whosever lane it sits in — until it is orphaned, the orphan
// annotation being the proof that the claim asserting somebody is working it
// is self-refuting.
func capacityFor(row annotation.AnnotatedIssue, standing claims.Standing, self model.Attribution) capacity {
	readiness := ClassifyReadiness(row.Annotations)
	relation := relationOf(standing, self)
	started := row.State() == model.StateInProgress
	if relation == laneOurs {
		if started {
			return resumeWork
		}
		if readiness.IsReady() {
			return serveWork
		}
		return routeAround
	}
	takeable := (started && readiness.IsOrphaned()) || (!started && readiness.IsReady())
	switch {
	case !takeable, relation == laneHeldForeign:
		return routeAround
	case started, relation == laneStaleForeign:
		return takeoverWork
	}
	return serveWork
}

// ownScope reads this checkout's held lanes, and the epics they sit in,
// straight from the standings.
//
// [LAW:one-source-of-truth] It reads standings and NOT the gathered rows. The
// rows are already narrowed by --type/--labels/--assignee, so deriving
// ownership from them let any display filter empty this set and drop the whole
// self-aware branch — a checkout with a perfectly fresh claim silently hopping
// epics because it asked for one issue type (links-claims-1b0p, N1). Ownership
// is a fact about the workspace; a display filter must not be able to change it.
//
// An absent self needs no guard here: claims.Derive refuses to build a Held or
// Stale standing from an unattributed establisher (derive.go), so no standing
// carries the zero Attribution and relationOf can never call a lane ours by
// matching absence against absence.
func ownScope(standings claims.Standings, self model.Attribution) (map[model.LaneID]bool, map[string]bool) {
	lanes := map[model.LaneID]bool{}
	epics := map[string]bool{}
	for lane, standing := range standings {
		if relationOf(standing, self) != laneOurs {
			continue
		}
		lanes[lane] = true
		if lane.Epic() != "" {
			epics[lane.Epic()] = true
		}
	}
	return lanes, epics
}

// routeNext applies the claim-aware selection precedence over an already
// gathered, already ordered workable set. Pure — standings and self are
// values the caller derived once from the store and the local filesystem, so
// every branch of the precedence is testable without either.
// [LAW:effects-at-boundaries]
//
// Precedence: this checkout's own lanes first, startable work and work of ours
// already underway alike; then an on-path external dependency that gates one of
// them; then the rest of its epic's lanes (the GRANULARITY RULING's epic-major
// amendment); then a loud exhaustion diagnostic if the epic has open work but
// none of it is reachable; then the global pool. A checkout with no lanes of its
// own starts straight at the global pool — unfocus is the zero state, not a hop
// through the earlier steps.
//
// [LAW:dataflow-not-control-flow] Every step walks the same rows in the same
// composite-rank order and asks capacityFor the same question; a step differs
// only in which lanes it admits and which verdicts it accepts. No step decides
// eligibility on its own.
func routeNext(rows []annotation.AnnotatedIssue, details map[string]storage.IssueRelations, standings claims.Standings, self model.Attribution) NextOutcome {
	laneOf := func(row annotation.AnnotatedIssue) model.LaneID {
		return model.LaneOf(row.Issue, details[row.ID].Parent)
	}
	verdict := func(row annotation.AnnotatedIssue) capacity {
		return capacityFor(row, standings.Of(laneOf(row)), self)
	}
	// pick keeps the first row, in rank order, that sits in an admitted lane
	// and carries one of the accepted verdicts.
	//
	// accept is a SET and never a preference order: composite rank is the only
	// tiebreak routing gets to apply, and ranking capacities against each other
	// would quietly reintroduce this ticket's headline symptom — the backlog's
	// #1 row, an orphan, passed over for a lower-ranked leaf that happened to
	// need no takeover. [LAW:one-source-of-truth] one ordering, and the gather
	// already established it.
	pick := func(inScope func(model.LaneID) bool, accept ...capacity) (annotation.AnnotatedIssue, capacity, bool) {
		for _, row := range rows {
			if how := verdict(row); inScope(laneOf(row)) && slices.Contains(accept, how) {
				return row, how, true
			}
		}
		return annotation.AnnotatedIssue{}, routeAround, false
	}

	ownLanes, ownEpics := ownScope(standings, self)
	mine := func(lane model.LaneID) bool { return ownLanes[lane] }
	if len(ownLanes) > 0 {
		// Step 1 — our own lanes, startable work and work already underway
		// alike, whichever the backlog ranks first.
		if row, how, ok := pick(mine, serveWork, resumeWork); ok {
			if how == resumeWork {
				return ResumedOwnWork{Row: row}
			}
			return ServedFromClaim{Row: row}
		}
		// Step 1b — a dependency outside our lanes that gates one of them. It
		// establishes a claim on a lane we do not hold, so it is announced as
		// one (N3) and its own lane's standing is honoured rather than
		// ignored (N2).
		if dep, ok := onPathDependency(rows, laneOf, mine, verdict); ok {
			return ServedFromNewLane{Row: dep, Lane: laneOf(dep).String()}
		}
		// Step 2 — the rest of our epic, in lanes we do not already hold.
		ourEpic := func(lane model.LaneID) bool {
			return lane.Epic() != "" && ownEpics[lane.Epic()]
		}
		if row, _, ok := pick(func(lane model.LaneID) bool { return ourEpic(lane) && !mine(lane) }, serveWork, takeoverWork); ok {
			lane := laneOf(row)
			return ServedFromEpicLane{Row: row, Epic: lane.Epic(), Lane: lane.String()}
		}
		// Step 3 — loud, and never a hop.
		return Exhausted{
			Epics: slices.Sorted(maps.Keys(ownEpics)),
			Blocked: gatingDependencyIDs(rows, laneOf, func(lane model.LaneID) bool {
				return mine(lane) || ourEpic(lane)
			}),
		}
	}

	// Step 4 — the global pool.
	if row, _, ok := pick(func(model.LaneID) bool { return true }, serveWork, takeoverWork); ok {
		return ServedFromNewLane{Row: row, Lane: laneOf(row).String()}
	}
	return NoWork{}
}

// gatingDependencyIDs collects the distinct ids of the open dependencies that
// gate the open rows whose lane inScope admits, in rank order. Both consumers
// are this walk plus one question: onPathDependency asks which of these ids
// this checkout may itself take, and Exhausted reports the whole list. They
// differ in the scope they pass and in what they do with the ids — never in how
// the ids are found. [LAW:one-source-of-truth]
func gatingDependencyIDs(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, inScope func(model.LaneID) bool) []string {
	seen := map[string]bool{}
	var ids []string
	for _, row := range rows {
		if !inScope(laneOf(row)) || row.State() != model.StateOpen {
			continue
		}
		for _, dep := range ClassifyReadiness(row.Annotations).DependencyIDs() {
			if !seen[dep] {
				seen[dep] = true
				ids = append(ids, dep)
			}
		}
	}
	return ids
}

// onPathDependency finds the first dependency gating one of our own lanes that
// this checkout may itself take — "a dependency outside the claimed lane that
// gates it is offered as on-path" (design-docs/work-claims.md, Routing step 1).
// A same-lane gate (an earlier sibling) never reaches here: it shares the
// blocked row's lane, so step 1 already served or resumed it.
//
// Takeability is the shared verdict, not a local readiness test. Before, this
// function saw no standings at all and would happily offer a ticket sitting in
// a lane another checkout holds fresh — which `lit start` then refused, so
// `next` recommended what `start` blocked (links-claims-1b0p, N2). It also no
// longer re-checks that the gated row is unservable: step 1 accepts every
// capacity an own lane can produce, so by the time we are here every row in
// `mine` is routeAround by construction.
func onPathDependency(rows []annotation.AnnotatedIssue, laneOf func(annotation.AnnotatedIssue) model.LaneID, mine func(model.LaneID) bool, verdict func(annotation.AnnotatedIssue) capacity) (annotation.AnnotatedIssue, bool) {
	byID := make(map[string]annotation.AnnotatedIssue, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, id := range gatingDependencyIDs(rows, laneOf, mine) {
		if dep, ok := byID[id]; ok && verdict(dep) != routeAround {
			return dep, true
		}
	}
	return annotation.AnnotatedIssue{}, false
}

// exhaustedError renders Exhausted as the loud diagnostic the design
// demands in place of a silent hop to another epic's leaf.
func exhaustedError(o Exhausted) error {
	scope := "your claimed lane(s)"
	if len(o.Epics) > 0 {
		scope = fmt.Sprintf("epic(s) %s", strings.Join(o.Epics, ", "))
	}
	if len(o.Blocked) == 0 {
		return fmt.Errorf("no ready work in %s — nothing else is queued behind what's already in progress; picking up other work is a deliberate re-focus, not a bare `next`", scope)
	}
	return fmt.Errorf("no ready work in %s — blocked on %s (unclaimed, on your path — `lit start` it); picking up other work is a deliberate re-focus, not a bare `next`", scope, strings.Join(o.Blocked, ", "))
}
