#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: ./scripts/release.sh <version-tag>" >&2
  echo "example: ./scripts/release.sh v0.1.0" >&2
  exit 2
fi

VERSION_TAG="$1"

if ! [[ "$VERSION_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "version tag must match vMAJOR.MINOR.PATCH" >&2
  exit 3
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# [LAW:single-enforcer] The release boundary is the one place that enforces
# "a tagged version is a documented version" — promote [Unreleased] -> [X.Y.Z]
# (Keep a Changelog) before the tag exists, so an undocumented release can't be cut.
CHANGELOG_VERSION="${VERSION_TAG#v}"
if [[ ! -r CHANGELOG.md ]]; then
  echo "CHANGELOG.md not found or not readable at repo root" >&2
  exit 6
fi
if ! grep -qE "^## \[${CHANGELOG_VERSION//./\\.}\]" CHANGELOG.md; then
  echo "CHANGELOG.md has no '## [${CHANGELOG_VERSION}]' section" >&2
  echo "promote '## [Unreleased]' to '## [${CHANGELOG_VERSION}] - $(date +%Y-%m-%d)' before releasing" >&2
  exit 6
fi

go test ./...

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is dirty; commit or stash before release" >&2
  exit 4
fi

if git rev-parse "$VERSION_TAG" >/dev/null 2>&1; then
  echo "tag already exists: $VERSION_TAG" >&2
  exit 5
fi

git tag -a "$VERSION_TAG" -m "release $VERSION_TAG"

REMOTE_NAME="${REMOTE_NAME:-origin}"
if git remote get-url "$REMOTE_NAME" >/dev/null 2>&1; then
  git push "$REMOTE_NAME" HEAD
  git push "$REMOTE_NAME" "$VERSION_TAG"
  echo "released $VERSION_TAG to remote $REMOTE_NAME"
else
  echo "created local tag $VERSION_TAG; no remote named '$REMOTE_NAME' configured"
fi
