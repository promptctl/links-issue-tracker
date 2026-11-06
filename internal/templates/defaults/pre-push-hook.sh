# --- BEGIN LIT INTEGRATION ---
set -u

remote_name="${1:-origin}"

# An empty trace-ref file reads as unset, so one invocation serves whether or
# not mktemp could make the file.
trace_ref_file="$(mktemp "${TMPDIR:-/tmp}/links-pre-push-trace.XXXXXX" 2>/dev/null || true)"

# stderr passes through: a failed push writes its own block there, and that
# block is the only thing that knows the failure's remedy.
# The `||` keeps a failure from tripping a `set -e` in the user's hook above.
lit_push_status=0
LNKS_AUTOMATION_TRIGGER="git-pre-push" \
  LNKS_AUTOMATION_REASON="git push triggered the managed pre-push sync" \
  LNKS_AUTOMATION_TRACE_REF_FILE="${trace_ref_file}" \
  lit sync push --remote "${remote_name}" >/dev/null || lit_push_status=$?
if [[ "${lit_push_status}" -ne 0 ]]; then
  trace_ref="$(cat "${trace_ref_file}" 2>/dev/null || true)"
  printf '[links] warning: hook-triggered lit sync push exited %s (trigger=git-pre-push remote=%s trace=%s); lit'\''s output above, if any, says what failed. The git push itself was not blocked.\n' "${lit_push_status}" "${remote_name}" "${trace_ref:-unavailable}" >&2
fi
rm -f "${trace_ref_file}"

exit 0
# --- END LIT INTEGRATION ---
