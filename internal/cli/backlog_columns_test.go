package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
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
func runBacklogColumns(t *testing.T, expr string) (string, error) {
	t.Helper()
	h := newReadyTestHarness(t)
	var stdout bytes.Buffer
	err := runWorkable(h.ctx, &stdout, h.ap, []string{"--columns", expr}, backlogView)
	return stdout.String(), err
}

// TestBacklogRejectsUnknownColumn is the reject half on the backlog surface.
// The `lit ls` table covers the vocabulary itself; what is specific here is
// that the refusal reaches the caller as a usage error naming the offender,
// on a view whose flag set is assembled differently (optionalString, gated on
// hasColumns) than `lit ls`'s.
func TestBacklogRejectsUnknownColumn(t *testing.T) {
	for _, expr := range []string{"bogus", "status", "id,bogus,title"} {
		t.Run(expr, func(t *testing.T) {
			out, err := runBacklogColumns(t, expr)
			if err == nil {
				t.Fatalf("backlog --columns %q: want error, got nil (output:\n%s)", expr, out)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("backlog --columns %q exit code = %d, want %d (ExitUsage)", expr, got, ExitUsage)
			}
			for _, valid := range sortedColumnNames() {
				if !strings.Contains(err.Error(), valid) {
					t.Errorf("backlog --columns %q error %q omits valid column %q", expr, err, valid)
				}
			}
			if out != "" {
				t.Errorf("backlog --columns %q printed before rejecting:\n%s", expr, out)
			}
		})
	}
}

// TestBacklogAcceptsValidColumns keeps the reject test honest: without it, a
// backlog whose --columns flag was broken outright would pass every case above
// for the wrong reason.
func TestBacklogAcceptsValidColumns(t *testing.T) {
	out, err := runBacklogColumns(t, "id,rank,title")
	if err != nil {
		t.Fatalf("backlog --columns id,rank,title: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("backlog --columns id,rank,title: accepted but rendered nothing")
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
