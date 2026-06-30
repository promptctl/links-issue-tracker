# shellcheck shell=bash
# cgo-env.sh — single source of truth for the cgo search paths the embedded
# Dolt engine needs when building `lit` FROM SOURCE. Source it before any
# `go build` / `go test` of this module:
#
#     source scripts/cgo-env.sh
#
# The shipped release binaries statically link ICU and zstd, so *installing*
# lit needs none of this — only compiling lit's own Go code does.
#
# Linux / other: the system packages (libicu-dev, libzstd-dev) sit on the
# toolchain's default search path — exactly as they do on the GitHub CI
# runners — so this script deliberately exports nothing. The host platform is
# the value that selects the paths; the same logic runs every invocation and
# simply resolves to empty off macOS. [LAW:dataflow-not-control-flow]
#
# macOS: Homebrew's icu4c/zstd are keg-only (off the default toolchain path),
# so cgo cannot find <unicode/regex.h> or the zstd libs without the -I/-L flags
# derived below.
#
# [LAW:one-source-of-truth] Every from-source build path — the Justfile recipes,
# scripts/install.sh, and `just setup`'s `go env -w` — derives these flags here
# and nowhere else, so they cannot drift apart.
# [LAW:no-silent-failure] On macOS, missing native deps abort with the exact
# `brew install` to run, rather than leaving the flags unset so the build dies
# later on the cryptic `unicode/regex.h file not found`.

# This file is meant to be sourced, not executed: it must export into the
# caller's shell. Executing it would set vars in a child that immediately exits.
if [ "${BASH_SOURCE[0]:-}" = "${0}" ]; then
    echo "cgo-env.sh is meant to be sourced: 'source scripts/cgo-env.sh'" >&2
    exit 64
fi

if [ "$(uname -s)" = "Darwin" ]; then
    if ! command -v brew >/dev/null 2>&1; then
        echo "cgo-env: building lit from source on macOS needs Homebrew to locate icu4c/zstd." >&2
        echo "         Install Homebrew (https://brew.sh), then: brew install icu4c@78 zstd" >&2
        return 1
    fi

    # Prefer the version the project pins and tests (icu4c@78); fall back to an
    # unversioned icu4c so a machine that already carries a compatible ICU still
    # builds. `|| true` keeps a missing keg from tripping the caller's `set -e`.
    _lit_icu="$(brew --prefix icu4c@78 2>/dev/null || true)"
    [ -n "$_lit_icu" ] || _lit_icu="$(brew --prefix icu4c 2>/dev/null || true)"
    _lit_zstd="$(brew --prefix zstd 2>/dev/null || true)"

    if [ -z "$_lit_icu" ] || [ ! -e "$_lit_icu/include/unicode/regex.h" ]; then
        echo "cgo-env: ICU headers not found. Run: brew install icu4c@78" >&2
        unset _lit_icu _lit_zstd
        return 1
    fi
    if [ -z "$_lit_zstd" ] || [ ! -e "$_lit_zstd/include/zstd.h" ]; then
        echo "cgo-env: zstd headers not found. Run: brew install zstd" >&2
        unset _lit_icu _lit_zstd
        return 1
    fi

    # CGO_CPPFLAGS feeds both the C and the C++ preprocessor, so one variable
    # covers go-icu-regex's .cpp sources and dolt's .c sources — no separate
    # CGO_CFLAGS / CGO_CXXFLAGS needed. Prepend ours; preserve anything the
    # caller already set.
    export CGO_CPPFLAGS="-I$_lit_icu/include -I$_lit_zstd/include${CGO_CPPFLAGS:+ $CGO_CPPFLAGS}"
    export CGO_LDFLAGS="-L$_lit_icu/lib -L$_lit_zstd/lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
    unset _lit_icu _lit_zstd
fi
