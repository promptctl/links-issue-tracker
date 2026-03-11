package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/workspace"
)

const (
	linksPrePushHookMarker = "# links-hook: pre-push v1" // legacy marker
	// [LAW:one-source-of-truth] Only the section between these markers is owned by links.
	linksHookBeginMarker = "# --- BEGIN LINKS INTEGRATION ---"
	linksHookEndMarker   = "# --- END LINKS INTEGRATION ---"
)

type hookInstallResult struct {
	HookPath   string
	LegacyPath string
	Changed    bool
}

func runHooks(stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit hooks install [--json]")
	}
	switch args[0] {
	case "install":
		return runHooksInstall(stdout, ws, args[1:])
	default:
		return errors.New("usage: lit hooks install [--json]")
	}
}

func runHooksInstall(stdout io.Writer, ws workspace.Info, args []string) error {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		return errors.New("usage: lit hooks install [--json]")
	}

	result, err := installHooks(ws)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"status":       "installed",
		"hook":         result.HookPath,
		"legacy_chain": result.LegacyPath,
		"changed":      result.Changed,
		"traces_dir":   automationTraceDir(ws),
	}
	return printValue(stdout, payload, jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]any)
		_, printErr := fmt.Fprintf(w, "installed %s\n", p["hook"])
		return printErr
	})
}

func installHooks(ws workspace.Info) (hookInstallResult, error) {
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	hookPath := filepath.Join(hooksDir, "pre-push")
	legacyPath := filepath.Join(hooksDir, "pre-push.links.user")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return hookInstallResult{}, fmt.Errorf("create hooks dir: %w", err)
	}

	section := renderLinksPrePushHookSection()
	existing, err := os.ReadFile(hookPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return hookInstallResult{}, fmt.Errorf("read existing pre-push hook: %w", err)
	}

	updated := renderLinksPrePushHookFile(section)
	mode := os.FileMode(0o755)
	if errors.Is(err, os.ErrNotExist) {
		if writeErr := os.WriteFile(hookPath, []byte(updated), mode); writeErr != nil {
			return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", writeErr)
		}
		return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: true}, nil
	}

	if info, statErr := os.Stat(hookPath); statErr == nil {
		mode = info.Mode().Perm()
		if mode&0o111 == 0 {
			mode = 0o755
		}
	}

	existingStr := string(existing)

	// Treat a hook as bash-compatible only if its shebang explicitly references bash.
	isBashCompatible := func(script string) bool {
		firstLineEnd := strings.IndexByte(script, '\n')
		var firstLine string
		if firstLineEnd == -1 {
			firstLine = strings.TrimSpace(script)
		} else {
			firstLine = strings.TrimSpace(script[:firstLineEnd])
		}
		if !strings.HasPrefix(firstLine, "#!") {
			// No explicit interpreter; assume not bash-compatible to avoid breaking the hook.
			return false
		}
		// Only treat hooks that explicitly mention bash as compatible with the managed bash section.
		return strings.Contains(firstLine, "bash")
	}

	// [LAW:single-enforcer] hook install owns all managed-hook rewrites, including legacy conversion.
	if strings.Contains(existingStr, linksPrePushHookMarker) && !strings.Contains(existingStr, linksHookBeginMarker) {
		updated = renderLinksPrePushHookFile(section)
	} else {
		if !isBashCompatible(existingStr) {
			// Do not insert a bash-specific managed section into a non-bash hook; that could break git pushes.
			return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: false}, nil
		}
		var changed bool
		updated, changed = upsertManagedSection(existingStr, section, linksHookBeginMarker, linksHookEndMarker)
		if !changed {
			return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: false}, nil
		}
	}

	if err := os.WriteFile(hookPath, []byte(updated), mode); err != nil {
		return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", err)
	}
	return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: true}, nil
}

func detectLegacyHookPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func renderLinksPrePushHookFile(section string) string {
	return "#!/usr/bin/env bash\n" + section
}

