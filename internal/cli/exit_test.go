package cli

import (
	"errors"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/store"
)

func TestExitCodeMappings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: ExitOK},
		{name: "not found", err: store.NotFoundError{Entity: "issue", ID: "lit-1"}, want: ExitNotFound},
		{name: "merge conflict typed", err: MergeConflictError{Message: "sync conflict"}, want: ExitConflict},
		{name: "corruption typed", err: CorruptionError{Message: "integrity_check failed"}, want: ExitCorruption},
		{name: "usage message", err: errors.New("usage: lit foo"), want: ExitUsage},
		{name: "validation required", err: errors.New("--title is required"), want: ExitValidation},
		{name: "beads migration required typed", err: BeadsMigrationRequiredError{}, want: ExitValidation},
		{name: "validation unknown command", err: errors.New("unknown command \"abc\""), want: ExitValidation},
		{name: "string conflict", err: errors.New("sync import conflict"), want: ExitConflict},
		{name: "generic", err: errors.New("boom"), want: ExitGeneric},
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
