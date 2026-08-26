package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// NextOutcome is what routeNext concluded, sealed to the cases the routing
// precedence in design-docs/work-claims.md — as amended by the epic-major
// GRANULARITY RULING on links-claims-1ihf.5 — can produce. A bare
// annotation.AnnotatedIssue cannot say WHICH case picked it, and the
// exhaustion case in particular must never be confused with an ordinary
// pick: it is an answer-shaped void unless the type forbids the confusion.
// [LAW:types-are-the-program] [LAW:parse-dont-validate]
type NextOutcome interface{ isNextOutcome() }

// ServedFromClaim is a ready ticket in a lane this checkout already holds —
// routing step 1. No new claim is established, so nothing is announced.
type ServedFromClaim struct{ Row annotation.AnnotatedIssue }

// ServedFromEpicLane is a ready ticket in a different, unclaimed lane of the
// same epic this checkout already holds a lane in — the GRANULARITY RULING's
// new step 2, epic-major before global. Starting it establishes a fresh
// claim on Lane, which is why it carries the label to announce.
type ServedFromEpicLane struct {
	Row  annotation.AnnotatedIssue
	Epic string
	Lane string
}

// ServedFromGlobal is a ready ticket in an unclaimed lane, reached only once
// the checkout has no live claims of its own — routing step 4, the zero
// state. Starting it establishes a fresh claim, announced by Lane exactly as
// the design's example spells it: "starting B.1 claims B#1".
type ServedFromGlobal struct {
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
func (ServedFromEpicLane) isNextOutcome() {}
func (ServedFromGlobal) isNextOutcome()   {}
func (Exhausted) isNextOutcome()          {}
func (NoWork) isNextOutcome()             {}

// routeNext applies the claim-aware selection precedence over an already
// gathered, already ordered workable set. Pure — standings and self are
// values the caller derived once from the store and the local filesystem, so
// every branch of the precedence is testable without either.
// [LAW:effects-at-boundaries]
//
// Precedence: the checkout's own held lanes first (a ready row, or failing
// that an on-path external dependency that gates one); then the rest of its
// epic's unclaimed lanes (the GRANULARITY RULING's epic-major amendment);
// then a loud exhaustion diagnostic if the epic has open work but none of it
// is reachable; then the global pool of unclaimed lanes elsewhere. A
// checkout with no live claims starts straight at the global pool — unfocus
// is the zero state, not a hop through the earlier steps.
// [LAW:dataflow-not-control-flow] rows is walked in the same composite-rank
// order at every step; only which predicate admits a row changes.
func routeNext(rows []annotation.AnnotatedIssue, details map[string]store.IssueRelations, standings claims.Standings, self model.Attribution) NextOutcome {
	laneOf := func(row annotation.AnnotatedIssue) model.LaneID {
		return model.LaneOf(row.Issue, details[row.ID].Parent)
	}

	if self.Present() {
		ownLanes := map[model.LaneID]bool{}
		for _, row := range rows {
			if heldBySelf(standings.Of(laneOf(row)), self) {
				ownLanes[laneOf(row)] = true
			}
		}
		if len(ownLanes) > 0 {
			for _, row := range rows {
				if ownLanes[laneOf(row)] && isReadyRow(row) {
					return ServedFromClaim{Row: row}
				}
			}
			if dep, ok := onPathDependency(rows, ownLanes, laneOf); ok {
				return ServedFromClaim{Row: dep}
			}
			epics := map[string]bool{}
			for lane := range ownLanes {
				if lane.Epic() != "" {
					epics[lane.Epic()] = true
				}
			}
			for _, row := range rows {
				lane := laneOf(row)
				if lane.Epic() != "" && epics[lane.Epic()] && !ownLanes[lane] && isReadyRow(row) && isUnclaimed(standings.Of(lane)) {
					return ServedFromEpicLane{Row: row, Epic: lane.Epic(), Lane: lane.String()}
				}
			}
			return Exhausted{
				Epics:   sortedKeys(epics),
				Blocked: blockedDependencyIDs(rows, ownLanes, epics, laneOf),
			}
		}
	}

	for _, row := range rows {
		lane := laneOf(row)
		if isReadyRow(row) && isUnclaimed(standings.Of(lane)) {
			return ServedFromGlobal{Row: row, Lane: lane.String()}
		}
	}
	return NoWork{}
}

// isReadyRow is routeNext's readiness predicate: the same open-and-unblocked
// test the pre-claims selector used, so a lane with nothing to gate it
// behaves exactly as it always did.
func isReadyRow(row annotation.AnnotatedIssue) bool {
	return row.State() == model.StateOpen && ClassifyReadiness(row.Annotations).IsReady()
}

// heldBySelf reports whether standing is a claim this checkout holds.
func heldBySelf(standing claims.Standing, self model.Attribution) bool {
	held, ok := standing.(claims.Held)
	return ok && held.By == self
}

// isUnclaimed reports the lane's zero state — the only standing bare `next`
// may route into on someone else's behalf. Held-by-another and Stale are
// both excluded: design-docs/work-claims.md is explicit that a stale claim
// is "never reached by bare `next`" — takeover is `lit start`'s deliberate
// act (links-claims-1ihf.7), not a default this command may reach for.
func isUnclaimed(standing claims.Standing) bool {
	_, ok := standing.(claims.Unclaimed)
	return ok
}

// onPathDependency finds the first open external dependency, in own-lane
// rank order, that gates a not-yet-ready row in one of ownLanes and is
// itself ready — "a dependency outside the claimed lane that gates it is
// offered as on-path" (design-docs/work-claims.md, Routing step 1). A
// same-lane gate (an earlier sibling) never reaches here: it shares the
// blocked row's lane, so it is already a member of ownLanes and would have
// been returned by the ready-row scan that runs before this one.
func onPathDependency(rows []annotation.AnnotatedIssue, ownLanes map[model.LaneID]bool, laneOf func(annotation.AnnotatedIssue) model.LaneID) (annotation.AnnotatedIssue, bool) {
	byID := make(map[string]annotation.AnnotatedIssue, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, row := range rows {
		if !ownLanes[laneOf(row)] || isReadyRow(row) || row.State() != model.StateOpen {
			continue
		}
		for _, depID := range ClassifyReadiness(row.Annotations).DependencyIDs() {
			if dep, ok := byID[depID]; ok && isReadyRow(dep) {
				return dep, true
			}
		}
	}
	return annotation.AnnotatedIssue{}, false
}

// blockedDependencyIDs collects the distinct open-dependency ids gating the
// open (not necessarily ready) rows within the exhausted scope — ownLanes
// plus the rest of their epics — in encounter order. Empty means the scope's
// remaining open work is in_progress with nothing else queued behind it,
// which the exhaustion message renders differently from a named blocker.
func blockedDependencyIDs(rows []annotation.AnnotatedIssue, ownLanes map[model.LaneID]bool, epics map[string]bool, laneOf func(annotation.AnnotatedIssue) model.LaneID) []string {
	seen := map[string]bool{}
	var ids []string
	for _, row := range rows {
		lane := laneOf(row)
		inScope := ownLanes[lane] || (lane.Epic() != "" && epics[lane.Epic()])
		if !inScope || row.State() != model.StateOpen {
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

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
