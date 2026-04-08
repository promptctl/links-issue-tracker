#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# [LAW:one-source-of-truth] Install to whichever `lit` the user's PATH actually
# resolves. Writing to $GOBIN while the shell picks up a stale binary elsewhere
# is how rank-overflow fixes kept "landing" but never reaching the running tool.
GOBIN="${GOBIN:-$(go env GOBIN)}"
GOBIN="${GOBIN:-$(go env GOPATH | cut -d: -f1)/bin}"

TARGET_DIR=""
if command -v lit >/dev/null 2>&1; then
    EXISTING="$(command -v lit)"
    # Resolve symlinks so we update the real file, not a dangling link.
    REAL_EXISTING="$(readlink -f "$EXISTING" 2>/dev/null || python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$EXISTING")"
    TARGET_DIR="$(dirname "$REAL_EXISTING")"
fi
TARGET_DIR="${TARGET_DIR:-$GOBIN}"

mkdir -p "$TARGET_DIR"
GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go build -o "$TARGET_DIR/lit" ./cmd/lit

# `lnks` remains as a compatibility entrypoint for local projects still referencing it.
ln -sf "$TARGET_DIR/lit" "$TARGET_DIR/lnks"

# Detect any *other* `lit` on PATH that we did NOT just overwrite — those are
# the stale binaries that cause "the fix landed but the bug came back" reports.
STALE=()
IFS=':' read -r -a PATH_ENTRIES <<< "$PATH"
for dir in "${PATH_ENTRIES[@]}"; do
    [ -z "$dir" ] && continue
    candidate="$dir/lit"
    [ -x "$candidate" ] || continue
    real="$(readlink -f "$candidate" 2>/dev/null || python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$candidate")"
    if [ "$real" != "$TARGET_DIR/lit" ]; then
        STALE+=("$candidate")
    fi
done

echo "Installed lit -> $TARGET_DIR/lit (lnks symlink created for compatibility)"
if [ "${#STALE[@]}" -gt 0 ]; then
    echo
    echo "WARNING: other 'lit' binaries found on PATH that were NOT updated:"
    for s in "${STALE[@]}"; do echo "  $s"; done
    echo "Remove them or shadow them, or future fixes will not reach the binary you actually run."
fi
