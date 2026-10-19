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

	update := func(t *testing.T, id string) {
		runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
	}
	// verdict records a verdict on the plan and returns the id of the review
	// it recorded, which `lit verdict` prints as "verdict <review-id>: ...".
	verdict := func(t *testing.T, id, by string, args ...string) string {
		out := strings.Fields(runCLIInDir(t, dir, append([]string{"verdict", id, args[0], "--plan", digest, "--by", by}, args[1:]...)...))
		return strings.TrimSuffix(out[1], ":")
	}
	for _, tc := range []struct {
		name     string
		standing string
		filing   []string                             // extra `lit new` flags
		review   func(t *testing.T, id string) string // returns the approving review's id
		warns    string                               // what the warning names; empty for none
	}{
		{"no_plan", "no_plan", nil, func(*testing.T, string) string { return "" }, " has no plan"},
		{"unreviewed", "unreviewed", nil, func(t *testing.T, id string) string {
			update(t, id)
			return ""
		}, "'s current plan (v1, digest " + digest + ") has no review"},
		{"author_only", "author_only", nil, func(t *testing.T, id string) string {
			update(t, id)
			verdict(t, id, "author", "pass")
			return ""
		}, "has passed only its author's own review (author)"},
		// A plan given at filing is its filer's, so the filer's own pass is
		// only the author's.
		{"filer_only", "author_only", []string{"--plan", plan}, func(t *testing.T, id string) string {
			verdict(t, id, "filer", "pass")
			return ""
		}, "has passed only its author's own review (filer)"},
		{"failed", "failed", nil, func(t *testing.T, id string) string {
			update(t, id)
			verdict(t, id, "ada", "fail", "--law", "carrying-cost")
			return ""
		}, "ada failed it citing carrying-cost"},
		// The author's own pass is the newest, so the approval rests on the
		// one before it.
		{"approved", "approved", nil, func(t *testing.T, id string) string {
			update(t, id)
			approval := verdict(t, id, "ada", "pass")
			verdict(t, id, "author", "pass")
			return approval
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filing := append([]string{"new", "--title", "Plan " + tc.name, "--topic", "demo", "--parent", epic, "--by", "filer"}, tc.filing...)
			id := extractTicketID(t, runCLIInDir(t, dir, filing...))
			runCLIInDir(t, dir, "start", id, "--by", "agent")
			approval := tc.review(t, id)

			out := runCLIInDir(t, dir, "done", id)
			facts := writtenFacts(t, lastEvent(t))
			if status := strings.TrimSpace(runCLIInDir(t, dir, "show", id, "--field", "status")); status != "closed" {
				t.Fatalf("lit done with plan %s: status = %q, want closed", tc.name, status)
			}
			if facts["done.plan_standing"] != tc.standing {
				t.Errorf("lit done event done.plan_standing = %v, want %s", facts["done.plan_standing"], tc.standing)
			}
			if tc.warns == "" {
				if strings.Contains(out, warning) || facts["done.plan_digest"] != digest || facts["done.plan_review"] != approval {
					t.Errorf("approved lit done: output %q, facts %v; want no warning, done.plan_digest %s and done.plan_review %s", out, facts, digest, approval)
				}
				return
			}
			if !strings.Contains(out, warning+id) || !strings.Contains(out, tc.warns) {
				t.Errorf("lit done output = %q, want a warning naming %s and %q", out, id, tc.warns)
			}
		})
	}
}

// A done on a ticket already closed finishes nothing and warns of nothing, and
// its event says it found the ticket closed.
func TestDoneOnAClosedTicketFinishesNothing(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "close", id, "--resolution", "wontfix")
	out := runCLIInDir(t, dir, "done", id)
	if got := writtenFacts(t, lastEvent(t))["done.plan_standing"]; got != "already_closed" || strings.Contains(out, "warning:") {
		t.Fatalf("lit done on a closed ticket: done.plan_standing = %v, output %q; want already_closed and no warning", got, out)
	}
}
