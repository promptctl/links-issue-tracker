package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// `lit ls --include-archived` and `--include-deleted` are the only way a frozen
// ticket reaches a listing, and with `--columns blocked` they are the only way
// the readiness pipeline is ever asked about one: the workable commands filter
// those rows out long before annotating.
//
// The cell answers what the readiness registry says about the row, which is a
// different axis from whether anyone can act on the row. Retention is reported
// by `state`, in the same row, which is why these print `open+archived` and
// `open+deleted` beside the verdict — the reader gets both facts and neither
// column has to carry the other's meaning. That is the same rule which lets
// `blocked` appear on a closed ticket.
//
// Filtering frozen rows out of the readiness rung instead would put a second
// predicate in front of the registry and make `blocked` mean "startable AND in
// play" here while it means the registry's verdict everywhere else — the
// shorter list this ticket deleted, one layer up. [LAW:one-source-of-truth]
func TestBlockedColumnOnFrozenRows(t *testing.T) {
	for _, freeze := range []struct {
		name string
		flag string
		spec transitionSpec
	}{
		{name: "archived", flag: "--include-archived", spec: archiveSpec},
		{name: "deleted", flag: "--include-deleted", spec: deleteSpec},
	} {
		t.Run(freeze.name, func(t *testing.T) {
			h := newReadyTestHarness(t)

			blocker := h.createIssue(storage.CreateIssueInput{
				Prefix: "frozen", Title: "Blocker", Topic: "frozen", IssueType: "task", Description: "d",
			})
			gated := h.createIssue(storage.CreateIssueInput{
				Prefix: "frozen", Title: "Gated", Topic: "frozen", IssueType: "task", Description: "d",
			})
			h.addDependency(gated.ID, blocker.ID)
			ungated := h.createIssue(storage.CreateIssueInput{
				Prefix: "frozen", Title: "Ungated", Topic: "frozen", IssueType: "task", Description: "d",
			})

			cellOf := func(id string, args ...string) string {
				t.Helper()
				var out bytes.Buffer
				args = append([]string{"--columns", "id,blocked"}, args...)
				if err := runListWithStore(h.ctx, &out, h.ap.Store, workspaceReadyPolicy(h.ap), args); err != nil {
					t.Fatalf("lit ls %v: %v", args, err)
				}
				cells := fieldsOf(lineForID(t, out.String(), id))
				if len(cells) != 2 {
					t.Fatalf("row for %s: want 2 cells, got %v\nfull output:\n%s", id, cells, out.String())
				}
				return cells[1]
			}

			// Read both cells before freezing. Asserting the values afterwards
			// against these rather than against literals is what makes the test
			// about the freeze: it cannot pass by every row reading "-".
			wantGated, wantUngated := cellOf(gated.ID), cellOf(ungated.ID)
			if wantGated != "blocked" || wantUngated != "-" {
				t.Fatalf("before freezing: gated=%q ungated=%q, want %q and %q — "+
					"the fixture must carry both answers or freezing them proves nothing",
					wantGated, wantUngated, "blocked", "-")
			}

			var sink bytes.Buffer
			for _, id := range []string{gated.ID, ungated.ID} {
				if err := runTransition(h.ctx, &sink, h.ap, []string{id}, freeze.spec); err != nil {
					t.Fatalf("runTransition(%s %s): %v", freeze.spec.name, id, err)
				}
			}

			// The rows are gone from a plain listing — otherwise the flag below
			// proves nothing about frozen rows.
			var plain bytes.Buffer
			if err := runListWithStore(h.ctx, &plain, h.ap.Store, workspaceReadyPolicy(h.ap),
				[]string{"--columns", "id,blocked"}); err != nil {
				t.Fatalf("lit ls after %s: %v", freeze.spec.name, err)
			}
			if got := plain.String(); strings.Contains(got, gated.ID) {
				t.Fatalf("%s is still listed without %s after %s; this test would then be "+
					"asserting nothing about frozen rows:\n%s", gated.ID, freeze.flag, freeze.spec.name, got)
			}

			if got := cellOf(gated.ID, freeze.flag); got != wantGated {
				t.Errorf("%s blocked cell = %q after %s, want %q — %s did not change what the "+
					"readiness registry says about the row, only whether anyone can act on it, "+
					"and `state` is the column that reports that",
					gated.ID, got, freeze.spec.name, wantGated, freeze.spec.name)
			}
			if got := cellOf(ungated.ID, freeze.flag); got != wantUngated {
				t.Errorf("%s blocked cell = %q after %s, want %q — a frozen row with nothing "+
					"gating it must not start reading as blocked",
					ungated.ID, got, freeze.spec.name, wantUngated)
			}
		})
	}
}
