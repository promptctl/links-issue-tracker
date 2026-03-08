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

const linksPrePushHookMarker = "# links-hook: pre-push v1"

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

	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	hookPath := filepath.Join(hooksDir, "pre-push")
	legacyPath := filepath.Join(hooksDir, "pre-push.links.user")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	existing, err := os.ReadFile(hookPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing pre-push hook: %w", err)
	}
	if len(existing) > 0 && !strings.Contains(string(existing), linksPrePushHookMarker) {
		if _, backupErr := os.Stat(legacyPath); errors.Is(backupErr, os.ErrNotExist) {
			if renameErr := os.Rename(hookPath, legacyPath); renameErr != nil {
				return fmt.Errorf("preserve existing pre-push hook: %w", renameErr)
			}
		}
	}

	if err := os.WriteFile(hookPath, []byte(renderLinksPrePushHook()), 0o755); err != nil {
		return fmt.Errorf("write pre-push hook: %w", err)
	}

	payload := map[string]any{
		"status":       "installed",
		"hook":         hookPath,
		"legacy_chain": legacyPath,
	}
	return printValue(stdout, payload, jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]any)
		_, printErr := fmt.Fprintf(w, "installed %s\n", p["hook"])
		return printErr
	})
}

func renderLinksPrePushHook() string {
	return `#!/usr/bin/env bash
` + linksPrePushHookMarker + `
set -u

remote_name="${1:-origin}"

hook_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
legacy_hook="${hook_dir}/pre-push.links.user"

if [[ -x "${legacy_hook}" ]]; then
  "${legacy_hook}" "$@" || true
fi

branches=""
while read -r local_ref local_sha remote_ref remote_sha; do
  branch="${local_ref#refs/heads/}"
  if [[ -z "${branch}" || "${branch}" == "${local_ref}" ]]; then
    continue
  fi
  if ! printf '%s\n' "${branches}" | grep -Fqx "${branch}"; then
    if [[ -z "${branches}" ]]; then
      branches="${branch}"
    else
      branches="${branches}
${branch}"
    fi
  fi
done

if [[ -z "${branches}" ]]; then
  current_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  if [[ -n "${current_branch}" && "${current_branch}" != "HEAD" ]]; then
    branches="${current_branch}"
  fi
fi

sync_failed=0
while read -r branch; do
  if [[ -z "${branch}" ]]; then
    continue
  fi
  if ! lit sync push --remote "${remote_name}" --branch "${branch}" >/dev/null 2>&1; then
    sync_failed=1
  fi
done <<EOF
${branches}
EOF

if [[ "${sync_failed}" -ne 0 ]]; then
  printf '\033[33m[links] warning: db sync failed; agent should auto-retry lit sync push --remote %s\033[0m\n' "${remote_name}" >&2
fi

exit 0
`
}
