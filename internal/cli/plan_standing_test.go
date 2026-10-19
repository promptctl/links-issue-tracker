package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// lit done finishes a ticket wherever its plan stands. A plan short of
// approval is a warning naming what is missing, never a refusal; the event
// records the standing, and an approval adds the plan and the review it
// rested on.
func TestDoneFinishesWhereverThePlanStands(t *testing.T) {
	isolateEvents(t)
	dir, _, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "planned epic")
	const plan = "Close the ticket; nothing is added."
	digest := string(model.DigestPlan(plan))
	const warning = "warning: finished without an approved plan: "

	for _, tc := range []struct {
		standing string
		review   func(id string) // the plan and verdicts the ticket is finished on
		warns    string          // what the warning names; empty for none
	}{
		{"no_plan", func(string) {}, " has no plan"},
		{"unreviewed", func(id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
		}, "'s current plan (v1, digest " + digest + ") has no review"},
		{"author_only", func(id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
			runCLIInDir(t, dir, "verdict", id, "pass", "--plan", digest, "--by", "author")
		}, "has passed only its author's own review (author)"},
		{"failed", func(id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
			runCLIInDir(t, dir, "verdict", id, "fail", "--plan", digest, "--law", "carrying-cost", "--by", "ada")
		}, "ada failed it citing carrying-cost"},
		{"approved", func(id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
			runCLIInDir(t, dir, "verdict", id, "pass", "--plan", digest, "--by", "ada")
		}, ""},
	} {
		t.Run(tc.standing, func(t *testing.T) {
			id := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Plan "+tc.standing, "--topic", "demo", "--parent", epic, "--by", "filer"))
			runCLIInDir(t, dir, "start", id, "--by", "agent")
			tc.review(id)

			out := runCLIInDir(t, dir, "done", id)
			facts := writtenFacts(t, lastEvent(t))
			if status := strings.TrimSpace(runCLIInDir(t, dir, "show", id, "--field", "status")); status != "closed" {
				t.Fatalf("lit done with plan %s: status = %q, want closed", tc.standing, status)
			}
			if facts["done.plan_standing"] != tc.standing {
				t.Errorf("lit done event done.plan_standing = %v, want %s", facts["done.plan_standing"], tc.standing)
			}
			if tc.warns == "" {
				if strings.Contains(out, warning) || facts["done.plan_digest"] != digest || facts["done.plan_review"] == nil {
					t.Errorf("approved lit done: output %q, facts %v; want no warning, done.plan_digest %s and done.plan_review", out, facts, digest)
				}
				return
			}
			if !strings.Contains(out, warning+id) || !strings.Contains(out, tc.warns) {
				t.Errorf("lit done output = %q, want a warning naming %s and %q", out, id, tc.warns)
			}
		})
	}
}

// A done on a ticket already closed finishes nothing, so it reads no plan, and
// its event says it found the ticket closed.
func TestDoneOnAClosedTicketReadsNoPlan(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "close", id, "--resolution", "wontfix")
	runCLIInDir(t, dir, "done", id)
	if got := writtenFacts(t, lastEvent(t))["done.plan_standing"]; got != "already_closed" {
		t.Fatalf("lit done on a closed ticket: done.plan_standing = %v, want already_closed", got)
	}
}
