#!/bin/sh
# Can an ordinary action inside this environment reach an upstream original?
# Run it INSIDE a candidate isolation mechanism. A mechanism nobody tried to defeat
# does not go in design-docs/clean-room-reimplementation.md.
#
#   scripts/cleanroom-reach-probe.sh [module-path] [version]
#
# Three verdicts, not two. An earlier version printed only REACHED/blocked, which made
# "blocked" an answer-shaped void: it meant BOTH "the sandbox refused me" and "I looked
# in the wrong place and found nothing". The second reading passed the gate with no
# isolation in place at all. So:
#
#   REACHED       got at the source. The mechanism does not hold.           -> exit 1
#   blocked       knew where to look and was refused. Proof.                -> exit 0
#   INCONCLUSIVE  could not establish the route was exercised. Not proof.   -> exit 2
#
# Only all-blocked passes. INCONCLUSIVE fails the gate on purpose: an untested route is
# not a closed one, and a gate that cannot tell the difference is worse than no gate.
#
# Always compare against a control run outside the mechanism — a route that fails
# everywhere proves nothing:
#
#   scripts/cleanroom-reach-probe.sh                                    # control
#   scripts/cleanroom-sandbox.sh --offline --deny github.com/hashicorp/golang-lru \
#       -- scripts/cleanroom-reach-probe.sh                             # candidate

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd -P)
# shellcheck source=scripts/cleanroom-modpath.sh
. "$SCRIPT_DIR/cleanroom-modpath.sh"

MOD_PATH="${1:-github.com/hashicorp/golang-lru/v2}"
MOD_VER="${2:-v2.0.7}"

MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
ESC_PATH=$(cleanroom_escape_module "$MOD_PATH")
CACHE_DIR="$MODCACHE/$ESC_PATH@$MOD_VER"

reached=0
unknown=0
verdict() {
	printf '%-30s %-13s %s\n' "$1" "$2" "$3"
	case "$2" in
	REACHED) reached=$((reached + 1)) ;;
	INCONCLUSIVE) unknown=$((unknown + 1)) ;;
	esac
	return 0
}

# Distinguish "refused" from "not there" by the errno, not by a boolean. EPERM/EACCES
# is the sandbox doing its job; ENOENT means this probe tested nothing.
classify() {
	err=$(ls -d "$1" 2>&1 >/dev/null)
	if [ -z "$err" ]; then
		echo reachable
	elif printf '%s' "$err" | grep -qiE 'not permitted|permission denied'; then
		echo denied
	else
		echo absent
	fi
}

echo "=== reach probe: $MOD_PATH@$MOD_VER ==="
echo "    GOMODCACHE=$MODCACHE"
echo "    expected at $ESC_PATH@$MOD_VER"
echo

# 1. The module cache by absolute path — the path an agent types from memory.
state=$(classify "$CACHE_DIR")
case "$state" in
reachable) verdict "1 module cache (abs path)" "REACHED" "$(find "$CACHE_DIR" -name '*.go' 2>/dev/null | wc -l | tr -d ' ') .go files" ;;
denied) verdict "1 module cache (abs path)" "blocked" "refused by policy" ;;
absent) verdict "1 module cache (abs path)" "INCONCLUSIVE" "not in this cache — nothing was tested" ;;
esac

# 2. Search by name — the agent who knows the name but not the exact path.
# Counts only trees whose contents actually open: seeing a directory NAME through a
# readable parent is not contamination, reading what is inside it is.
leaf=$(basename "$(cleanroom_module_base "$ESC_PATH")")
if [ "$(classify "$MODCACHE")" = "denied" ]; then
	verdict "2 search by name" "blocked" "cache root refused"
else
	cands=$(find "$MODCACHE" -maxdepth 6 -type d -name "$leaf*" 2>/dev/null)
	n=0
	for d in $cands; do
		[ -n "$(find "$d" -name '*.go' 2>/dev/null | head -1)" ] && n=$((n + 1))
	done
	if [ "$n" -gt 0 ]; then verdict "2 search by name" "REACHED" "$n readable tree(s)"
	elif [ -n "$cands" ]; then verdict "2 search by name" "blocked" "names visible, contents refused"
	else verdict "2 search by name" "INCONCLUSIVE" "no candidate trees exist here"; fi
