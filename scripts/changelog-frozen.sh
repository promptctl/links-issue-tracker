#!/usr/bin/env bash
# changelog-frozen.sh — a CHANGELOG section that has shipped is immutable, and
# this is the proof.
#
# The defect it exists to stop (links-release-3ixp): a ticket PR branched while
# `## [Unreleased]` is the live heading writes its entry there, exactly as
# RELEASING.md requires. A release promotion then renames that heading to
# `## [X.Y.Z]` and opens a fresh empty `## [Unreleased]` above it. The ticket PR
# merges with no conflict and its addition lands under the RELEASED heading —
# an insertion never reads the heading it lands beneath. The tagged notes then
# describe code the tag does not contain, and the pending section loses the
# entry: one bullet, two wrong releases. That happened on 2026-09-07 (#498 into
# v0.14.0) and every gate in this repo was green over it.
#
# The check: origin's newest release tag is the authority on what shipped, and
# HEAD's CHANGELOG is a map of it. Everything from that tag's own version
# heading down to end-of-file is the FROZEN REGION — the notes for that release
# and for every release before it — and it must be byte-identical to the same
# region in the tag's own CHANGELOG. [LAW:one-source-of-truth]
#
# Why the whole tail, and not section-by-section against each section's own tag:
# v0.1.0's CHANGELOG carries no `## [0.1.0]` section at all — it was written
# after the tag was cut, back when releases were promoted by hand. A per-section
# check has to carry a hardcoded exemption for that, which is a second and
# drifting record of which sections are "really" frozen. Comparing the tail as
# ONE region against the newest tag's copy dissolves it: 0.1.0's section sits
# INSIDE the region, so it is checked against a tag that does record it. One
# comparison, one tag, no exemption list, every past release covered.
#
# A pending release is free by construction, with no case for it here: under a
# `pending X.Y.Z` state the newest TAG is still the previous release, so the
# not-yet-tagged `## [X.Y.Z]` section sits ABOVE the frozen region and the
# promotion PR writes it freely. The tag is a value that moves the region's
# boundary, never a condition selecting a code path.
# [LAW:dataflow-not-control-flow]
#
# The exit code is the contract: 0 = the released notes are intact, 1 = they are
# not, or the question could not be answered. Every anomaly — an unreachable
# origin, a tag whose objects are not local, a released heading deleted from
# HEAD, a tag that never recorded its own section — takes the 1 with an ::error
# line naming it, and none of them can fall through to a quiet pass.
# [LAW:no-silent-failure]
#
# Requires: a checkout at the repo root with an `origin` remote AND the release
# tags present locally (`actions/checkout` needs `fetch-depth: 0`). This script
# reads and never fetches, so it cannot deepen or reshape the repo it runs in;
# the caller owns having fetched, and is told so loudly when it has not.
# [LAW:effects-at-boundaries]
set -euo pipefail

test -f CHANGELOG.md || { echo "::error::CHANGELOG.md not found at repo root — broken checkout or moved file; refusing to certify anything about released notes." >&2; exit 1; }

# Ask ORIGIN which release tags exist, never the local tag list. A local list
# missing a just-pushed tag would shrink the frozen region to the previous
# release and wave the newest section through — a false pass precisely when the
# notes are newest and least reviewed. release-kind.sh probes origin for the
# same reason. [LAW:no-silent-failure]
set +e
RAW=$(git ls-remote --tags origin 'refs/tags/v*')
r=$?
set -e
if [ "$r" -ne 0 ]; then
  echo "::error::git ls-remote failed (exit $r) listing origin's release tags — cannot determine which notes are frozen; refusing to guess." >&2
  exit 1
fi

# A release tag in this repo is exactly `vX.Y.Z` — the shape release-validate.yml
# cuts. Matching it strictly is what defines the accept-set: the `^{}` peeled
# entries annotated tags emit and any hand-made non-release tag fail the match
# and are gone, rather than being stripped case by case downstream.
# [LAW:parse-dont-validate]
REFS=$(echo "$RAW" | awk '{print $2}')
# The lone masked exit is grep's benign "no lines matched" (1), which is the
# real state of a repo that has never released. grep gets its own line so that
# `|| true` covers nothing else. [LAW:no-silent-failure]
RELEASE_REFS=$(echo "$REFS" | grep -E '^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' || true)
if [ -z "$RELEASE_REFS" ]; then
  echo "no release tag on origin — nothing has shipped, so no notes are frozen yet"
  exit 0
fi

VER=$(echo "$RELEASE_REFS" | sed 's|^refs/tags/v||' | sort -V | tail -1)
TAG="v$VER"

# `--verify --quiet` is a probe, not a silencer: it asks whether the object is
# here, and both answers are acted on loudly.
if ! git rev-parse --verify --quiet "$TAG^{commit}" >/dev/null; then
  echo "::error::origin's newest release tag $TAG is not in this checkout, so its notes cannot be compared against what it shipped. Fetch the tags — actions/checkout needs 'fetch-depth: 0'; locally, 'git fetch origin --tags'." >&2
  exit 1
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ONE extraction, called for both sides. Two copies of this parse could disagree
# about where the region starts and manufacture a diff — or bury one.
# [LAW:one-source-of-truth]
#
# index(...) == 1 rather than a regex: the heading is a literal full of regex
# metacharacters ('[', '.'), and matching it literally keeps the rule free of
# escaping. The closing ']' is what makes it exact — '## [0.14.0]' cannot match
# '## [0.14.01] - ...'.
frozen_region() {  # $1 = file, $2 = version
  awk -v v="$2" 'index($0, "## [" v "]") == 1 { f = 1 } f' "$1"
}

git show "$TAG:CHANGELOG.md" > "$TMP/tag-changelog.md" || {
  echo "::error::$TAG has no CHANGELOG.md — cannot establish what it shipped." >&2
  exit 1
}

frozen_region "$TMP/tag-changelog.md" "$VER" > "$TMP/shipped"
frozen_region CHANGELOG.md "$VER" > "$TMP/current"

if [ ! -s "$TMP/shipped" ]; then
  echo "::error::$TAG's own CHANGELOG has no '## [$VER]' section, so the tag does not record what it released and nothing can be proved against it. (That is the shape v0.1.0 has, promoted by hand after its tag was cut; a tag cut by release-validate.yml always contains its own section.)" >&2
  exit 1
fi

if [ ! -s "$TMP/current" ]; then
  echo "::error::CHANGELOG.md at HEAD has no '## [$VER]' section, but $TAG shipped one — the released notes were deleted or the heading renamed. Restore the section exactly as $TAG records it." >&2
  exit 1
fi

if ! diff -u "$TMP/shipped" "$TMP/current" > "$TMP/drift"; then
  echo "::error::released notes changed: everything from '## [$VER]' down is frozen by tag $TAG, and this tree edits it." >&2
  echo "--- what $TAG shipped" >&2
  echo "+++ what this tree claims $TAG shipped" >&2
  # Drop diff's own ---/+++ header lines; the two above name the sides in the
  # terms that matter here, rather than by temp-file path.
  sed -n '3,$p' "$TMP/drift" >&2
  echo "::error::A ticket entry belongs under '## [Unreleased]'. A shipped section never changes: the tag is the record of what that release contained, and editing the section makes the record lie in both directions — the tag gains notes for code it does not have, and the pending release loses them." >&2
  exit 1
fi

echo "frozen $VER — released notes match tag $TAG ($(wc -c < "$TMP/current" | tr -d ' ') bytes)"
