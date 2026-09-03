package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

func TestCommandErrorReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unknown command", UnknownCommandError{Command: "wat"}, "unknown_command"},
		{"not found", storage.NotFoundError{Entity: "issue", ID: "lit-abc"}, "entity_not_found"},
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
		{
			"remote unreachable",
			store.RemoteUnreachableError{Attempts: 4, Symptom: "ssh: connect to host github.com port 22: Connection refused", Cause: errors.New("wrapped")},
			"remote_unreachable",
		},
		// Both validation types share the one refusal reason (links-sync-r779
		// defect 3): a deterministic policy refusal must never fall to the
		// default "Retry the command" remediation.
		{"cli validation refusal", ValidationError{Message: "Do not set 'blocks' relationships between two issues in the same epic."}, "validation_refused"},
		{"storage validation refusal", storage.ValidationError{Message: "priority out of range"}, "validation_refused"},
		{
			"workspace busy",
			fmt.Errorf("another lit process is writing to this workspace; retry after it completes: %w", store.ErrWorkspaceBusy),
			"workspace_busy",
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
	t.Parallel()
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
	t.Parallel()
	var stderr bytes.Buffer
	err := store.WorkspaceWriteBlockedError{
		Cause: errors.New("dolt commit working set: Error 1105: cannot update manifest: database is read only"),
	}
	// A write blocked by another holder is a retryable resource-busy condition, not
	// a merge conflict — it exits ExitGeneric, the same family as "workspace busy".
	if code := WriteCommandError(&stderr, err); code != ExitGeneric {
		t.Fatalf("exitCode = %d, want %d (ExitGeneric)", code, ExitGeneric)
	}
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

// TestWriteCommandErrorValidationRefusalNeverSaysRetry pins defect 3 of
// links-sync-r779's comment: a policy refusal (exit 3) is deterministic —
// retrying can never succeed — so its remediation must not tell the operator
// to retry, and must not point at `lit doctor` (nothing is wrong).
func TestWriteCommandErrorValidationRefusalNeverSaysRetry(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := ValidationError{Message: "Do not set 'blocks' relationships between two issues in the same epic.  Use rank to specify that one issue must be completed before another issue"}
	if code := WriteCommandError(&stderr, err); code != ExitValidation {
		t.Fatalf("exitCode = %d, want %d", code, ExitValidation)
	}
	out := stderr.String()
	if strings.Contains(out, "Retry the command") || strings.Contains(out, "lit doctor") {
		t.Fatalf("deterministic refusal must not carry the generic retry remediation: %q", out)
	}
	if !strings.Contains(out, "Do not retry unchanged") {
		t.Fatalf("missing the no-retry remediation: %q", out)
	}
}

// TestWriteCommandErrorRemoteUnreachable pins defect 2 of links-sync-r779: a
// transport failure that survived the retry budget surfaces as the remote
// being unreachable — naming the transport symptom — with remediation aimed at
// the network, never at credentials or `lit doctor`.
func TestWriteCommandErrorRemoteUnreachable(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := fmt.Errorf("push remote %q: %w", "origin", store.RemoteUnreachableError{
		Attempts: 4,
		Symptom:  "ssh: connect to host github.com port 22: Connection refused",
		Cause:    errors.New("git authentication required but interactive prompting is disabled"),
	})
	if code := WriteCommandError(&stderr, err); code != ExitGeneric {
		t.Fatalf("exitCode = %d, want %d", code, ExitGeneric)
	}
	out := stderr.String()
	if !strings.Contains(out, "remote unreachable: ssh: connect to host github.com port 22: Connection refused") {
		t.Fatalf("missing transport-symptom headline: %q", out)
	}
	if !strings.Contains(out, "credentials are not the problem") {
		t.Fatalf("remediation must correct the auth misdiagnosis: %q", out)
	}
	if strings.Contains(out, "lit doctor") {
		t.Fatalf("a network failure must not point at lit doctor (nothing is wrong locally): %q", out)
	}
}
