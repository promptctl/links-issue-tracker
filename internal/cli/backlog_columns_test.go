package cli

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// `lit backlog` gained the --columns rejection at the same moment `lit ls` did,
// because both run through parseColumnSelection in runWorkable — but the two
// surfaces fail differently, and only this one can fail by PRINTING FIRST.
// runWorkable emits the sync-staleness warning before it renders, so the
// rejection is correct only while the parse sits above that warning. Nothing
// but an assertion holds those two lines in that order.

// runBacklogColumns runs `lit backlog --columns expr` through the workable
// runner and returns stdout alongside the error, so a case can assert on both.
// An error that still printed is the failure this boundary exists to prevent.
//
// It takes the harness rather than building one, because a backlog with no rows
// renders only the preamble and "(backlog empty)" — a constant that satisfies
// any "did it print something" assertion no matter what the projection did.
// Every caller here seeds rows first.
func runBacklogColumns(h readyTestHarness, expr string) (string, error) {
	h.t.Helper()
	var stdout bytes.Buffer
	err := runWorkable(h.ctx, &stdout, h.ap, []string{"--columns", expr}, backlogView)
	return stdout.String(), err
}

var backlogRowPrefix = regexp.MustCompile(`^\s*\d+\.\s+`)

// backlogCells returns the projected cells of the backlog row for id. Rows carry
// a "NN. " list prefix and join their columns with two spaces; the per-row
// context lines beneath a row carry no such prefix and are skipped, so what is
// returned is the projection itself and nothing else.
func backlogCells(t *testing.T, out, id string) []string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !backlogRowPrefix.MatchString(line) {
			continue
		}
		cells := regexp.MustCompile(`\s{2,}`).Split(backlogRowPrefix.ReplaceAllString(line, ""), -1)
		if len(cells) > 0 && cells[0] == id {
			return cells
		}
	}
	t.Fatalf("no backlog row for %q in:\n%s", id, out)
	return nil
}

// TestBacklogRejectsUnknownColumn is the reject half on the backlog surface.
// The `lit ls` table covers the vocabulary itself; what is specific here is
// that the refusal reaches the caller as a usage error naming the offender,
// on a view whose flag set is assembled differently (optionalString, gated on
// hasColumns) than `lit ls`'s. The store is seeded so that an accepted run
// would have rows to print — an empty stdout below therefore means the command
// refused, not that it had nothing to say.
func TestBacklogRejectsUnknownColumn(t *testing.T) {
	for _, tc := range []struct{ expr, unknown string }{
		{expr: "bogus", unknown: "bogus"},
		{expr: "status", unknown: "status"},
		{expr: "id,bogus,title", unknown: "bogus"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			h := newReadyTestHarness(t)
			h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Row one", Topic: "cols", IssueType: "task", Priority: 0})

			out, err := runBacklogColumns(h, tc.expr)
			if err == nil {
				t.Fatalf("backlog --columns %q: want error, got nil (output:\n%s)", tc.expr, out)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("backlog --columns %q exit code = %d, want %d (ExitUsage)", tc.expr, got, ExitUsage)
			}
			for _, valid := range sortedColumnNames() {
				if !strings.Contains(err.Error(), valid) {
					t.Errorf("backlog --columns %q error %q omits valid column %q", tc.expr, err, valid)
				}
			}
			// Quoted, for the same reason the `lit ls` rows are: the message
			// carries the whole valid-columns list, so a bare substring match
			// can be satisfied by a name the caller never typed.
			if quoted := fmt.Sprintf("%q", tc.unknown); !strings.Contains(err.Error(), quoted) {
				t.Errorf("backlog --columns %q error %q does not name the offender as %s", tc.expr, err, quoted)
			}
			if out != "" {
				t.Errorf("backlog --columns %q printed before rejecting:\n%s", tc.expr, out)
			}
		})
	}
}

