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
