package cli

// The quickstart routing gate: capacityFor's verdict for every lane relation,
// pinned as one table, because internal/templates/defaults/quickstart-work.md
// teaches that verdict to every agent `lit init` ever runs for.
//
// The incident this guards against (links-claims-xwqu): links-claims-1b0p
// retired the rule "a lane claimed elsewhere is never a bare `lit next`
// target" for the stale half, and the shipped quickstart kept teaching it for
// months. That is the same shape as the `lit ready` incident the dispatch gate
// in template_dispatch_gate_test.go was built for — text versioned apart from
// the behavior it describes — but one level deeper: the vocabulary stayed
// valid, so a "does this command still dispatch?" predicate passes it. The lie
// was in the semantics.
//
// SCOPE, stated honestly. This gate pins the behavior; it does not parse the
// prose, and no test here can. The correct text must say "never a bare `lit
// next` target" of a FRESH foreign hold, so banning that phrase would reject
// the fix; and the retired sentence names staleness too, so requiring the word
// passes the very text that caused the incident. A keyword gate over this
// paragraph fails in both directions, which is why there isn't one. What this
// table buys instead: any change to the routing verdicts turns it red, and the
// failure message names the file and the claim that must move in the same
// commit. Non-silent is the goal — mechanically-derived prose is not on offer.
// [LAW:behavior-not-structure] [LAW:no-silent-failure]

import (
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// capacityName and relationName render the two routing enums for a failure
// message. Neither carries a String method, so a bare %v prints the integer
// and sends the reader to next_route.go to count constants. The default arm
// names the gap instead of printing a number, so a value added to either enum
// without a name here reports itself. [LAW:no-silent-failure]
func capacityName(c capacity) string {
	switch c {
	case routeAround:
		return "routeAround"
	case serveWork:
		return "serveWork"
	case resumeWork:
		return "resumeWork"
	case takeoverWork:
		return "takeoverWork"
	}
	return fmt.Sprintf("capacity(%d) — unnamed here; add it to capacityName", int(c))
}

func relationName(r laneRelation) string {
	switch r {
	case laneUnclaimed:
		return "laneUnclaimed"
	case laneOurs:
		return "laneOurs"
	case laneStaleForeign:
		return "laneStaleForeign"
	case laneHeldForeign:
		return "laneHeldForeign"
	}
	return fmt.Sprintf("laneRelation(%d) — unnamed here; add it to relationName", int(r))
}

// laneRoutingCase is one lane relation and what a ready, open row sitting in
// it is worth to this checkout. `teaches` is the claim quickstart-work.md
// makes about that relation, carried here so a verdict change fails with the
// sentence that has just become false rather than with a bare enum mismatch.
// It is documentation of the shipped text, never a match against it — the
// prose may be reworded freely; only its meaning is pinned, and only to the
// engineer reading the failure.
type laneRoutingCase struct {
	name     string
	standing claims.Standing
	want     capacity
	teaches  string
}

// laneRoutingTable covers every relation relationOf can produce. The two
// foreign rows are the pair links-claims-xwqu is about: they differ ONLY in
// whether the holder's evidence has aged out, and that single fact flips the
// row between "routed around" and "offered, as a takeover".
var laneRoutingTable = []laneRoutingCase{
	{
		name:     "nobody holds it",
		standing: nil,
		want:     serveWork,
		teaches:  "starting global work with no live claim yet claims that lane for this checkout",
	},
	{
		name:     "we hold it",
		standing: heldBy(selfAttribution),
		want:     serveWork,
		teaches:  "`lit next` serves this checkout's own claims before anything else",
	},
	{
		name:     "another checkout holds it right now",
		standing: heldBy(otherAttribution),
		want:     routeAround,
		teaches:  "the epic continues into \"any lane of the same epic no other checkout holds fresh\", and \"a fresh claim is reached only by `lit start --take`\"",
	},
	{
		name:     "another checkout's hold has gone stale",
		standing: staleBy(otherAttribution),
		want:     takeoverWork,
		teaches:  "epic continuation admits stale lanes, \"stale claims included\", and \"only a stale claim makes it a bare `lit next` target — taking it transfers the lane to this checkout\"",
	},
}

// TestCapacityForEachLaneRelation is the gate. One ready, open row is built
// once and held fixed; only the standing varies, so every difference in the
// verdict is attributable to the lane relation alone and nothing else.
// [LAW:one-source-of-truth] the row's readiness is established by the real
// gather, not asserted by hand.
func TestCapacityForEachLaneRelation(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "ready leaf", Topic: "next", IssueType: "task", Priority: 0})
	rows, _ := h.gather()
	row := rowByID(t, rows, issue.ID)
	if row.State() != model.StateOpen || !ClassifyReadiness(row.Annotations).IsReady() {
		t.Fatalf("fixture row is state=%v ready=%v, want an open ready row — the table below reads the lane relation only, so the row must contribute nothing", row.State(), ClassifyReadiness(row.Annotations).IsReady())
	}

	for _, tc := range laneRoutingTable {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityFor(row, tc.standing, selfAttribution)
			if got != tc.want {
				t.Errorf("capacityFor(ready open row, %s) = %s, want %s.\n"+
					"internal/templates/defaults/quickstart-work.md teaches: %s\n"+
					"If this verdict changed deliberately, that text is now false — rewrite it and this row in the same commit.",
					tc.name, capacityName(got), capacityName(tc.want), tc.teaches)
			}
			// The quickstart's claim is about targeting, not about the verdict's
			// name: everything that is not routed around is something a bare
			// `lit next` can land on. Derived here rather than restated, so the
			// two can never disagree.
			if bareNextTarget := got != routeAround; bareNextTarget != (tc.want != routeAround) {
				t.Errorf("bare `lit next` target = %v for %s, want %v", bareNextTarget, tc.name, tc.want != routeAround)
			}
		})
	}
}

// TestLaneRoutingTableCoversEveryRelation holds the table to the relation set
// relationOf actually produces. The table is keyed by standing, so a relation
// that gained a new standing — or one that quietly stopped being reachable —
// shows up here rather than as a silently unexercised routing arm.
// [LAW:no-silent-failure]
func TestLaneRoutingTableCoversEveryRelation(t *testing.T) {
	covered := map[laneRelation]bool{}
	for _, tc := range laneRoutingTable {
		relation := relationOf(tc.standing, selfAttribution)
		if covered[relation] {
			t.Errorf("case %q duplicates lane relation %s; each relation earns exactly one row", tc.name, relationName(relation))
		}
		covered[relation] = true
	}
	// Every standing claims.Derive can build, against both identities: the
	// full input space relationOf is total over.
	for _, standing := range []claims.Standing{
		nil,
		heldBy(selfAttribution), heldBy(otherAttribution),
		staleBy(selfAttribution), staleBy(otherAttribution),
	} {
		if relation := relationOf(standing, selfAttribution); !covered[relation] {
			t.Errorf("standing %T yields lane relation %s, which no row in laneRoutingTable covers; add it, and say what quickstart-work.md teaches about it", standing, relationName(relation))
		}
	}
}
