package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRejectsUnknownMode pins that an unrecognized mode — including the
// zero value — fails closed rather than falling through to a default arm.
//
// The zero value is the case that matters: [Mode] is a string, so a caller that
// forgets to pass one passes "" without the compiler noticing, and a table
// lookup that fell back to a default would hand that caller write access to a
// database it never asked to bootstrap. [LAW:no-silent-failure]
func TestOpenRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "dolt")
	for _, mode := range []Mode{"", "admin", "readwrite"} {
		st, err := Open(context.Background(), mode, dir, "test-workspace-id")
		if err == nil {
			st.Close()
			t.Fatalf("Open(%q) succeeded, want invalid-mode error", mode)
		}
		if !strings.Contains(err.Error(), "invalid storage engine mode") {
			t.Fatalf("Open(%q) error = %q, want invalid-mode error", mode, err)
		}
		// Failing closed means failing before any effect: a refused mode must
		// not have created the workspace on its way to the error.
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("Open(%q) created %s despite refusing the mode", mode, dir)
		}
	}
}

// TestEveryModeHasAnOpener pins that the declared vocabulary and the table that
// gives it meaning cannot drift apart.
//
// A mode constant added without a row would be a value callers can name and
// spell correctly, that fails at runtime with "invalid storage engine mode" —
// the message for a typo, delivered for a mode that exists. Neither a build nor
// a green suite would say so. [LAW:one-source-of-truth]
func TestEveryModeHasAnOpener(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{ReadWrite, ReadOnly, Sync} {
		if _, ok := openers[mode]; !ok {
			t.Errorf("mode %q is declared but openers has no row for it", mode)
		}
	}
	if len(openers) != 3 {
		t.Errorf("openers has %d rows, but only 3 modes are declared; an unreachable row is a mode nobody can ask for", len(openers))
	}
}
