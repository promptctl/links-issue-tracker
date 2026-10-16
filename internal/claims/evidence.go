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
//
// The write side reads from here too, and that is deliberate rather than a
// layering slip: Claimant is what a storage engine compares to decide whether a
// transition moved ownership. With two definitions of ownership — the row's
// assignee on the write side, the establishing event's checkout here — a
// takeover between two checkouts sharing one assignee would fall into the gap
// between them and record nothing. One home for "who holds this" closes it.
// [LAW:one-source-of-truth]
package claims

import (
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Evidence is a complete lane-partitioned reading of the backlog: every issue
// placed in its lane, and every event placed in the lane of the issue it
// touched. "Complete" is the whole point of the type — see NewEvidence.
type Evidence struct {
	lanes   map[string]model.LaneID
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
		lanes:   lanes,
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
	// One ordering for every lane, imposed once here so that trails' fold —
	// which takes the LAST write to a checkout's activity as its latest — reads
	// the data rather than the order a caller happened to read it in.
	// [LAW:single-enforcer]
	for lane := range ev.events {
		slices.SortFunc(ev.events[lane], model.CompareEventRecency)
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

// LaneProgress is the "how is it going" half of a lane's dossier: how many of
// its member issues are closed, out of how many total, and which member (if
// any) is the one actually in progress right now. Only members still in the
// flow count: an archived or deleted ticket has left the lane's work, so it is
// neither done nor outstanding. Active is its own field
// rather than something a caller infers from Done/Total because an
// epic-major lane can hold several sibling issues — the one a listing
// happens to be rendering is not necessarily the one the claimant is
// actually working, and the dossier names that one explicitly.
type LaneProgress struct {
	Done, Total int
	Active      *model.Issue
}

// LaneProgress reports the given lane's progress. A lane this evidence never
// saw reports the zero LaneProgress (Total 0), which formats as nothing —
// the same total-map-read convention Standings.Of uses.
func (e Evidence) LaneProgress(lane model.LaneID) LaneProgress {
	return progressOf(e.members[lane])
}

// LaneProgressWith reports the progress of the lane written is in, as this
// evidence partitioned it, with written counted in place of the version this
// evidence read. A command that gathered the evidence before its own write
// learns the progress after it this way, without gathering again; and because
// written itself is counted, the progress it reports is never the empty lane's.
func (e Evidence) LaneProgressWith(written model.Issue) LaneProgress {
	members := slices.DeleteFunc(slices.Clone(e.members[e.lanes[written.ID]]), func(member model.Issue) bool { return member.ID == written.ID })
	return progressOf(append(members, written))
}

// IssueEvents is every event of the issue with id, oldest first: the issue's
// whole history, since the evidence holds every event of every issue.
func (e Evidence) IssueEvents(id string) []model.IssueEvent {
	return slices.DeleteFunc(slices.Clone(e.events[e.lanes[id]]), func(event model.IssueEvent) bool { return event.IssueID != id })
}

func progressOf(members []model.Issue) LaneProgress {
	var progress LaneProgress
	// [LAW:one-source-of-truth] Frozen is the program's one answer to "has this
	// left the flow", the same one readiness and claim derivation ask.
	inFlow := slices.DeleteFunc(slices.Clone(members), func(member model.Issue) bool { return model.Frozen(member.Retention()) })
	for _, issue := range inFlow {
		progress.Total++
		switch issue.State() {
		case model.StateClosed:
			progress.Done++
		case model.StateInProgress:
			active := issue
			progress.Active = &active
		}
	}
	return progress
}
