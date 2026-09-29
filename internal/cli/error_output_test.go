package cli

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// reasonCodes is the published reason vocabulary, each token with the exit code
// it arrives on, written out by hand: the expected code must not come from
// ExitCode, the function under test. TestCommandReasonVocabulary holds it equal
// to the constants in error_output.go and to the table in docs/cli-reference.md.
var reasonCodes = map[commandReason]int{
	"bulk_partial_failure":      1,
	"command_failed":            1,
	"corruption_detected":       7,
	"entity_not_found":          4,
	"merge_conflict":            5,
	"no_ready_work":             6,
	"outside_git_workspace":     3,
	"owner_approval_required":   5,
	"remote_unreachable":        1,
	"retired_command":           3,
	"scope_exhausted":           6,
	"state_already_holds":       6,
	"stored_prefix_refused":     3,
	"sync_divergence":           5,
	"takeover_unconfirmed":      3,
	"template_shape_refused":    3,
	"transient_gc_contention":   1,
	"unknown_command":           3,
	"unsupported_flag":          3,
	"usage_error":               2,
	"validation_refused":        3,
	"workspace_busy":            1,
	"workspace_not_initialized": 3,
	"workspace_schema_ahead":    3,
	"workspace_write_blocked":   1,
}

func TestCommandErrorReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want commandReason
	}{
		{"merge conflict", MergeConflictError{Message: "sync conflict"}, "merge_conflict"},
		{
			"sync divergence",
			SyncFailureError{Failure: SyncFailure{Class: syncFailureProseHeld, Remote: "origin", Branch: "master"}},
			"sync_divergence",
		},
		{
			"owner approval required",
			ownerApprovalRefusalError{
				Approval: store.OwnerApprovalRequiredError{
					Choice: storage.TakeLocal, ApprovalToken: "deadbeef0123",
					LocalHead: "aaaaaaaaaaaaaaaaaaaa", RemoteHead: "bbbbbbbbbbbbbbbbbbbb",
					Inventory: &storage.UnrelatedInventory{OnlyLocal: []string{"proj-mine"}},
				},
				Remote: "origin", Branch: "master",
			},
			"owner_approval_required",
		},
		{"retired command", RetiredCommandError{Command: "ready", Replacement: "use `lit next`"}, "retired_command"},
		{"outside git workspace", OutsideWorkspaceError{Message: "links requires running inside a git repository/worktree"}, "outside_git_workspace"},
		{"transient gc contention", store.ErrTransientGCContention, "transient_gc_contention"},
		{
			"bulk partial failure",
			BulkFailureError{Failures: []itemFailure{{IssueID: "lit-abc", Err: storage.NotFoundError{Entity: "issue", ID: "lit-abc"}}}},
			"bulk_partial_failure",
		},
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
		// A binary older than the workspace is not refused for its command, so
		// it takes its own reason rather than validation_refused's "adjust the
		// command"; the traversal refusals are refused for their --to, so they
		// are model.Refusal and take validation_refused.
		{"workspace schema ahead", schemaAheadError(), "workspace_schema_ahead"},
		{"workspace schema ahead wrapped", fmt.Errorf("open store: %w", schemaAheadError()), "workspace_schema_ahead"},
		{"upgrade target behind wrapped", fmt.Errorf("upgrade: %w", upgradeTargetBehindError()), "validation_refused"},
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
	// Asserted on the rendered header, not on commandErrorReason: the header
	// is where a caller reads the reason, so a reason computed correctly and
	// printed nowhere must fail here.
	produced := map[commandReason]bool{}
	for _, tc := range tests {
		tc := tc
		produced[tc.want] = true
		t.Run(tc.name, func(t *testing.T) {
			code, ok := reasonCodes[tc.want]
			if !ok {
				t.Fatalf("reason %q is not in reasonCodes", tc.want)
			}
			var stderr bytes.Buffer
			WriteCommandError(&stderr, tc.err)
			header, _, _ := strings.Cut(stderr.String(), "\n")
			wantPrefix := fmt.Sprintf("error (code=%d, reason=%s): ", code, tc.want)
			if !strings.HasPrefix(header, wantPrefix) {
				t.Fatalf("header = %q, want prefix %q", header, wantPrefix)
			}
		})
	}
	// Every published reason is rendered by at least one real error above, so
	// no row of the vocabulary is a token nothing prints.
	for reason := range reasonCodes {
		if !produced[reason] {
			t.Errorf("no case renders reason %q", reason)
		}
	}
}

