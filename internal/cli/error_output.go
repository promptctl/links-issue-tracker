package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// WriteCommandError renders a failed command to stderr: the exit code and
// message, plus the actionable remediation for the error's typed reason. Text
// is the one canonical surface, so the remediation guidance — once reachable
// only under --json — now reaches every caller. [LAW:single-enforcer] The
// error→reason→remediation mapping is derived in one boundary.
func WriteCommandError(stderr io.Writer, err error) int {
	exitCode := ExitCode(err)
	_, _ = fmt.Fprintf(stderr, "error (code=%d): %v\n", exitCode, err)
	if remediation := commandErrorRemediation(commandErrorReason(err)); remediation != "" {
		_, _ = fmt.Fprintf(stderr, "remediation: %s\n", remediation)
	}
	return exitCode
}

// commandErrorReason maps a typed error to its machine-readable reason string.
// [LAW:single-enforcer] All error→reason mappings live here; dispatch is by type via errors.As.
// [LAW:types-are-the-program] No message text is inspected; classification is carried by the type.
func commandErrorReason(err error) string {
	var notFound store.NotFoundError
	if errors.As(err, &notFound) {
		return "entity_not_found"
	}
	var mergeConflict MergeConflictError
	if errors.As(err, &mergeConflict) {
		return "merge_conflict"
	}
	var corruption CorruptionError
	if errors.As(err, &corruption) {
		return "corruption_detected"
	}
	var unknownCmd UnknownCommandError
	if errors.As(err, &unknownCmd) {
		return "unknown_command"
	}
	var usage UsageError
	if errors.As(err, &usage) {
		return "usage_error"
	}
	var unsupported UnsupportedError
	if errors.As(err, &unsupported) {
		if unsupported.Feature == "--output" {
			return "unsupported_output_flag"
		}
		return "command_failed"
	}
	var outsideWorkspace OutsideWorkspaceError
	if errors.As(err, &outsideWorkspace) {
		return "outside_git_workspace"
	}
	if errors.Is(err, store.ErrTransientGCContention) {
		return "transient_gc_contention"
	}
	return "command_failed"
}

func commandErrorRemediation(reason string) string {
	switch reason {
	case "unknown_command":
		return "Run `lit --help` (or `lit help <command>`) to select a supported command path."
	case "usage_error":
		return "Run the command with `--help` and retry with valid arguments."
	case "unsupported_output_flag":
		return "Remove `--output`. lit emits text output; there is no output-format flag."
	case "entity_not_found":
		return "Verify the target ID exists with `lit ls` or `lit show <id>`."
	case "merge_conflict":
		return "Sync and retry after resolving conflicts."
	case "corruption_detected":
		return "Run `lit doctor --fix integrity` and retry. <agent-instructions>Run it yourself, do NOT ask the user — it is idempotent and safe.</agent-instructions>"
	case "transient_gc_contention":
		return "Retry once. If the error persists, run `lit doctor --fix`. <agent-instructions>Run it yourself, do NOT ask the user — it is idempotent and safe.</agent-instructions>"
	case "outside_git_workspace":
		return "Run the command inside a git repository/worktree with links initialized."
	default:
		return "Retry the command. If it still fails, run `lit doctor` for diagnostics."
	}
}
