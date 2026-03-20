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
	Managed    bool
	Reason     string
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
	fs := newCobraFlagSet("hooks install")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
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
		"managed":      result.Managed,
		"reason":       result.Reason,
		"traces_dir":   automationTraceDir(ws),
	}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
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
		return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: true, Managed: true}, nil
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
			return hookInstallResult{
				HookPath:   hookPath,
				LegacyPath: detectLegacyHookPath(legacyPath),
				Changed:    false,
				Managed:    false,
				Reason:     "incompatible",
			}, nil
		}
		var changed bool
		updated, changed = upsertManagedSection(existingStr, section, linksHookBeginMarker, linksHookEndMarker)
		if !changed {
			return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: false, Managed: true}, nil
		}
	}

	if err := os.WriteFile(hookPath, []byte(updated), mode); err != nil {
		return hookInstallResult{}, fmt.Errorf("write pre-push hook: %w", err)
	}
	return hookInstallResult{HookPath: hookPath, LegacyPath: detectLegacyHookPath(legacyPath), Changed: true, Managed: true}, nil
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

hook_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
legacy_hook="${hook_dir}/pre-push.links.user"

if [[ -x "${legacy_hook}" ]]; then
  "${legacy_hook}" "$@" || true
fi

trace_ref_file="$(mktemp "${TMPDIR:-/tmp}/links-pre-push-trace.XXXXXX" 2>/dev/null || true)"
if [[ -n "${trace_ref_file}" ]]; then
  if ! LNKS_AUTOMATION_TRIGGER="git-pre-push" \
    LNKS_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
    LNKS_AUTOMATION_TRACE_REF_FILE="${trace_ref_file}" \
    lit sync push --remote "${remote_name}" >/dev/null 2>&1; then
    trace_ref="$(cat "${trace_ref_file}" 2>/dev/null || true)"
    printf '\033[33m[links] warning: hook-triggered lit sync push failed (trigger=git-pre-push remote=%s trace=%s); agent should retry lit sync push --remote %s\033[0m\n' "${remote_name}" "${trace_ref:-unavailable}" "${remote_name}" >&2
  fi
  rm -f "${trace_ref_file}"
else
  if ! LNKS_AUTOMATION_TRIGGER="git-pre-push" \
    LNKS_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
    lit sync push --remote "${remote_name}" >/dev/null 2>&1; then
    printf '\033[33m[links] warning: hook-triggered lit sync push failed (trigger=git-pre-push remote=%s trace=%s); agent should retry lit sync push --remote %s\033[0m\n' "${remote_name}" "unavailable" "${remote_name}" >&2
  fi
fi

exit 0
# --- END LINKS INTEGRATION ---
`) + "\n"
}
