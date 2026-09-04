#!/usr/bin/env bash
# release-kind.sh — THE resolver of master's release state.
#
# [LAW:one-source-of-truth] [LAW:single-enforcer] This is the single derivation
# of release state from (CHANGELOG × origin tags). Two consumers read it:
# release-validate.yml decides whether to build a real artifact and publish,
# and release-watchdog.yml decides whether a pending release has been left
# behind. A second copy of this parse in either place could drift — and a
# watchdog running a drifted copy is blind exactly when it is needed.
#
# stdout is exactly one verdict line — a closed grammar; consumers must match
# these shapes and fail loudly on anything else:
#   snapshot        CHANGELOG has no released version yet (only [Unreleased])
#   tagged X.Y.Z    the newest CHANGELOG version already has tag vX.Y.Z on origin
#   pending X.Y.Z   the newest CHANGELOG version has NO tag: a release is pending
# Every other state — missing CHANGELOG, malformed heading, tag-probe failure —
# exits 1 with an ::error line on stderr, never a silent fall-through to a
# verdict. [LAW:no-silent-failure]
#
# Requires: a checkout at the repo root with an `origin` remote (the tag probe
# asks origin for the live truth, so a stale local tag list cannot lie).
set -euo pipefail

# The newest release section is the first '## [...]' heading that is not
# [Unreleased]. Three cases, kept distinct so a typo can't silently skip a
# release: none (only [Unreleased]) -> snapshot; a well-formed '## [X.Y.Z]' ->
# tagged or pending by whether its tag exists; a version-ish heading that is
# MALFORMED -> loud error, never a silent fall-through. [LAW:no-silent-failure]
# Guard the file's existence FIRST, so a broken checkout or a moved/renamed
# CHANGELOG errors loudly instead of being swallowed by the pipeline's
# `|| true` and masquerading as "no releases yet". After this guard, `|| true`
# masks only the benign no-match (grep exit 1); a missing-file (grep exit 2)
# can no longer reach it.
test -f CHANGELOG.md || { echo "::error::CHANGELOG.md not found at repo root — broken checkout or moved file; refusing to resolve release state." >&2; exit 1; }
CAND=$(grep -E '^## \[' CHANGELOG.md | grep -vE '^## \[Unreleased\]' | head -1 || true)
if [ -z "$CAND" ]; then
  echo "snapshot"
elif ! echo "$CAND" | grep -qE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]( - .*)?$'; then
  echo "::error::newest CHANGELOG release heading is malformed: '$CAND' (expected '## [X.Y.Z] - <date>')" >&2
  exit 1
else
  VER=$(echo "$CAND" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')
  # [LAW:no-silent-failure] `git ls-remote --exit-code` is a three-way signal,
  # not a boolean: 0 = tag present, 2 = no matching ref, anything else (128
  # network/auth/missing-remote) = a real failure. An `if` collapses 2 AND
  # every error into "no tag" and would flag a spurious release the moment
  # origin is unreachable. Capture the code and branch on the actual value;
  # keep stderr visible so the error is loud.
  set +e
  git ls-remote --exit-code --tags origin "refs/tags/v$VER" >/dev/null
  r=$?
  set -e
  case "$r" in
    0) echo "tagged $VER" ;;
    2) echo "pending $VER" ;;
    *)
      echo "::error::git ls-remote failed (exit $r) probing refs/tags/v$VER — cannot determine release state; refusing to guess." >&2
      exit 1
      ;;
  esac
fi
