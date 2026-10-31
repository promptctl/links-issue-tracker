package cli

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// TestStartAnnouncesNoTransferFromAPredecessorTheLaneNoLongerNames reopens a
// ticket another checkout finished, in a lane this checkout has since taken
// through a sibling. The ticket's record still names the other checkout, so
// Apply reports the handover it recorded; the lane names this checkout, so the
// other one held nothing to hand over and the start owes no transfer line.
func TestStartAnnouncesNoTransferFromAPredecessorTheLaneNoLongerNames(t *testing.T) {
	h := newReadyTestHarness(t)
	recorder := telemetry.Begin(telemetry.Event{})
	h.ctx = telemetry.With(h.ctx, recorder)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "Home epic", Topic: "claims", IssueType: "epic"})
	finished := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "finished elsewhere", Topic: "claims", IssueType: "task", ParentID: epic.ID})
	sibling := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "taken here", Topic: "claims", IssueType: "task", ParentID: epic.ID})
	h.asCheckout("otherstream01")
	h.transition(finished.ID, model.Start{})
	h.transition(finished.ID, model.Done{})
	h.asCheckout(h.ap.Stream.Value())
	h.transition(sibling.ID, model.Start{})

	// runLeaf's phases under this test's own recorder, which runLeaf would
	// replace with a fresh one, so the event below is the start's.
	var out bytes.Buffer
	start := transitionLeaf(startSpec)
	positional, err := parseLeaf(h.ctx, start, []string{finished.ID}, cwdInvocation(&out, &out, nil))
	if err != nil {
		t.Fatalf("parse start %s error = %v", finished.ID, err)
	}
	if err := start.work(h.ctx, cwdInvocation(&out, &out, nil), h.ap, positional); err != nil {
		t.Fatalf("start %s error = %v", finished.ID, err)
	}
	if strings.Contains(out.String(), "claim transferred") {
		t.Fatalf("start %s = %q, want no transfer line: the lane names this checkout, not the one that finished the ticket", finished.ID, out.String())
	}
	facts := map[string]any{}
	for key, value := range writtenFacts(t, recorder.Finish(telemetry.End{})) {
		if strings.HasPrefix(key, "start.") {
			facts[key] = value
		}
	}
	if want := map[string]any{"start.lane_family_issues": float64(2), "start.lane_held": true, "start.handover": true, "start.transfer_announced": false}; !reflect.DeepEqual(facts, want) {
		t.Fatalf("start event facts = %v, want %v", facts, want)
	}
}

// TestRelationOf is the gate's predicate stated as its own contract: every
// standing claims.Derive can build, read against the two identities a caller
// can have, and the relation each pair yields. The takeover gate and routing
// both consume this value, so what it pins is what both of them do.
// [LAW:behavior-not-structure]
//
// There is no row for an expired claim because there is no standing for one:
// the derivation returns Unclaimed the moment the window closes, so this
// function never learns a claim existed and cannot treat it as a grade of
// hold. That absence is the ruling of links-claims-y6yz, and it is pinned
// where a reader would go looking for the missing row.
func TestRelationOf(t *testing.T) {
	tests := []struct {
		name     string
		standing claims.Standing
		self     model.Attribution
		want     laneRelation
	}{
		{"unclaimed is nobody's", claims.Unclaimed{}, selfAttribution, laneUnclaimed},
		{"a nil standing reads as unclaimed, never as a panic", nil, selfAttribution, laneUnclaimed},
		{"held by self is ours", heldBy(selfAttribution), selfAttribution, laneOurs},
		{"held by someone else is foreign", heldBy(otherAttribution), selfAttribution, laneHeldForeign},
		// The public checkout is every unattributed writer at once, and a
		// checkout with no minted token has recorded nothing, so a zero self
		// equal to a zero holder proves nothing about whose lane it is.
		{"held by the public checkout is foreign even to an unminted self", heldBy(publicAttribution), publicAttribution, laneHeldForeign},
		{"held by the public checkout is foreign to a minted self", heldBy(publicAttribution), selfAttribution, laneHeldForeign},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := relationOf(tc.standing, tc.self); got != tc.want {
				t.Errorf("relationOf(%#v, %v) = %v, want %v", tc.standing, tc.self, got, tc.want)
			}
		})
	}
}

// Questions asked over one stdin each read their own answer, so a bulk start
// prompting for two held lanes hears both of a piped pair of answers.
func TestAskYesNoLeavesEachLaterAnswerForItsQuestion(t *testing.T) {
	stdin := strings.NewReader("y\nn\ny\n")
	var got []bool
	for range 3 {
		ok, err := askYesNo(stdin, io.Discard, "take over this lane?")
		if err != nil {
			t.Fatalf("askYesNo error = %v", err)
		}
		got = append(got, ok)
	}
	if want := []bool{true, false, true}; !reflect.DeepEqual(got, want) {
		t.Errorf("answers = %v, want %v", got, want)
	}
}
