package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A plan review is recorded and decides nothing: lit done finishes a ticket
// whose plan has no review, or a failing one, the same way, and says nothing
// about the plan.
func TestDoneReadsNoPlanReview(t *testing.T) {
	t.Parallel()
	isolateEvents(t)
	dir, _, _ := statuslineCheckout(t)
	epic := newEpicInDir(t, dir, "planned epic")
	const plan = "Close the ticket; nothing is added."
	digest := string(model.DigestPlan(plan))

	for _, tc := range []struct {
		name   string
		review func(t *testing.T, id string)
	}{
		{"no plan", func(*testing.T, string) {}},
		{"never reviewed", func(t *testing.T, id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
		}},
		{"failed", func(t *testing.T, id string) {
			runCLIInDir(t, dir, "update", id, "--plan", plan, "--by", "author")
			runCLIInDir(t, dir, "verdict", id, "fail", "--plan", digest, "--law", "carrying-cost", "--by", "ada")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := extractTicketID(t, runCLIInDir(t, dir, "new", "--title", "Plan "+tc.name, "--topic", "demo", "--parent", epic, "--by", "filer"))
			runCLIInDir(t, dir, "start", id, "--by", "agent")
			tc.review(t, id)

			out := runCLIInDir(t, dir, "done", id, "--by", "agent")
			if status := strings.TrimSpace(runCLIInDir(t, dir, "show", id, "--field", "status")); status != "closed" {
				t.Fatalf("lit done with %s: status = %q, want closed", tc.name, status)
			}
			if strings.Contains(out, "warning:") {
				t.Errorf("lit done with %s printed %q; want no warning", tc.name, out)
			}
		})
	}
}
