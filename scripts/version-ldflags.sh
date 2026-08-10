# shellcheck shell=bash
# version-ldflags.sh — single source of truth for the git commit and build
# timestamp every from-source build stamps into internal/version.{Commit,Date}.
# Source it, then build with:
#
#     source scripts/version-ldflags.sh
#     go build -ldflags "-X ${pkg}.Commit=$LIT_BUILD_COMMIT -X ${pkg}.Date=$LIT_BUILD_DATE" ...
#
# Both values come from local commands only (git rev-parse --short HEAD, the
# local system clock) — no network call, so a from-source build stamps
# identically on a restricted or air-gapped machine. [LAW:one-source-of-truth] the
# Justfile's `build` recipe and scripts/install.sh's source mode both source
# this and nowhere else computes these two strings, so they cannot drift
# apart the way install.sh's now-removed inline copy could have.
#
# Deliberately does NOT set Version. internal/version.Version == "" is the
# IsDev discriminator (internal/version/version.go) that
# internal/store/migration_runner.go's producer-binary-version guard relies
# on so an ordinary local dev build never overwrites a real release's
# downgrade stamp — see TestProducerBinaryVersionUnstampedForDevBuild. Only
# scripts/install.sh's own `git describe`-derived Version deliberately opts a
# checkout out of that guard; `just build` must not.

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
export LIT_BUILD_COMMIT LIT_BUILD_DATE
