# shellcheck shell=bash
# version-ldflags.sh — single source of truth for the git commit, build
# timestamp and provenance every from-source build stamps into
# internal/version.{Commit,Date,Origin}.
# Source it, then build with:
#
#     source scripts/version-ldflags.sh
#     go build -ldflags "-X ${pkg}.Commit=$LIT_BUILD_COMMIT -X ${pkg}.Date=$LIT_BUILD_DATE -X ${pkg}.Origin=$LIT_BUILD_ORIGIN" ...
#
# None of the three needs the network: Commit and Date come from local commands
# (git rev-parse --short HEAD, the local system clock) and Origin is a literal,
# so a from-source build stamps identically on a restricted or air-gapped
# machine. [LAW:one-source-of-truth] the Justfile's `build` recipe and
# scripts/install.sh's source mode both source this and nowhere else computes
# these three strings, so they cannot drift apart.
#
# Deliberately does NOT set Version, and TestOnlyTheJustfileOmitsVersion pins
# that. internal/version.Version == "" is the IsDev discriminator
# (internal/version/version.go): `lit version` reports such a binary as a dev
# build, and `lit upgrade` never calls it already current. Only
# scripts/install.sh's own `git describe`-derived Version deliberately opts a
# checkout out of dev mode; `just build` must not.

# This file is meant to be sourced, not executed: it must export into the
# caller's shell. Executing it would set vars in a child that immediately exits.
if [ "${BASH_SOURCE[0]:-}" = "${0}" ]; then
    echo "version-ldflags.sh is meant to be sourced: 'source scripts/version-ldflags.sh'" >&2
    exit 64
fi

# [LAW:no-silent-failure] a build that cannot resolve HEAD (not a git checkout,
# corrupted repo) stops here with a clear message rather than silently
# stamping an empty commit that `lit version` would render as "unknown" —
# exactly the unstamped state this script exists to eliminate.
LIT_BUILD_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || true)"
if [ -z "$LIT_BUILD_COMMIT" ]; then
    echo "version-ldflags: 'git rev-parse --short HEAD' failed — not a git checkout, or history is unavailable" >&2
    unset LIT_BUILD_COMMIT
    return 1
fi
LIT_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Provenance. Both entrypoints that source this file build from a working tree,
# so both stamp the same value — the discriminator internal/version.FromSource
# reads to decide whether build age means anything. It lives here, beside Commit
# and Date, for the reason they do: one string, two callers, no way to drift.
# [LAW:one-source-of-truth]
LIT_BUILD_ORIGIN="source"
# The released-schema ceiling: the highest migration in the newest local
# release tag's registry, read by tools/schema-release-check, the same reading
# install.sh's guard makes. A lit built from a tree ahead of every release
# carries the release's ceiling, so it migrates no existing workspace past it
# and pushes no commit above it. With no local release tag the ceiling is 0.
# A stale tag list can only lower it, which fails closed.
# shellcheck source=scripts/release-tags.sh
. "$(dirname "${BASH_SOURCE[0]}")/release-tags.sh"
_lit_newest_tag="$(git tag --list | newest_release_tag)"
if ! LIT_BUILD_RELEASED_SCHEMA="$(GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go run ./tools/schema-release-check -released-schema "$_lit_newest_tag")"; then
    echo "version-ldflags: could not read the released schema of tag '${_lit_newest_tag}'" >&2
    unset _lit_newest_tag LIT_BUILD_RELEASED_SCHEMA
    return 1
fi
unset _lit_newest_tag
export LIT_BUILD_COMMIT LIT_BUILD_DATE LIT_BUILD_ORIGIN LIT_BUILD_RELEASED_SCHEMA