fi

# 3. Actually open a source file.
case "$state" in
absent) verdict "3 open a source file" "INCONCLUSIVE" "no tree to open" ;;
denied) verdict "3 open a source file" "blocked" "refused by policy" ;;
reachable)
	f=$(find "$CACHE_DIR" -name '*.go' 2>/dev/null | head -1)
	if [ -n "$f" ] && b=$(wc -c <"$f" 2>/dev/null) && [ "${b:-0}" -gt 0 ]; then
		verdict "3 open a source file" "REACHED" "$(echo "$b" | tr -d ' ') bytes"
	else
		verdict "3 open a source file" "blocked" "open refused"
	fi
	;;
esac

# 4. go doc — prints the original author's doc comments.
if ! command -v go >/dev/null 2>&1; then
	verdict "4 go doc" "INCONCLUSIVE" "no go toolchain to test with"
else
	b=$(go doc "$MOD_PATH" 2>/dev/null | wc -c | tr -d ' ')
	if [ "${b:-0}" -gt 40 ]; then verdict "4 go doc" "REACHED" "$b bytes of doc"
	elif [ "$state" = "absent" ]; then verdict "4 go doc" "INCONCLUSIVE" "module absent; nothing to resolve"
	else verdict "4 go doc" "blocked" "no output"; fi
fi

# 5. Refetch into a relocated cache — defeats a merely-emptied one.
if ! command -v go >/dev/null 2>&1; then
	verdict "5 go mod download" "INCONCLUSIVE" "no go toolchain to test with"
else
	s=$(mktemp -d)
	(cd "$s" && go mod init probe.local/reach >/dev/null 2>&1 &&
		GOMODCACHE="$s/mc" GOFLAGS='' go mod download "$MOD_PATH@$MOD_VER" >/dev/null 2>&1)
	n=$(find "$s/mc" -name '*.go' 2>/dev/null | wc -l | tr -d ' ')
	if [ "${n:-0}" -gt 0 ]; then
		verdict "5 go mod download" "REACHED" "$n .go files fetched"
	else
		verdict "5 go mod download" "blocked" "fetch refused"
	fi
	# go marks module-cache files read-only; make them writable before removing.
	if [ -n "$s" ] && [ -d "$s" ]; then
		chmod -R u+w "$s" 2>/dev/null
		rm -rf "$s"
	fi
fi

# 6. Raw network fetch, bypassing the go command entirely. The URL takes the same
# case-encoding as the cache path; proxy.golang.org rejects the raw path outright.
if ! command -v curl >/dev/null 2>&1; then
	verdict "6 curl module zip" "INCONCLUSIVE" "no curl to test with"
else
	b=$(curl -sS -m 25 "https://proxy.golang.org/$ESC_PATH/@v/$MOD_VER.zip" 2>/dev/null | wc -c | tr -d ' ')
	if [ "${b:-0}" -gt 1000 ]; then
		verdict "6 curl module zip" "REACHED" "$b bytes"
	elif curl -sS -m 15 -o /dev/null "https://proxy.golang.org/" 2>/dev/null; then
		# Egress works but this fetch did not: the module URL is wrong, or the
		# proxy is unhappy. Either way nothing about isolation was established.
		verdict "6 curl module zip" "INCONCLUSIVE" "egress works but fetch failed ($b bytes)"
	else
		verdict "6 curl module zip" "blocked" "no egress (confirm via the control run)"
	fi
fi

echo
if [ "$reached" -gt 0 ]; then
	echo "=== $reached route(s) REACHED — this mechanism does not hold ==="
	exit 1
fi
if [ "$unknown" -gt 0 ]; then
	echo "=== $unknown route(s) INCONCLUSIVE — the gate is NOT established ==="
	exit 2
fi
echo "=== all routes blocked ==="
exit 0
