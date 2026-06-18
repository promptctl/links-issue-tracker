package cli

import (
	"bytes"
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
