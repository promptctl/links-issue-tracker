#!/usr/bin/env bash
set -euo pipefail

# Print the next release tag, given the bump kind, derived from the latest
# semver tag on the repo. This is the ONE place the version-bump arithmetic
# lives, so the "minor resets patch to 0" rule can never be hand-rolled wrong at
# a callsite. [LAW:one-source-of-truth]
#
# Policy this repo follows (see CONTRIBUTING.md "Cutting a release"):
#   - major is FROZEN — never bumped by this script.
#   - minor = a feature or any presumed-breaking change.
#   - patch = a pure bugfix.
#
# Pure: reads git tags, writes the answer to stdout and nothing else, so a
# caller can `NEXT=$(scripts/next-version.sh minor)`. [LAW:effects-at-boundaries]
#
# Usage:
#   scripts/next-version.sh minor   # v0.1.0 -> v0.2.0
#   scripts/next-version.sh patch   # v0.1.0 -> v0.1.1

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/next-version.sh <minor|patch>" >&2
  exit 2
fi

BUMP="$1"
# The accept-set is exactly {minor, patch}; "major" is rejected by construction
# because this repo never cuts one. [LAW:types-are-the-program]
if [[ "$BUMP" != "minor" && "$BUMP" != "patch" ]]; then
  echo "bump must be 'minor' or 'patch' (major is frozen for this repo); got: $BUMP" >&2
  exit 2
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# Latest CLEAN semver tag: `vX.Y.Z` with no prerelease suffix. The `-*-*` in a
# refname-sorted list would sort a `v0.2.0-rc1` above `v0.1.9`, so filter
# prereleases out — the bump must build on the last real release, not an rc.
# [LAW:no-silent-failure] if there is no such tag we stop rather than inventing
# a base version, because the first release is a deliberate human act.
LATEST="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname \
  | grep -vE '-' \
  | head -1)"
if [[ -z "$LATEST" ]]; then
  echo "no vX.Y.Z tag found to bump from; create the first release tag by hand" >&2
  exit 3
fi

if ! [[ "$LATEST" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "latest tag is not vMAJOR.MINOR.PATCH: $LATEST" >&2
  exit 3
fi
MAJOR="${BASH_REMATCH[1]}"
MINOR="${BASH_REMATCH[2]}"
PATCH="${BASH_REMATCH[3]}"

# Same operations run every time; only the arithmetic differs by bump kind, and
# the case is over the domain's own two-value enum. [LAW:dataflow-not-control-flow]
case "$BUMP" in
  minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
  patch) PATCH=$((PATCH + 1)) ;;
esac

NEXT="v${MAJOR}.${MINOR}.${PATCH}"

# Guard against re-cutting: if the computed tag already exists, the caller's
# assumption about "latest" is stale. Fail loud. [LAW:no-silent-failure]
if git rev-parse "$NEXT" >/dev/null 2>&1; then
  echo "computed next tag already exists: $NEXT (is your master up to date?)" >&2
  exit 4
fi

echo "$NEXT"