// TestCommandReasonVocabulary holds the published reason list to the code:
// the constants in error_output.go, reasonCodes, and the table in
// docs/cli-reference.md name the same tokens, and the doc gives each the code
// reasonCodes does. A script branches on these tokens, so a reason added,
// renamed, or moved to another code without the reference changing is a
// broken contract, not a doc nit.
func TestCommandReasonVocabulary(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "error_output.go", nil, 0)
	if err != nil {
		t.Fatalf("parse error_output.go: %v", err)
	}
	declared := map[commandReason]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "commandReason" {
				continue
			}
			for _, v := range vs.Values {
				lit, err := strconv.Unquote(v.(*ast.BasicLit).Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", v.(*ast.BasicLit).Value, err)
				}
				declared[commandReason(lit)] = true
			}
		}
	}
	for reason := range declared {
		if _, ok := reasonCodes[reason]; !ok {
			t.Errorf("error_output.go declares %q; reasonCodes does not list it", reason)
		}
	}
	for reason := range reasonCodes {
		if !declared[reason] {
			t.Errorf("reasonCodes lists %q; error_output.go declares no such constant", reason)
		}
	}

	doc, err := os.ReadFile("../../docs/cli-reference.md")
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| ([0-9]) \\|")
	documented := map[commandReason]int{}
	for _, m := range row.FindAllStringSubmatch(string(doc), -1) {
		documented[commandReason(m[1])] = int(m[2][0] - '0')
	}
	for reason, code := range reasonCodes {
		got, ok := documented[reason]
		if !ok {
			t.Errorf("docs/cli-reference.md has no row for reason %q", reason)
			continue
		}
		if got != code {
			t.Errorf("docs/cli-reference.md gives %q code %d; it arrives on %d", reason, got, code)
		}
	}
	for reason := range documented {
		if _, ok := reasonCodes[reason]; !ok {
			t.Errorf("docs/cli-reference.md documents %q, which no failure prints", reason)
		}
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
	if !strings.Contains(out, "error (code=3, reason=unknown_command): unknown command \"unknown\"") {
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
	if !strings.Contains(out, "error (code=3, reason=workspace_not_initialized): repository not initialized with lit — run 'lit init' first") {
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

// schemaAheadError is the ticket's reproduction: a workspace at schema 999
// whose baseline is broken, opened by a binary that supports up to 5.
func schemaAheadError() *store.UnsupportedSchemaVersionError {
	return &store.UnsupportedSchemaVersionError{WorkspaceVersion: 999, MaxSupported: 5, MissingBaseline: []string{"issues.title"}}
}

// upgradeTargetBehindError is `lit upgrade` against that same workspace: the
// latest release stops at schema 5, and this binary cannot open the workspace.
func upgradeTargetBehindError() *UpgradeTargetBehindError {
	return &UpgradeTargetBehindError{Current: 999, Target: 5, Tag: "v0.14.0", WorkspaceOpenable: false}
}

// TestWriteCommandErrorSchemaVersionRefusals pins the surface an agent reads
// for the two refusals the ticket reproduced. Each repeats on every run, so
// its remediation must not send the agent to retry or to `lit doctor`, and
// must not contradict the remedy its message already names. The reason and
// exit code are pinned as data in the tables; this asserts the words.
func TestWriteCommandErrorSchemaVersionRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		reason string
		// want is the remediation's act; absent is advice that would be false
		// for this refusal.
		want, absent string
	}{
		// The command is not what is wrong, so "adjust the command" would send
		// the agent looking for a flag that does not exist.
		// Both paths the message names change more than this command, so the
		// agent is told to wait for the user rather than take either.
		{"workspace schema ahead", schemaAheadError(), "workspace_schema_ahead", "take such a path only when the user directs it", "adjust the command"},
		// The message names a newer target, which is the command adjusted.
		{"upgrade target behind", upgradeTargetBehindError(), "validation_refused", "adjust the command to satisfy it", "supported path"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if code := WriteCommandError(&stderr, tc.err); code != ExitValidation {
				t.Fatalf("exitCode = %d, want %d (ExitValidation)", code, ExitValidation)
			}
			out := stderr.String()
			if !strings.Contains(out, "error (code=3, reason="+tc.reason+"): "+tc.err.Error()) {
				t.Fatalf("missing the message body at exit 3: %q", out)
			}
			_, remediation, ok := strings.Cut(out, "\nremediation: ")
			if !ok {
				t.Fatalf("missing remediation line: %q", out)
			}
			if strings.Contains(remediation, "Retry the command") || strings.Contains(remediation, "lit doctor") {
				t.Fatalf("a refusal that repeats on every run must not carry the retry-then-doctor advice: %q", remediation)
			}
			if !strings.HasPrefix(remediation, "Do not retry unchanged") || !strings.Contains(remediation, tc.want) {
				t.Fatalf("remediation must say not to retry and name %q: %q", tc.want, remediation)
			}
			if strings.Contains(remediation, tc.absent) {
				t.Fatalf("remediation must not say %q: %q", tc.absent, remediation)
			}
		})
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
