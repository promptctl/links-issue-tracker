package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
		{name: "unsupported flag", err: UnsupportedError{Message: "--output is no longer supported; omit it for text output"}, want: ExitValidation},
		// "Am I somewhere lit can work?" has two negative answers, and both exit
		// ExitValidation rather than ExitGeneric: the environment is not ready
		// and no retry changes that, which a script must be able to tell from
		// "lit is broken" without parsing the English (links-cli-errors-yfbg).
		// The act each calls for differs and is carried by the reason.
		{name: "outside workspace", err: OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}, want: ExitValidation},
		{name: "workspace not initialized", err: store.ErrWorkspaceNotInitialized, want: ExitValidation},
		{name: "workspace not initialized wrapped", err: fmt.Errorf("open store: %w", store.ErrWorkspaceNotInitialized), want: ExitValidation},
		// A prefix lit cannot settle on is a self-fixable precondition, not
		// "lit is broken" — ExitGeneric meant both, and a script could only
		// tell them apart by parsing the English (links-init-hn19).
		{name: "issue prefix refused", err: workspace.ErrIssuePrefixRefused, want: ExitValidation},
		{name: "issue prefix refused wrapped", err: fmt.Errorf("resolve workspace: %w", workspace.ErrIssuePrefixRefused), want: ExitValidation},
		// The typed member keeps the family's code: it carries a different
		// remediation, not a different exit contract, and it reaches this arm only
		// through its Unwrap. Dropping that Unwrap would move it to ExitGeneric,
		// which is the "lit is broken" code this ticket moved it off.
		{
			name: "stored prefix refused",
			err:  workspace.StoredPrefixError{ConfigPath: "/w/config.json", Stored: "ab", Err: errors.New("too short")},
			want: ExitValidation,
		},
		// The genuine fault on the same path keeps the unclassified-fault code.
		{name: "genuine stat fault", err: errors.New("stat database dir: permission denied"), want: ExitGeneric},
		{name: "generic", err: ValidationError{Message: "boom"}, want: ExitValidation},
		// Both of `lit next`'s terminal answers exit ExitNoWork: the command ran
		// correctly and simply has no ticket to hand back. Sharing one code is
		// deliberate — the caller's question is binary, and which emptiness it
		// was is carried by the reason and the message (links-cli-cpou).
		{name: "router scope exhausted", err: Exhausted{Epics: []string{"links-epic-abcd"}}, want: ExitNoWork},
		{name: "router no ready work", err: NoWork{}, want: ExitNoWork},
		// A container action splits on whether the children already establish
		// the state that was asked for: a satisfied request changed nothing and
		// shares the "nothing to hand back" code, while a refusal is an ordinary
		// domain-constraint rejection. Neither is ExitGeneric any more
		// (links-cli-errors-1u9g). The command-driven proof is in
		// container_action_error_test.go; these two pin the mapping itself.
		{
			name: "container action already satisfied",
			err: model.ContainerActionError{
				ID: "links-epic-abcd", Action: model.ActionDone,
				Target: model.StateClosed, State: model.StateClosed,
				Progress: model.Progress{Total: 3, Closed: 3},
			},
			want: ExitNoWork,
		},
		{
			name: "container action refused",
			err: model.ContainerActionError{
				ID: "links-epic-abcd", Action: model.ActionStart,
				Target: model.StateInProgress, State: model.StateClosed,
				Progress: model.Progress{Total: 3, Closed: 3},
			},
			want: ExitValidation,
		},
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
