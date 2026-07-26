package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

func TestCommandErrorReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unknown command", UnknownCommandError{Command: "wat"}, "unknown_command"},
		{"not found", store.NotFoundError{Entity: "issue", ID: "lit-abc"}, "entity_not_found"},
		{"unsupported output flag", UnsupportedError{Feature: "--output"}, "unsupported_output_flag"},
		{"generic", UsageError{Message: "bad"}, "usage_error"},
		// A write blocked by another store holder is its own reason, and wins over
		// the transient-contention fallthrough it unwraps to. The Cause is the real
		// ErrTransientGCContention sentinel (as production's is), so errors.Is(err,
		// ErrTransientGCContention) is true here too: reversing the errors.As and
		// errors.Is checks in commandErrorReason would flip this to
		// transient_gc_contention and fail — the ordering is actually guarded.
		// links-sync-s3r6 #3.
		{
			"workspace write blocked",
			store.WorkspaceWriteBlockedError{Cause: store.ErrTransientGCContention},
			"workspace_write_blocked",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := commandErrorReason(tc.err); got != tc.want {
				t.Fatalf("commandErrorReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteCommandError pins the text error surface: the code+message line plus
// the actionable remediation for the error's typed reason. Text is the one
// canonical surface, so the remediation guidance reaches every caller.
func TestWriteCommandError(t *testing.T) {
	var stderr bytes.Buffer
	exitCode := WriteCommandError(&stderr, UnknownCommandError{Command: "unknown"})
	if exitCode != ExitValidation {
		t.Fatalf("exitCode = %d, want %d", exitCode, ExitValidation)
	}
	out := stderr.String()
	if !strings.Contains(out, "error (code=3): unknown command \"unknown\"") {
		t.Fatalf("missing error line: %q", out)
	}
	if !strings.Contains(out, "remediation: Run `lit --help`") {
		t.Fatalf("missing remediation line: %q", out)
	}
}

// TestWriteCommandErrorWorkspaceWriteBlocked pins defect #3 of links-sync-s3r6:
// a write refused because another process holds the store surfaces the holder-
// aware headline and its resolution steps — never the raw "database is read only"
// line as the whole message. [FRAMING:representation]
func TestWriteCommandErrorWorkspaceWriteBlocked(t *testing.T) {
	var stderr bytes.Buffer
	err := store.WorkspaceWriteBlockedError{
		Cause: errors.New("dolt commit working set: Error 1105: cannot update manifest: database is read only"),
	}
	WriteCommandError(&stderr, err)
	out := stderr.String()
	if !strings.Contains(out, "another lit process is holding this workspace open for writing") {
		t.Fatalf("missing holder-aware headline: %q", out)
	}
	if !strings.Contains(out, "remediation:") || !strings.Contains(out, "ps aux | grep") {
		t.Fatalf("missing holder remediation steps: %q", out)
	}
	// The backend cause is preserved for diagnosis (demoted behind the holder
	// sentence, not the headline). Assert the concrete cause text, not the store's
	// wrapping format, so a rename of that parenthetical can't break this contract.
	// [LAW:behavior-not-structure]
	if !strings.Contains(out, "cannot update manifest: database is read only") {
		t.Fatalf("backend cause not preserved for diagnosis: %q", out)
	}
}
