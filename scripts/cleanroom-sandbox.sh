#!/bin/sh
# Run a command with named upstream modules unreadable at the kernel level.
# The isolation mechanism for stage 4 of the clean-room protocol; see
# design-docs/clean-room-reimplementation.md.
#
#   scripts/cleanroom-sandbox.sh [--offline] --deny <module-path> [--deny ...] -- <cmd> [args]
#
# --deny takes a module path WITHOUT a major-version suffix; one rule walls off every
# version and every /vN variant of that library (golang-lru v1.0.2 and golang-lru/v2
# v2.0.7 are two modules, one --deny). Both the unpacked tree and the downloaded zip
# are covered — extracting the zip reaches the same source.
#
# --offline additionally denies the network, which closes the last route to an
# original: refetching it from proxy.golang.org. An agent process cannot use
# --offline, because it needs network to reach its own API.
#
# Sandbox the AGENT PROCESS, not the shell commands it spawns. An agent's file-reading
# tools run inside the harness process and never enter a profile applied to its
# children. Seatbelt denies at the syscall level, so wrapping the whole process covers
# in-process reads too.
#
# The deny is per-module, deliberately, rather than the whole module cache: denying
# GOMODCACHE wholesale makes the go command unusable ("could not create module cache"),
# and a stage-4 agent that cannot run `go test` cannot verify its own work.

set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck source=scripts/cleanroom-modpath.sh
. "$SCRIPT_DIR/cleanroom-modpath.sh"

OFFLINE=0
DENIED=""

while [ "$#" -gt 0 ]; do
	case "$1" in
	--offline)
		OFFLINE=1
		shift
		;;
	--deny)
		[ "$#" -ge 2 ] || {
			echo "ERROR: --deny needs a module path" >&2
			exit 2
		}
		DENIED="$DENIED $(cleanroom_module_base "$2")"
		shift 2
		;;
	--)
		shift
		break
		;;
	-*)
		echo "ERROR: unknown option $1" >&2
		exit 2
		;;
	*) break ;;
	esac
done

if [ "$#" -eq 0 ] || [ -z "$DENIED" ]; then
	echo "usage: $0 [--offline] --deny <module-path> [--deny ...] -- <command> [args...]" >&2
	echo "       at least one --deny is required: a sandbox that walls off nothing is not one." >&2
	exit 2
fi

command -v sandbox-exec >/dev/null 2>&1 || {
	echo "ERROR: sandbox-exec not found — this wrapper is macOS-only." >&2
	echo "       On Linux, use a container without the module cache and rerun" >&2
	echo "       scripts/cleanroom-reach-probe.sh inside it before trusting it." >&2
	exit 1
}

MODCACHE=$(go env GOMODCACHE) || {
	echo "ERROR: 'go env GOMODCACHE' failed; cannot determine what to deny." >&2
	exit 1
}
[ -n "$MODCACHE" ] || {
	echo "ERROR: GOMODCACHE is empty; refusing to run an unenforcing profile." >&2
	exit 1
}
[ -d "$MODCACHE" ] || {
	echo "ERROR: GOMODCACHE does not exist: $MODCACHE" >&2
	exit 1
}

# Seatbelt matches RESOLVED physical paths. A rule naming a symlinked path does not
# error and does not warn — it silently matches nothing, leaving a profile that looks
# protective and enforces nothing. Resolve before writing the rule, never after.
MODCACHE=$(cd "$MODCACHE" && pwd -P)

PROFILE=$(mktemp -t cleanroom.sb) || {
	echo "ERROR: mktemp failed" >&2
	exit 1
}
trap 'rm -f "$PROFILE"' EXIT INT TERM

{
	echo '(version 1)'
	echo '(allow default)'
	for mod in $DENIED; do
		esc=$(cleanroom_escape_module "$mod")
		# <base>@<ver> and <base>/vN@<ver> are siblings, not nested: one anchored
		# regex on the shared prefix covers every version and every major variant.
		echo "(deny file-read* (regex #\"^$(cleanroom_regex_quote "$MODCACHE/$esc")(@|/)\"))"
		# The zip is the same source in another form.
		echo "(deny file-read* (subpath \"$MODCACHE/cache/download/$esc\"))"
	done
	[ "$OFFLINE" -eq 1 ] && echo '(deny network*)'
} >"$PROFILE"

# Deliberately not `exec`: exec replaces this process, so the EXIT trap above would
# never fire and every invocation would leak a profile into TMPDIR. Running it as a
# child lets the trap clean up. Seatbelt reads the profile at startup and never
# consults it again, so removing it while the child runs is safe.
set +e
sandbox-exec -f "$PROFILE" "$@"
status=$?
set -e
exit "$status"
