// Package claims derives, at read time, which checkout is working which lane.
//
// Nothing here is stored. A claim is a reading of work records the database
// already synchronizes — who started or completed which ticket, and when — so
// it cannot drift from the evidence the way a written claim row could, because
// it IS the evidence, read. The normative design is
// design-docs/work-claims.md; where this package and that document disagree,
// the document wins.
//
// The package is deliberately pure: it takes issues, events, a clock reading,
// and what the local machine knows about its own checkouts, and returns values.
// No store, no time.Now, no filesystem. Deriving a claim writes nothing, and
// the shape of this package is what makes that true rather than a promise.
// [LAW:effects-at-boundaries]
package claims

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Evidence is a complete lane-partitioned reading of the backlog: every issue
// placed in its lane, and every event placed in the lane of the issue it
// touched. "Complete" is the whole point of the type — see NewEvidence.
type Evidence struct {
	members map[model.LaneID][]model.Issue
	events  map[model.LaneID][]model.IssueEvent
}

// NewEvidence partitions issues into lanes and files every event under the lane
// of the issue it touched. parents maps an issue id to the parent the caller
// resolved for it; an issue absent from the map, or mapped to nil, is
// parentless.
//
// It fails when an event names an issue that was not supplied, and that check is
// the reason this constructor exists. Claim derivation reads closed tickets:
// completing a ticket is an establishing act, so a checkout's hold on a lane can
// rest entirely on a `done` against a ticket that is no longer open. A caller
// that passes only the live issues would hand over a lane whose establishing
// event it never read, and the derivation would answer "unclaimed" — a plausible
// answer, indistinguishable from the truth, for a lane somebody is holding. So
// the incomplete read is refused here rather than silently producing a wrong
// claim downstream.
//
// [LAW:parse-dont-validate] This is the one crossing: loose slices go in, and an
// Evidence — which by its existence proves every event's issue was supplied —
// comes out. Derive takes only Evidence, so no derivation can run on a partial
// read, and nothing inland re-checks.
func NewEvidence(issues []model.Issue, parents map[string]*model.Issue, events []model.IssueEvent) (Evidence, error) {
	lanes := make(map[string]model.LaneID, len(issues))
	ev := Evidence{
		members: make(map[model.LaneID][]model.Issue),
		events:  make(map[model.LaneID][]model.IssueEvent),
	}
	for _, issue := range issues {
		lane := model.LaneOf(issue, parents[issue.ID])
		lanes[issue.ID] = lane
		ev.members[lane] = append(ev.members[lane], issue)
	}
	for _, event := range events {
		lane, known := lanes[event.IssueID]
		if !known {
			return Evidence{}, fmt.Errorf("claims: event %s belongs to issue %s, which was not among the %d issues supplied: claim derivation needs every issue the events touch, closed ones included", event.ID, event.IssueID, len(issues))
		}
		ev.events[lane] = append(ev.events[lane], event)
	}
	// One ordering for every lane, established once here so that "the latest
	// establishing event" is a fact about the data rather than about the order a
	// caller happened to read it in. Two events sharing a timestamp are ordered
	// by id — arbitrary, but total and stable, which is what the predicate needs.
	// [LAW:single-enforcer]
	for lane := range ev.events {
		slices.SortFunc(ev.events[lane], func(a, b model.IssueEvent) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
		})
	}
	return ev, nil
}

// Lanes lists every lane the evidence covers, in no particular order.
func (e Evidence) Lanes() []model.LaneID {
	lanes := make([]model.LaneID, 0, len(e.members))
	for lane := range e.members {
		lanes = append(lanes, lane)
	}
	return lanes
}
