package claims

import "github.com/promptctl/links-issue-tracker/internal/model"

// establishing is the set of lifecycle verbs that put a lane in a checkout's
// hands. Every other verb — and every plain field edit, which records no verb at
// all — can only refresh a claim that already exists, never create or transfer
// one, which is what stops a drive-by comment or a grooming pass from another
// checkout capturing an epic somebody else is deep into.
//
// Why exactly these two. `start` is the act of taking work: it is the moment a
// checkout commits, and the design's whole mechanism is "claiming is starting a
// ticket." `done` is the neutral success close — the work finished — so a
// checkout whose latest act was completing a ticket mid-lane still holds the
// lane it is halfway through, which is the behavior the design's own residual
// note describes.
//
// Why not the other six. `close` ends work *unfinished* (the type carries an
// Outcome saying why: duplicate, superseded, obsolete, wontfix) and is as often
// a grooming judgement as an act of execution — closing someone else's ticket as
// a duplicate must not hand you their lane. `reopen` un-completes rather than
// takes. The four retention verbs move an issue on an axis orthogonal to work
// entirely.
//
// The classification fails safe, which is why the narrow reading is the right
// one when the design leaves room: a verb wrongly treated as establishing hands
// a lane to a checkout that is not working it, and every other stream is then
// routed away from real work by evidence that never meant what it was read to
// mean. A verb wrongly treated as non-establishing only leaves the lane
// unclaimed — which is exactly how the tool behaved before claims existed.
//
// A map rather than a switch so establishingCoversEveryAction can assert this
// decision was made for every verb in model's sealed set; a ninth action added
// to lifecycle.Actions fails that test instead of falling through a default arm
// into "not establishing" with nobody having thought about it.
// [LAW:no-silent-failure]
var establishing = map[model.ActionName]bool{
	model.ActionStart: true,
	model.ActionDone:  true,

	model.ActionClose:     false,
	model.ActionReopen:    false,
	model.ActionArchive:   false,
	model.ActionUnarchive: false,
	model.ActionDelete:    false,
	model.ActionRestore:   false,
}

// establishes reports whether an event takes or transfers a lane. IssueEvent
// carries the verb as a bare string and leaves it empty for plain field updates,
// so an unrecognized or absent verb reads as non-establishing through the same
// lookup a known one does — the empty verb is genuinely "an edit, not a
// transition", which is the answer the map would give anyway.
func establishes(event model.IssueEvent) bool {
	return establishing[model.ActionName(event.Action)]
}

// LatestEstablisher returns the newest event among events that takes or
// transfers a lane, and reports whether there was one. "Newest" is byRecency,
// the one order this package means by later.
//
// It scans for the maximum rather than walking a sorted slice backwards, so it
// answers the same for a lane's ordered run as for a caller holding an issue's
// events in whatever order it read them. That independence is what lets the
// write side ask this question: ClaimantOf reads one issue's history straight
// out of the store, with no Evidence to have ordered it first.
func LatestEstablisher(events []model.IssueEvent) (model.IssueEvent, bool) {
	var latest model.IssueEvent
	found := false
	for _, event := range events {
		if establishes(event) && (!found || byRecency(latest, event) < 0) {
			latest, found = event, true
		}
	}
	return latest, found
}
