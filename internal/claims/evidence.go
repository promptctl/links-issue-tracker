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

// Lane is a complete reading of one lane: every issue in it and every event
// they carry. Standing and progress are questions about one lane, so a reader
// that asks them of one lane holds this and has no other lane to misread, where
// an Evidence read short of the backlog would answer Unclaimed for every lane
// it skipped. Evidence.Lane yields the same value for each lane of the backlog.
type Lane struct {
	id      model.LaneID
	members []model.Issue
	events  []model.IssueEvent
}

// Family is the unparsed reading NewLane takes, what storage's LaneFamily
// returns: the issues model.LaneFamily names, closed, archived and deleted
// included, what each sits under, and the history they carry. An event needs
// only its action, attribution and instant here, the whole of what derivation
// reads.
//
// ParentTypes gives each parent ParentByChild names its type and nothing else,
// because a lane needs only to know whether its parent scopes it, and hydrating
// a parent costs its whole subtree.
type Family struct {
	Issues        []model.Issue
	ParentByChild map[string]string
	ParentTypes   map[string]model.IssueType
	Events        []model.IssueEvent
}

// NewEvidence partitions issues into lanes and files every event under the lane
// of the issue it touched. parentByChild maps an issue id to its parent's id; an
// issue absent from the map is parentless.
//
// It fails when an event names an issue that was not supplied, or a supplied
// issue sits under one that was not, and that check is the reason this
// constructor exists. A missing parent would leave its children as lanes of
// one. Claim derivation reads closed tickets:
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
// comes out. Derive takes only Evidence, so it cannot run on a partial read, and
// nothing inland re-checks. The one other way to a standing is a Lane, which
// NewLane proves whole for its one lane.
func NewEvidence(issues []model.Issue, parentByChild map[string]string, events []model.IssueEvent) (Evidence, error) {
	parentTypes := make(map[string]model.IssueType, len(issues))
	for _, issue := range issues {
		parentTypes[issue.ID] = issue.IssueType
	}
	placed, err := newPartition(issues, parentByChild, parentTypes, events)
	return Evidence(placed), err
}

// NewLane is the crossing for a reader of one lane: it partitions issueID's
// family exactly as NewEvidence partitions the backlog, refusals included, and
// keeps the lane issueID landed in. The family reaches past that lane, because
// a lane key is an editable field and a sibling's parent can be another issue's
// in restored data, so every other lane the partition holds may be part-read,
// which is why a partition is not an Evidence.
//
// The events come from a read that shares no id list with the issues', so the
// refusal holds here as it does for NewEvidence: a closed, archived or deleted
// ticket a read dropped is one the events still name. Both reads do walk one
// join, so the refusal cannot see that walk fall short; the conformance case
// holding every lane to the whole-store Derive does.
func NewLane(issueID string, family Family) (Lane, error) {
	placed, err := newPartition(family.Issues, family.ParentByChild, family.ParentTypes, family.Events)
	if err != nil {
		return Lane{}, err
	}
	lane, known := placed.lanes[issueID]
	if !known {
		return Lane{}, fmt.Errorf("claims: issue %s is not among the %d issues of the family read for it", issueID, len(family.Issues))
	}
	return placed.lane(lane), nil
}

// partition is what a read placed in lanes, which may be less than the backlog.
// It has Evidence's fields and none of its methods, so only NewEvidence, handed
// the whole backlog, can make a derivation's input of one.
type partition Evidence

// newPartition places every issue in its lane and every event in the lane of
// the issue it touched. A parent is its id and type here and nothing else,
// because model.LaneOf reads no lifecycle.
func newPartition(issues []model.Issue, parentByChild map[string]string, parentTypes map[string]model.IssueType, events []model.IssueEvent) (partition, error) {
	lanes := make(map[string]model.LaneID, len(issues))
	placed := partition{
		lanes:   lanes,
		members: make(map[model.LaneID][]model.Issue),
		events:  make(map[model.LaneID][]model.IssueEvent),
	}
	for _, issue := range issues {
		var parent *model.Issue
		if parentID, under := parentByChild[issue.ID]; under {
			parentType, known := parentTypes[parentID]
			if !known {
				return partition{}, fmt.Errorf("claims: issue %s sits under %s, which was not supplied: claim derivation places an issue by its parent, so every parent must be supplied", issue.ID, parentID)
			}
			parent = &model.Issue{ID: parentID, IssueType: parentType}
		}
		lane := model.LaneOf(issue, parent)
		lanes[issue.ID] = lane
		placed.members[lane] = append(placed.members[lane], issue)
	}
	for _, event := range events {
		lane, known := lanes[event.IssueID]
		if !known {
			return partition{}, fmt.Errorf("claims: event %s belongs to issue %s, which was not among the %d issues supplied: claim derivation needs every issue the events touch, closed ones included", event.ID, event.IssueID, len(issues))
		}
		placed.events[lane] = append(placed.events[lane], event)
	}
	// One ordering for every lane, imposed once here so that trails' fold —
	// which takes the LAST write to a checkout's activity as its latest — reads
	// the data rather than the order a caller happened to read it in.
	// [LAW:single-enforcer]
	for lane := range placed.events {
		slices.SortFunc(placed.events[lane], model.CompareEventRecency)
	}
	return placed, nil
}

