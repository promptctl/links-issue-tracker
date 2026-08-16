#!/bin/sh
# Run a command with the Go module cache — where upstream originals sit unpacked —
# denied at the kernel level. The isolation mechanism for stage 4 of the clean-room
# protocol; see design-docs/clean-room-reimplementation.md.
#
#   scripts/cleanroom-sandbox.sh [--offline] <command> [args...]
#
# --offline also denies the network, which closes the last route to an original
# (refetching it from proxy.golang.org). Use it for any command that does not need
# network of its own. An agent process cannot use --offline: it needs its own API.
#
# Sandbox the AGENT PROCESS, not the shell commands it spawns. An agent's file-reading
# tools run inside the harness process and never enter a profile applied to its
# children. Seatbelt denies at the syscall level, so wrapping the whole process covers
# in-process reads too.

set -eu

OFFLINE=0
if [ "${1:-}" = "--offline" ]; then OFFLINE=1; shift; fi

if [ "$#" -eq 0 ]; then
	echo "usage: $0 [--offline] <command> [args...]" >&2
	exit 2
fi

command -v sandbox-exec >/dev/null 2>&1 || {
	echo "ERROR: sandbox-exec not found — this wrapper is macOS-only." >&2
	echo "       On Linux, use a container with the module cache masked and rerun" >&2
	echo "       scripts/cleanroom-reach-probe.sh inside it before trusting it." >&2
	exit 1
}

MODCACHE=$(go env GOMODCACHE) || {
	echo "ERROR: 'go env GOMODCACHE' failed; cannot determine what to deny." >&2
	exit 1
}
[ -n "$MODCACHE" ] || { echo "ERROR: GOMODCACHE is empty; refusing to run an unenforcing profile." >&2; exit 1; }
[ -d "$MODCACHE" ] || { echo "ERROR: GOMODCACHE does not exist: $MODCACHE" >&2; exit 1; }

# Seatbelt matches RESOLVED physical paths. A rule naming a symlinked path does not
# error and does not warn — it silently matches nothing, leaving a profile that looks
# protective and enforces nothing. Resolve before writing the rule, never after.
MODCACHE=$(cd "$MODCACHE" && pwd -P)
SUMDB=$(dirname "$MODCACHE")/sumdb

PROFILE=$(mktemp -t cleanroom.sb) || { echo "ERROR: mktemp failed" >&2; exit 1; }
trap 'rm -f "$PROFILE"' EXIT INT TERM

{
	echo '(version 1)'
	echo '(allow default)'
	echo "(deny file-read* (subpath \"$MODCACHE\"))"
	echo "(deny file-read* (subpath \"$SUMDB\"))"
	[ "$OFFLINE" -eq 1 ] && echo '(deny network*)'
} >"$PROFILE"

exec sandbox-exec -f "$PROFILE" "$@"