// TestBacklogAcceptsValidColumns pins the projection actually reaching the
// renderer. Asserting only that output is non-empty proves nothing here: an
// empty backlog prints a constant preamble, and even a seeded one prints row
// text regardless of which columns were selected. So this asserts the row's
// exact cells — a regression that dropped knobs.columns and rendered
// defaultColumns() instead would print `id state topic title` and fail.
func TestBacklogAcceptsValidColumns(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Projected row", Topic: "cols", IssueType: "task", Priority: 0})

	out, err := runBacklogColumns(h, "id,rank,title")
	if err != nil {
		t.Fatalf("backlog --columns id,rank,title: %v", err)
	}
	got := backlogCells(t, out, issue.ID)
	want := []string{issue.ID, emptyDash(issue.Rank), "Projected row"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("backlog --columns id,rank,title cells = %v, want %v\nfull output:\n%s", got, want, out)
	}
}

// TestBacklogRendersRelationColumns is the regression pin for the bug that
// `--columns` validation on this surface created: `columnsFlagUsage()` began
// advertising `parent` and `blocked` on `lit backlog`, and parseColumnSelection
// began accepting them, while printBacklogOutput still rendered through a nil
// relations map — so both cells were "-" on every row, including rows the
// context line directly below described as blocked.
//
// "-" is the same value that honestly means "no parent" and "not blocked", so
// the failure was indistinguishable from a true answer rather than visible as
// one. Asserting the real ids and the `blocked` label is what makes reverting
// the derivation in runWorkable fail here instead of printing dashes.
func TestBacklogRendersRelationColumns(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Epic", Topic: "cols", IssueType: "epic", Priority: 1})
	child := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Child", Topic: "cols", IssueType: "task", Priority: 0, ParentID: epic.ID})
	blocker := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Blocker", Topic: "cols", IssueType: "task", Priority: 0})
	h.addDependency(child.ID, blocker.ID)

	out, err := runBacklogColumns(h, "id,parent,blocked")
	if err != nil {
		t.Fatalf("backlog --columns id,parent,blocked: %v", err)
	}

	got := backlogCells(t, out, child.ID)
	want := []string{child.ID, epic.ID, "blocked"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("blocked child cells = %v, want %v\nfull output:\n%s", got, want, out)
	}

	// The unblocked, parentless blocker is the control: it proves "-" still
	// reaches the page for a row that genuinely has no parent and no live
	// dependency, so the assertion above is reading real data rather than a
	// map that happens to be populated for everything.
	gotBlocker := backlogCells(t, out, blocker.ID)
	wantBlocker := []string{blocker.ID, "-", "-"}
	if strings.Join(gotBlocker, "|") != strings.Join(wantBlocker, "|") {
		t.Errorf("blocker cells = %v, want %v\nfull output:\n%s", gotBlocker, wantBlocker, out)
	}
}

// TestBacklogBlockedColumnAgreesWithTheContextLine covers the blocker that has
// no dependency edge at all. A leaf whose only blocker is the sibling gate used
// to render "-" under `blocked` on the line directly above its own
// "blocked: earlier sibling X still open" context line — the column asked
// DependsOn while the line asked the readiness classifier, so one row said both
// things at once.
//
// The assertion is deliberately the pair, not the cell: it reads the column and
// the context line out of the same output and requires them to agree, so any
// future predicate that drifts from the annotation registry fails here whatever
// the two happen to say.
func TestBacklogBlockedColumnAgreesWithTheContextLine(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Epic", Topic: "cols", IssueType: "epic", Priority: 1})
	first := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "First", Topic: "cols", IssueType: "task", Priority: 0, ParentID: epic.ID})
	second := h.createIssue(storage.CreateIssueInput{Prefix: "cols", Title: "Second", Topic: "cols", IssueType: "task", Priority: 1, ParentID: epic.ID})

	out, err := runBacklogColumns(h, "id,blocked")
	if err != nil {
		t.Fatalf("backlog --columns id,blocked: %v", err)
	}

	// The gate has to actually be firing, or the agreement below is vacuous.
	if !strings.Contains(out, "earlier sibling "+first.ID+" still open") {
		t.Fatalf("sibling gate did not fire for %s, so this proves nothing:\n%s", second.ID, out)
	}

	if got := backlogCells(t, out, second.ID); strings.Join(got, "|") != second.ID+"|blocked" {
		t.Errorf("sibling-gated row cells = %v, want [%s blocked] — the column and the "+
			"context line beneath it disagree\nfull output:\n%s", got, second.ID, out)
	}
	if got := backlogCells(t, out, first.ID); strings.Join(got, "|") != first.ID+"|-" {
		t.Errorf("ungated row cells = %v, want [%s -]\nfull output:\n%s", got, first.ID, out)
	}
}