// lane is the reading of one lane as placed.
func (p partition) lane(lane model.LaneID) Lane {
	return Lane{id: lane, members: p.members[lane], events: p.events[lane]}
}

// Lanes lists every lane the evidence covers, in no particular order.
func (e Evidence) Lanes() []model.LaneID {
	lanes := make([]model.LaneID, 0, len(e.members))
	for lane := range e.members {
		lanes = append(lanes, lane)
	}
	return lanes
}

// Progress is how far a set of tickets has come: how many are closed, out of
// how many are still in the flow. An archived or deleted ticket has left the
// work, so it is neither done nor outstanding.
type Progress struct{ Done, Total int }

// Finished says nothing in the scope is left to do: every ticket in it is done,
// or none is left in it at all.
func (p Progress) Finished() bool { return p.Done == p.Total }

// LaneProgress is the "how is it going" half of a lane's dossier: the lane's
// Progress, and which member (if any) is the one actually in progress right
// now. Active is its own field
// rather than something a caller infers from Done/Total because an
// epic-major lane can hold several sibling issues — the one a listing
// happens to be rendering is not necessarily the one the claimant is
// actually working, and the dossier names that one explicitly.
type LaneProgress struct {
	Progress
	Active *model.Issue
}

// Lane is one lane of this evidence. A lane this evidence never saw has no
// members and no events — the same total-map-read convention Standings.Of uses.
func (e Evidence) Lane(lane model.LaneID) Lane {
	return partition(e).lane(lane)
}

// LaneProgress reports the given lane's progress. A lane this evidence never
// saw reports the zero LaneProgress (Total 0), which formats as nothing.
func (e Evidence) LaneProgress(lane model.LaneID) LaneProgress {
	return e.Lane(lane).Progress()
}

// ScopeProgress reports the progress of every lane inScope admits, counted as
// one set of tickets — how far along a scope several lanes make up, such as
// a whole epic, has come. It names no Active ticket: a scope can hold one in
// progress in each of its lanes, and choosing one would be arbitrary.
func (e Evidence) ScopeProgress(inScope func(model.LaneID) bool) Progress {
	var members []model.Issue
	for lane, laneMembers := range e.members {
		if inScope(lane) {
			members = append(members, laneMembers...)
		}
	}
	return progressOf(members).Progress
}

// LaneProgressWith is ProgressWith on the lane written is in, as this evidence
// partitioned it.
func (e Evidence) LaneProgressWith(written model.Issue) LaneProgress {
	return e.Lane(e.lanes[written.ID]).ProgressWith(written)
}

// ID names the lane this reading is of.
func (l Lane) ID() model.LaneID { return l.id }

// Standing applies the claim predicate to this lane: what Derive answers for
// it, by the same derivation.
func (l Lane) Standing(fresh Freshness, local LocalCheckouts) Standing {
	return standingOf(l.members, l.events, fresh, local)
}

// Progress reports this lane's progress.
func (l Lane) Progress() LaneProgress {
	return progressOf(l.members)
}

// ProgressWith reports this lane's progress with written counted in place of
// the version this reading holds. A command that read the lane before its own
// write learns the progress after it this way, without reading again; and
// because written itself is counted, the progress it reports is never the empty
// lane's.
func (l Lane) ProgressWith(written model.Issue) LaneProgress {
	members := slices.DeleteFunc(slices.Clone(l.members), func(member model.Issue) bool { return member.ID == written.ID })
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
