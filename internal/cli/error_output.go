package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/store"
)

type commandErrorPayload struct {
	Code               string `json:"code"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	Remediation        string `json:"remediation,omitempty"`
	RemediationCommand string `json:"remediation_command,omitempty"`
	TraceRef           string `json:"trace_ref,omitempty"`
	TraceError         string `json:"trace_error,omitempty"`
	BlockedCommand     string `json:"blocked_command,omitempty"`
	Summary            string `json:"summary,omitempty"`
	Trigger            string `json:"trigger,omitempty"`
	ExitCode           int    `json:"exit_code"`
}

func WriteCommandError(stderr io.Writer, stdout io.Writer, args []string, err error) int {
	payload := buildCommandErrorPayload(err)
	if shouldEmitJSONError(args, stdout) {
		_ = json.NewEncoder(stderr).Encode(map[string]any{
			"error": payload,
		})
		return payload.ExitCode
	}
	_, _ = fmt.Fprintf(stderr, "error (code=%d): %v\n", payload.ExitCode, err)
	return payload.ExitCode
}

func buildCommandErrorPayload(err error) commandErrorPayload {
	exitCode := ExitCode(err)
	code := commandErrorCode(exitCode)
	reason := commandErrorReason(err)
	remediation := commandErrorRemediation(reason)
	traceRef := commandErrorTraceRef(code, reason, err.Error())
	details := commandErrorDetails(err)
	traceRef = firstNonEmptyString(details["trace_ref"], traceRef)
	// [LAW:single-enforcer] Machine-readable failure schema (code/reason/remediation/trace) is derived in one boundary.
	return commandErrorPayload{
		Code:               code,
		Reason:             reason,
		Message:            err.Error(),
		Remediation:        remediation,
		RemediationCommand: firstNonEmptyString(details["remediation_command"]),
		TraceRef:           traceRef,
		TraceError:         firstNonEmptyString(details["trace_error"]),
		BlockedCommand:     firstNonEmptyString(details["blocked_command"]),
		Summary:            firstNonEmptyString(details["summary"]),
		Trigger:            firstNonEmptyString(details["trigger"]),
		ExitCode:           exitCode,
	}
}

func shouldEmitJSONError(args []string, stdout io.Writer) bool {
	// [LAW:one-source-of-truth] Error-output format follows the same global output precedence resolver as normal command output.
	_, mode, err := parseGlobalOutputMode(args, stdout)
	if err == nil {
		return mode == outputModeJSON || explicitJSONErrorRequest(args)
	}
	return explicitJSONErrorRequest(args)
}

func explicitJSONErrorRequest(args []string) bool {
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "--":
			return false
		case args[index] == "--json":
			return true
		}
	}
	return false
}

func commandErrorCode(exitCode int) string {
	switch exitCode {
	case ExitUsage:
		return "usage"
	case ExitValidation:
		return "validation"
	case ExitNotFound:
		return "not_found"
	case ExitConflict:
		return "conflict"
	case ExitCorruption:
		return "corruption"
	default:
		return "generic"
	}
}

func commandErrorReason(err error) string {
	var beadsRequired BeadsMigrationRequiredError
	if errors.As(err, &beadsRequired) {
		return "beads_migration_required"
	}
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
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(message, "unknown command"):
		return "unknown_command"
	case strings.HasPrefix(message, "usage:"):
		return "usage_error"
	case strings.Contains(message, "--output is no longer supported"),
		strings.Contains(message, "flag provided but not defined: -output"):
		return "unsupported_output_flag"
	case strings.Contains(message, "cannot update manifest") && strings.Contains(message, "read only"):
		return "manifest_read_only"
	case strings.Contains(message, "requires running inside a git repository"):
		return "outside_git_workspace"
	default:
		return "command_failed"
	}
}

func commandErrorRemediation(reason string) string {
	switch reason {
	case "beads_migration_required":
		return "Run `lnks migrate beads --apply --json` before retrying the command."
	case "unknown_command":
		return "Run `lnks --help` (or `lnks help <command>`) to select a supported command path."
	case "usage_error":
		return "Run the command with `--help` and retry with valid arguments."
	case "unsupported_output_flag":
		return "Remove `--output`. Use `--json` for JSON output or omit it for text output."
	case "entity_not_found":
		return "Verify the target ID exists with `lnks ls --json` or `lnks show <id> --json`."
	case "merge_conflict":
		return "Sync and retry after resolving conflicts."
	case "corruption_detected":
		return "Run `lnks fsck --repair --json` and retry."
	case "manifest_read_only":
		return "Retry once. If the error persists, run `lnks doctor --json` and `lnks fsck --repair --json`."
	case "outside_git_workspace":
		return "Run the command inside a git repository/worktree with links initialized."
	default:
		return "Retry the command. If it still fails, run `lnks doctor --json` for diagnostics."
	}
}

func commandErrorTraceRef(code string, reason string, message string) string {
	sum := sha256.Sum256([]byte(code + "|" + reason + "|" + message))
	return "err-" + hex.EncodeToString(sum[:8])
}

func commandErrorDetails(err error) map[string]any {
	var detailer errorDetailer
	if !errors.As(err, &detailer) {
		return nil
	}
	// [LAW:single-enforcer] Optional typed error details are merged into the canonical error envelope only here.
	return detailer.ErrorDetails()
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(fmt.Sprintf("%v", value))
		if trimmed == "" || trimmed == "<nil>" {
			continue
		}
		return trimmed
	}
	return ""
}
