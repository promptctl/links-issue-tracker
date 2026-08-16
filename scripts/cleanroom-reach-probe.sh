#!/bin/sh
# Can an ordinary action inside this environment reach an upstream original?
# Run it INSIDE a candidate isolation mechanism. Any REACHED means the mechanism
# does not hold, and a mechanism nobody tried to defeat does not go in
# design-docs/clean-room-reimplementation.md.
#
#   scripts/cleanroom-reach-probe.sh [module-path] [version]
#
# Compare against a control run outside the mechanism: a route that fails everywhere
# proves nothing about the sandbox.
#
#   scripts/cleanroom-reach-probe.sh                        # control
#   scripts/cleanroom-sandbox.sh --offline scripts/cleanroom-reach-probe.sh

MOD_PATH="${1:-github.com/hashicorp/golang-lru/v2}"
MOD_VER="${2:-v2.0.7}"

# Strip a trailing major-version element: the module cache stores v2+ modules as
# <base>/v2@<version> under the base directory, not under a literal "v2" path element.
MOD_BASE=$(echo "$MOD_PATH" | sed -E 's|/v[0-9]+$||')
MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
CACHE_DIR="$MODCACHE/$MOD_BASE"

reached=0
verdict() {
	printf '%-32s %-8s %s\n' "$1" "$2" "$3"
	[ "$2" = "REACHED" ] && reached=$((reached + 1))
	return 0
}

echo "=== reach probe: $MOD_PATH@$MOD_VER ==="
echo "    GOMODCACHE=$MODCACHE"
echo

# 1. The module cache by absolute path — the path an agent types from memory.
n=$(find "$CACHE_DIR" -name '*.go' 2>/dev/null | wc -l | tr -d ' ')
[ "${n:-0}" -gt 0 ] && verdict "1 module cache (abs path)" "REACHED" "$n .go files" ||
	verdict "1 module cache (abs path)" "blocked" "nothing visible"

# 2. Search by name — the agent who knows the name but not the exact path.
leaf=$(basename "$MOD_BASE")
n=$(find "$MODCACHE" -maxdepth 6 -type d -name "$leaf*" 2>/dev/null | wc -l | tr -d ' ')
[ "${n:-0}" -gt 0 ] && verdict "2 search by name" "REACHED" "$n dirs" ||
	verdict "2 search by name" "blocked" "no dirs"

# 3. Actually open a source file.
f=$(find "$CACHE_DIR" -name '*.go' 2>/dev/null | head -1)
if [ -n "$f" ] && b=$(wc -c <"$f" 2>/dev/null) && [ "${b:-0}" -gt 0 ]; then
	verdict "3 open a source file" "REACHED" "$(echo "$b" | tr -d ' ') bytes"
else
	verdict "3 open a source file" "blocked" "open failed"
fi

# 4. go doc — prints the original author's doc comments.
if command -v go >/dev/null 2>&1; then
	b=$(go doc "$MOD_PATH" 2>/dev/null | wc -c | tr -d ' ')
	[ "${b:-0}" -gt 40 ] && verdict "4 go doc" "REACHED" "$b bytes of doc" ||
		verdict "4 go doc" "blocked" "no output"
else
	verdict "4 go doc" "blocked" "no toolchain"
fi

# 5. Refetch into a relocated cache — defeats a merely-emptied one.
if command -v go >/dev/null 2>&1; then
	s=$(mktemp -d)
	(cd "$s" && go mod init probe.local/reach >/dev/null 2>&1 &&
		GOMODCACHE="$s/mc" GOFLAGS= go mod download "$MOD_PATH@$MOD_VER" >/dev/null 2>&1)
	n=$(find "$s/mc" -name '*.go' 2>/dev/null | wc -l | tr -d ' ')
	[ "${n:-0}" -gt 0 ] && verdict "5 go mod download" "REACHED" "$n .go files fetched" ||
		verdict "5 go mod download" "blocked" "fetch failed"
	# go marks module-cache files read-only; make them writable before removing.
	if [ -n "$s" ] && [ -d "$s" ]; then chmod -R u+w "$s" 2>/dev/null; rm -rf "$s"; fi
else
	verdict "5 go mod download" "blocked" "no toolchain"
fi

# 6. Raw network fetch, bypassing the go command entirely.
if command -v curl >/dev/null 2>&1; then
	b=$(curl -sS -m 25 "https://proxy.golang.org/$MOD_PATH/@v/$MOD_VER.zip" 2>/dev/null | wc -c | tr -d ' ')
	[ "${b:-0}" -gt 1000 ] && verdict "6 curl module zip" "REACHED" "$b bytes" ||
		verdict "6 curl module zip" "blocked" "$b bytes"
else
	verdict "6 curl module zip" "blocked" "no curl"
fi

echo
if [ "$reached" -eq 0 ]; then
	echo "=== all routes blocked ==="
	exit 0
fi
echo "=== $reached route(s) REACHED — this mechanism does not hold ==="
exit 1
