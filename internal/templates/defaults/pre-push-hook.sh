# --- BEGIN LIT INTEGRATION ---
set -u

remote_name="${1:-origin}"

# An empty trace-ref file reads as unset, so one invocation serves whether or
# not mktemp could make the file.
trace_ref_file="$(mktemp "${TMPDIR:-/tmp}/links-pre-push-trace.XXXXXX" 2>/dev/null || true)"

# stderr passes through: a failed push writes its own block there, and that
# block is the only thing that knows the failure's remedy.
if ! LNKS_AUTOMATION_TRIGGER="git-pre-push" \
  LNKS_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
  LNKS_AUTOMATION_TRACE_REF_FILE="${trace_ref_file}" \
  lit sync push --remote "${remote_name}" >/dev/null; then
  trace_ref=""
  if [[ -n "${trace_ref_file}" ]]; then
    trace_ref="$(cat "${trace_ref_file}" 2>/dev/null || true)"
  fi
  printf '[links] warning: hook-triggered lit sync push failed (trigger=git-pre-push remote=%s trace=%s); its own output above says what failed and how to resolve it. The git push itself was not blocked.\n' "${remote_name}" "${trace_ref:-unavailable}" >&2
fi
if [[ -n "${trace_ref_file}" ]]; then
  rm -f "${trace_ref_file}"
fi

exit 0
# --- END LIT INTEGRATION ---