func renderLinksPrePushHookSection() string {
	return strings.TrimSpace(`
# --- BEGIN LINKS INTEGRATION ---
set -u

remote_name="${1:-origin}"
retry_command="lit sync push --remote ${remote_name} --json"

hook_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
legacy_hook="${hook_dir}/pre-push.links.user"

if [[ -x "${legacy_hook}" ]]; then
  "${legacy_hook}" "$@" || true
fi

trace_reason_from_file() {
  local trace_path="${1:-}"
  if [[ -z "${trace_path}" || ! -f "${trace_path}" ]]; then
    return 1
  fi
  local reason_line
  reason_line="$(grep -m1 '"reason"' "${trace_path}" 2>/dev/null || true)"
  if [[ -z "${reason_line}" ]]; then
    return 1
  fi
  printf '%s\n' "${reason_line}" | sed -E 's/^[[:space:]]*"reason":[[:space:]]*"([^"]*)".*/\1/'
}

extract_json_string_field() {
  local field_name="${1:-}"
  local json_path="${2:-}"
  if [[ -z "${field_name}" || -z "${json_path}" || ! -f "${json_path}" ]]; then
    return 1
  fi
  local field_line
  field_line="$(grep -m1 "\"${field_name}\"" "${json_path}" 2>/dev/null || true)"
  if [[ -z "${field_line}" ]]; then
    return 1
  fi
  printf '%s\n' "${field_line}" | sed -E "s/.*\"${field_name}\"[[:space:]]*:[[:space:]]*\"([^\"]*)\".*/\1/"
}

emit_sync_failure_notice() {
  local reason_code="${1:-command_failed}"
  local trace_ref="${2:-unavailable}"
  local level="warning"
  local color='\033[33m'
  local event_name="hook_sync_push_failed"
  if [[ "${reason_code}" == "manifest_read_only" ]]; then
    level="info"
    color='\033[36m'
    event_name="hook_sync_push_nonblocking"
  fi
  printf "${color}[links] %s: %s trigger=git-pre-push remote=%s reason=%s trace=%s retry_command=%q\033[0m\n" "${level}" "${event_name}" "${remote_name}" "${reason_code}" "${trace_ref}" "${retry_command}" >&2
}

trace_ref_file="$(mktemp "${TMPDIR:-/tmp}/links-pre-push-trace.XXXXXX" 2>/dev/null || true)"
sync_output_file="$(mktemp "${TMPDIR:-/tmp}/links-pre-push-output.XXXXXX" 2>/dev/null || true)"
sync_failed=0
trace_ref="unavailable"
if [[ -n "${trace_ref_file}" ]]; then
  if ! LIT_AUTOMATION_TRIGGER="git-pre-push" \
    LIT_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
    LIT_AUTOMATION_TRACE_REF_FILE="${trace_ref_file}" \
    lit sync push --remote "${remote_name}" --json >"${sync_output_file:-/dev/null}" 2>&1; then
    sync_failed=1
    trace_ref="$(cat "${trace_ref_file}" 2>/dev/null || true)"
    trace_ref="${trace_ref:-unavailable}"
  fi
  rm -f "${trace_ref_file}"
else
  if ! LIT_AUTOMATION_TRIGGER="git-pre-push" \
    LIT_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
    lit sync push --remote "${remote_name}" --json >"${sync_output_file:-/dev/null}" 2>&1; then
    sync_failed=1
  fi
fi

if [[ "${sync_failed}" == "1" ]]; then
  reason_code="$(extract_json_string_field "reason" "${sync_output_file}" || true)"
  reason_code="${reason_code:-command_failed}"
  output_trace_ref="$(extract_json_string_field "trace_ref" "${sync_output_file}" || true)"
  if [[ "${trace_ref}" == "unavailable" && -n "${output_trace_ref}" ]]; then
    trace_ref="${output_trace_ref}"
  fi
  trace_reason="$(trace_reason_from_file "${trace_ref}" || true)"
  if [[ "${trace_reason}" == *"cannot update manifest"* && "${trace_reason}" == *"read only"* ]]; then
    reason_code="manifest_read_only"
  fi
  emit_sync_failure_notice "${reason_code}" "${trace_ref}"
fi

if [[ -n "${sync_output_file}" ]]; then
  rm -f "${sync_output_file}"
fi

exit 0
# --- END LINKS INTEGRATION ---
`) + "\n"
}
