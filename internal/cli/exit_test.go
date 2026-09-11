package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestExitCodeMappings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: ExitOK},
		{name: "not found", err: storage.NotFoundError{Entity: "issue", ID: "lit-1"}, want: ExitNotFound},
		{name: "merge conflict typed", err: MergeConflictError{Message: "sync conflict"}, want: ExitConflict},
		{name: "corruption typed", err: CorruptionError{Message: "integrity_check failed"}, want: ExitCorruption},
		{name: "usage message", err: UsageError{Message: "usage: lit foo"}, want: ExitUsage},
		{name: "validation required", err: ValidationError{Message: "--title is required"}, want: ExitValidation},
		{name: "validation unknown command", err: UnknownCommandError{Command: "abc"}, want: ExitValidation},
		{name: "usage unknown flag", err: UsageError{Message: "unknown flag: --json"}, want: ExitUsage},
		{name: "string conflict", err: MergeConflictError{Message: "sync import conflict"}, want: ExitConflict},
		{name: "store validation", err: storage.ValidationError{Message: "issue type must be task, feature, bug, chore, or epic"}, want: ExitValidation},
		{name: "unsupported feature", err: UnsupportedError{Message: "unsupported --format \"csv\"", Feature: "--format"}, want: ExitValidation},
		{name: "outside workspace", err: OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}, want: ExitGeneric},
		{name: "generic", err: ValidationError{Message: "boom"}, want: ExitValidation},
		// Both of `lit next`'s terminal answers exit ExitNoWork: the command ran
		// correctly and simply has no ticket to hand back. Sharing one code is
		// deliberate — the caller's question is binary, and which emptiness it
		// was is carried by the reason and the message (links-cli-cpou).
		{name: "router scope exhausted", err: Exhausted{Epics: []string{"links-epic-abcd"}}, want: ExitNoWork},
		{name: "router no ready work", err: NoWork{}, want: ExitNoWork},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ExitCode(tc.err)
			if got != tc.want {
				t.Fatalf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}