// TestBacklogHelpEnumeratesValidColumns: backlog builds its --columns flag
// through optionalString rather than fs.String, so its help text is a separate
// call site from `lit ls`'s and can drift out of the shared usage string.
func TestBacklogHelpEnumeratesValidColumns(t *testing.T) {
	h := newReadyTestHarness(t)
	var stdout bytes.Buffer
	// --help is answered by the flag parser, which prints and then reports it
	// via a sentinel error; the output is what this asserts on.
	_ = runWorkable(h.ctx, &stdout, h.ap, []string{"--help"}, backlogView)
	for _, name := range sortedColumnNames() {
		if !strings.Contains(stdout.String(), name) {
			t.Errorf("`lit backlog --help` does not enumerate column %q:\n%s", name, stdout.String())
		}
	}
}

// TestBacklogRejectsUnknownColumnBeforeTheSyncWarning pins the ordering, and it
// is the reason this file uses the heavier clone fixture rather than the
// in-process harness above: the assertion is only worth anything on a workspace
// where the warning provably DOES fire. The fixture is the one
// sync_staleness_e2e_test.go uses for exactly that — a clone with one unpushed
// change — so the first half of this test establishes that a plain `lit backlog`
// there really does warn, and the second half shows the rejected run prints none
// of it. Without the first half, a fixture that silently stopped warning would
// turn the real assertion vacuous while it kept passing.
func TestBacklogRejectsUnknownColumnBeforeTheSyncWarning(t *testing.T) {
	dir, _ := unpushedCloneWithOneLocalChange(t)

	warned := runCLIInDir(t, dir, "backlog")
	if !strings.Contains(warned, "sync:") || !strings.Contains(warned, "not pushed") {
		t.Fatalf("fixture no longer warns, so the ordering assertion below proves nothing:\n%s", warned)
	}

	out, err := runCLIInDirAllowError(t, dir, "backlog", "--columns", "bogus")
	if err == nil {
		t.Fatalf("backlog --columns bogus: want error, got nil (output:\n%s)", out)
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("backlog --columns bogus exit code = %d, want %d (ExitUsage)", got, ExitUsage)
	}
	// Run returns the usage error rather than writing it, so the offender is
	// asserted on the error and the writers carry only what the command chose
	// to print — which is what makes the emptiness below meaningful.
	if quoted := fmt.Sprintf("%q", "bogus"); !strings.Contains(err.Error(), quoted) {
		t.Errorf("backlog --columns bogus error %q does not name the offender as %s", err, quoted)
	}
	for _, marker := range []string{"sync:", "not pushed", "lit sync push"} {
		if strings.Contains(out, marker) {
			t.Errorf("backlog --columns bogus emitted the sync warning %q before rejecting — the parse has fallen below printSyncStalenessWarning in runWorkable:\n%s", marker, out)
		}
	}
	// The same fixture printed a warning a moment ago, so an empty stream here
	// is the ordering itself: the refusal beat everything runWorkable writes.
	if out != "" {
		t.Errorf("backlog --columns bogus printed before rejecting:\n%s", out)
	}
}
