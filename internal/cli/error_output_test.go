package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
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
		// A retired flag fails the same way on every run, so neither retired
		// flag may fall to the default retry advice.
		{"unsupported output flag", unsupportedOutputFlagError(), "unsupported_flag"},
		{"unsupported continue flag", UnsupportedError{Message: "--continue is retired"}, "unsupported_flag"},
		{"generic", UsageError{Message: "bad"}, "usage_error"},
		// A write blocked by another store holder is its own reason, and wins over
		// the transient-contention fallthrough it unwraps to. The Cause is the real
		// ErrTransientGCContention sentinel (as production's is), so errors.Is(err,
		// ErrTransientGCContention) is true here too: reversing the errors.As and
		// errors.Is checks in commandErrorReason would flip this to
		// transient_gc_contention and fail — the ordering is actually guarded.
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
		// Both validation types share the one refusal reason: a deterministic
		// policy refusal must never fall to the default "Retry the command"
		// remediation.
		{"cli validation refusal", model.ValidationError{Message: "Do not set 'blocks' relationships between two issues in the same epic."}, "validation_refused"},
		{"storage validation refusal", model.ValidationError{Message: "priority out of range"}, "validation_refused"},
		// Both takeover-gate arms, and one wrapped.
		{"takeover without --take", takeoverUnconfirmedError{Message: "claimed here: … — this lane is claimed and active; pass --take to confirm the takeover"}, "takeover_unconfirmed"},
		{"takeover declined", takeoverUnconfirmedError{Message: "takeover declined"}, "takeover_unconfirmed"},
		{"takeover declined wrapped", fmt.Errorf("start: %w", takeoverUnconfirmedError{Message: "takeover declined"}), "takeover_unconfirmed"},
		// A managed template that cannot converge is refused deterministically
		// like a validation failure, but the act it calls for is editing a file
		// on disk — so it carries its own reason rather than inheriting
		// validation_refused's "adjust the command".
		{"template shape refusal", templateShapeError{Message: "template must contain either no markers or be exactly one such block"}, "template_shape_refused"},
		{
			"workspace busy",
			fmt.Errorf("another lit process is writing to this workspace; retry after it completes: %w", store.ErrWorkspaceBusy),
			"workspace_busy",
		},
		// The router's terminal answers are answers, not faults, and each names
		// a different act — so each is its own reason rather than both sharing
		// one, and neither may fall through to "command_failed".
		{"router scope exhausted", Exhausted{Epics: []string{"links-epic-abcd"}}, "scope_exhausted"},
		{"router no ready work", NoWork{}, "no_ready_work"},
		// A repository `lit init` has never run in is terminal: no rerun makes
		// the workspace exist. Both the bare sentinel and a wrapped one are
		// pinned, because the store returns it bare today and a caller adding
		// context later must not silently drop back to the default.
		{"workspace not initialized", store.ErrWorkspaceNotInitialized, "workspace_not_initialized"},
		// The two command-fixable members of the prefix family share
		// validation_refused: the act each asks for is "adjust the command",
		// which is exactly what that reason already means, and the flag or
		// command that resolves it is named by the message.
		{"issue prefix refused", workspace.ErrIssuePrefixRefused, "validation_refused"},
		{
			"issue prefix refused wrapped",
			fmt.Errorf("resolve workspace: %w", workspace.ErrIssuePrefixRefused),
			"validation_refused",
		},
		// The third member does not, for the same cause template_shape_refused
		// exists: a stored issue_prefix config.json refuses is cleared by editing
		// that file, and no command touches it, so validation_refused's "adjust
		// the command to satisfy it" would be false advice. Both rows sit after the sentinel
		// pair they unwrap to, because that unwrap is what keeps the exit code
		// shared — and is exactly what would silently reclassify them into the row
		// above if the typed arm were ever removed.
		{
			"stored prefix refused",
			workspace.StoredPrefixError{ConfigPath: "/w/config.json", Stored: "ab", Err: errors.New("too short")},
			"stored_prefix_refused",
		},
		{
			"stored prefix refused wrapped",
			fmt.Errorf("resolve workspace: %w", workspace.StoredPrefixError{ConfigPath: "/w/config.json", Stored: "ab", Err: errors.New("too short")}),
			"stored_prefix_refused",
		},
		{
			"workspace not initialized wrapped",
			fmt.Errorf("open store: %w", store.ErrWorkspaceNotInitialized),
			"workspace_not_initialized",
		},
		// A stat that failed for any reason but ENOENT is an unclassified fault,
		// and the retry-then-doctor default is the right advice for it. The
		// workspace_not_initialized arm must not widen to it.
		{"genuine stat fault stays unclassified", errors.New("stat database dir: permission denied"), "command_failed"},
		// A genuine fault reaching the same surface keeps its own reason: the
		// arms above dispatch on their concrete types, so they shadow nothing.
		{"genuine fault still classifies", CorruptionError{Message: "integrity_check failed"}, "corruption_detected"},
		// A container action carries its own split: the request the children
		// already satisfy needs nothing done, while the one they do not is a
		// deterministic refusal joining the existing validation reason. Both
		// must stay off the default's retry advice.
		{
			"container action already satisfied",
			model.ContainerActionError{
				ID: "links-epic-abcd", Action: model.ActionDone,
				Target: model.StateClosed, State: model.StateClosed,
				Progress: model.Progress{Total: 3, Closed: 3},
			},
			"state_already_holds",
		},
		{
			"container action refused",
			model.ContainerActionError{
				ID: "links-epic-abcd", Action: model.ActionDone,
				Target: model.StateClosed, State: model.StateOpen,
				Progress: model.Progress{Total: 3, Closed: 1},
			},
			"validation_refused",
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

// TestWriteCommandErrorWorkspaceWriteBlocked: a write refused because another
// process holds the store surfaces the holder-aware headline and its resolution
// steps — never the raw "database is read only" line as the whole message.
// [FRAMING:representation]
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

// TestWriteCommandErrorValidationRefusalNeverSaysRetry: a policy refusal
// (exit 3) is deterministic — retrying can never succeed — so its remediation
// must not tell the operator to retry, and must not point at `lit doctor`
// (nothing is wrong).
func TestWriteCommandErrorValidationRefusalNeverSaysRetry(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := model.ValidationError{Message: "Do not set 'blocks' relationships between two issues in the same epic.  Use rank to specify that one issue must be completed before another issue"}
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

// TestWriteCommandErrorUninitializedWorkspace pins the surface an agent
// actually reads. The condition is terminal — `lit init` has never run here —
// so the remediation must agree with the message body instead of contradicting
// it: no retry advice, and no referral to `lit doctor`, which reads the very
// workspace that is missing.
//
// The absence assertions alone would be vacuous (they hold of any answer that
// merely avoids the default), so the reason and exit code are pinned as data in
// the tables above, and this test also asserts the positive: the one act that
// resolves the condition is named.
func TestWriteCommandErrorUninitializedWorkspace(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := WriteCommandError(&stderr, store.ErrWorkspaceNotInitialized); code != ExitValidation {
		t.Fatalf("exitCode = %d, want %d (ExitValidation)", code, ExitValidation)
	}
	out := stderr.String()
	if !strings.Contains(out, "error (code=3): repository not initialized with lit — run 'lit init' first") {
		t.Fatalf("missing the message body at the precondition exit code: %q", out)
	}
	if strings.Contains(out, "Retry the command") {
		t.Fatalf("a terminal precondition must not carry the generic retry remediation: %q", out)
	}
	if strings.Contains(out, "lit doctor") {
		t.Fatalf("remediation must not send an agent to `lit doctor` for a workspace that does not exist: %q", out)
	}
	if !strings.Contains(out, "Do not retry unchanged") || !strings.Contains(out, "lit init") {
		t.Fatalf("remediation must say the condition is terminal and name `lit init`: %q", out)
	}
	// The terminal claim is about this command, not about all of them: saying
	// every store-touching command repeats this answer until a workspace exists
	// is false, because the write paths bootstrap one. A remediation is acted
	// on, not read for flavour, so an overstatement here is as much a defect as
	// retry advice. [LAW:no-silent-failure]
	if strings.Contains(out, "every store-touching command") {
		t.Fatalf("remediation must not claim every command repeats this answer; the write paths bootstrap: %q", out)
	}
}

// TestWriteCommandErrorRemoteUnreachable: a transport failure that survived
// the retry budget surfaces as the remote being unreachable — naming the
// transport symptom — with remediation aimed at the network, never at
// credentials or `lit doctor`.
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
