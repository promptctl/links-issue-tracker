package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// gatePlan is the plan approvePlan and approvePlanInDir give a ticket.
const gatePlan = "Close the ticket; nothing is added."

// approvePlan gives ticket id a plan written by one actor and passed by
// another, through the store, so a test that finishes a ticket with lit done
// finishes one the plan gate admits.
func approvePlan(t *testing.T, ctx context.Context, ap *app.App, id string) {
	t.Helper()
	plan := gatePlan
	if _, err := ap.Store.Apply(ctx, id, storage.Change{Actor: "plan-author", Fields: storage.UpdateIssueInput{Plan: &plan}}); err != nil {
		t.Fatalf("write plan of %s: %v", id, err)
	}
	if _, err := ap.Store.AddPlanReview(ctx, storage.AddPlanReviewInput{IssueID: id, Plan: string(model.DigestPlan(plan)), Verdict: string(model.PlanPass), CreatedBy: "plan-reviewer"}); err != nil {
		t.Fatalf("pass plan of %s: %v", id, err)
	}
}

// approvePlanInDir is approvePlan through the commands, for a test driving the
// workspace at dir. The author and the reviewer each run as their own session,
// so they differ whatever session the test runs in, and the session the test
// had is back once they are done.
func approvePlanInDir(t *testing.T, dir, id string) {
	t.Helper()
	asSession(t, "plan-author", func() { runCLIInDir(t, dir, "update", id, "--plan", gatePlan) })
	asSession(t, "plan-reviewer", func() {
		runCLIInDir(t, dir, "verdict", id, string(model.PlanPass), "--plan", string(model.DigestPlan(gatePlan)))
	})
}

func asSession(t *testing.T, session string, run func()) {
	t.Helper()
	const key = "CLAUDE_CODE_SESSION_ID"
	prev, had := os.LookupEnv(key)
	t.Setenv(key, session)
	run()
	if had {
		os.Setenv(key, prev)
	} else {
		os.Unsetenv(key)
	}
}

// lit done finishes a ticket only on a current plan that a reviewer other than
// its author has passed. Each refusal exits 3 with reason plan_not_approved,
// names what is missing, and records which gap on the event; the done that
// lands records the plan and the review it rested on.
func TestDoneFinishesOnlyAPassedCurrentPlan(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "start", id, "--by", "agent")
	const plan, revised = "Add the gate", "Add the gate, and name the gap it refused on"
	digest, revisedDigest := string(model.DigestPlan(plan)), string(model.DigestPlan(revised))
	refused := func(gap, message string) {
		t.Helper()
		_, err := runCLIInDirAllowError(t, dir, "done", id)
		if err == nil {
			t.Fatalf("lit done finished %s with gap %s open", id, gap)
		}
		if ExitCode(err) != ExitValidation || commandErrorReason(err) != reasonPlanNotApproved || !strings.Contains(err.Error(), message) {
			t.Fatalf("lit done = exit %d, reason %s, %q; want exit %d, reason %s, naming %q", ExitCode(err), commandErrorReason(err), err, ExitValidation, reasonPlanNotApproved, message)
		}
		if got := writtenFacts(t, lastEvent(t))["done.plan_gate"]; got != gap {
			t.Fatalf("refused lit done event done.plan_gate = %v, want %s", got, gap)
		}
	}

	refused("no_plan", id+" has no plan")
	runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
	refused("unreviewed", id+"'s current plan (v1, digest "+digest+") has no review")
	runCLIInDir(t, dir, "verdict", id, "pass", "--plan", digest, "--by", "author")
	refused("author_only", "has passed only its author's own review (author)")
	runCLIInDir(t, dir, "verdict", id, "fail", "--plan", digest, "--law", "carrying-cost", "--by", "ada")
	runCLIInDir(t, dir, "verdict", id, "pass", "--plan", digest, "--by", "grace")
	refused("failed", "ada failed it citing carrying-cost")
	runCLIInDir(t, dir, "update", id, "--plan", revised, "--by", "author")
	refused("unreviewed", "(v2, digest "+revisedDigest+") has no review; the reviews on record judged an earlier text")

	verdict := strings.Fields(runCLIInDir(t, dir, "verdict", id, "pass", "--plan", revisedDigest, "--by", "ada"))
	runCLIInDir(t, dir, "done", id)
	facts := writtenFacts(t, lastEvent(t))
	if review := strings.TrimSuffix(verdict[1], ":"); facts["done.plan_gate"] != "approved" || facts["done.plan_digest"] != revisedDigest || facts["done.plan_review"] != review {
		t.Errorf("lit done event facts = %v, want done.plan_gate approved, done.plan_digest %s and done.plan_review %s", facts, revisedDigest, review)
	}
	if status := strings.TrimSpace(runCLIInDir(t, dir, "show", id, "--field", "status")); status != "closed" {
		t.Errorf("after an approved lit done, status = %q, want closed", status)
	}
}

// A plan given when the ticket is filed is its filer's: lit new records who
// ran it, so the filer passing their own plan approves nothing.
func TestDoneRefusesAPlanItsFilerPassed(t *testing.T) {
	isolateEvents(t)
	dir, _, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "planned epic")
	id := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Planned at filing", "--topic", "demo", "--parent", epic, "--plan", gatePlan, "--by", "filer"))
	runCLIInDir(t, dir, "start", id, "--by", "filer")
	runCLIInDir(t, dir, "verdict", id, "pass", "--plan", string(model.DigestPlan(gatePlan)), "--by", "filer")
	_, err := runCLIInDirAllowError(t, dir, "done", id)
	if want := "has passed only its author's own review (filer)"; err == nil || commandErrorReason(err) != reasonPlanNotApproved || !strings.Contains(err.Error(), want) {
		t.Fatalf("lit done on a plan only its filer passed = %v, want reason %s naming %q", err, reasonPlanNotApproved, want)
	}
}

// A done on a ticket already closed finishes nothing, so the gate asks it for
// no plan, and its event says the gate found the ticket closed.
func TestDoneOnAClosedTicketAsksForNoPlan(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "close", id, "--resolution", "wontfix")
	runCLIInDir(t, dir, "done", id)
	if got := writtenFacts(t, lastEvent(t))["done.plan_gate"]; got != "already_closed" {
		t.Fatalf("lit done on a closed ticket: done.plan_gate = %v, want already_closed", got)
	}
}
