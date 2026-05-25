#!/usr/bin/env bash
#
# install.sh — install `lit` from one of three sources:
#
#   (default)              build from this checkout's source (go build, ldflag-stamped)
#   --from-release <tag>   download the published release archive for that tag
#   --latest-release       download the latest published release archive
#
# All modes write to the same target directory (the dir on $PATH that already
# owns a `lit` if any; otherwise $GOBIN) and run the same stale-binary
# detector. Switching modes is a single flag, not a different script.
#
# [LAW:single-enforcer] One installer, one target-resolution rule, one stale
# detector. The "what to install" varies; "where + safety checks" do not.
# [LAW:one-source-of-truth] Source builds inject version/commit/date via
# ldflags so `lit version` reports something meaningful even for ad-hoc
# checkouts; release-download mode trusts the prebuilt binary's already-baked
# stamps (set by goreleaser).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

REPO_DOWNLOAD_BASE="https://github.com/brandon-fryslie/links-issue-tracker/releases/download"
REPO_LATEST_API="https://api.github.com/repos/brandon-fryslie/links-issue-tracker/releases/latest"

# realpath_compat — resolve a symlink chain to its canonical absolute path.
#
# [LAW:single-enforcer] One resolver used by both the target-dir lookup and
# the stale-binary detector — they previously each carried their own
# readlink-or-python3 chain with subtly different error handling.
#
# Tool cascade, in order of preference:
#   1. `realpath`     — modern coreutils (Linux) and BSD utils (macOS 10.11+)
#   2. `readlink -f`  — GNU coreutils; BSD readlink (older macOS) lacks -f
#   3. pure-shell     — walks the link chain manually, works anywhere POSIX
#
# Release-download mode is explicitly Go-free, and this script must not require
# python3 either; the pure-shell branch is the genuine last-resort that needs
# no external tools at all. (Previous versions invoked `python3` as a fallback
# and would error out on minimal environments that lacked it.)
realpath_compat() {
    local path="$1"
    if command -v realpath >/dev/null 2>&1; then
        realpath "$path"
        return
    fi
    if readlink -f / >/dev/null 2>&1; then
        readlink -f "$path"
        return
    fi
    # Pure-shell branch (no preferred-resolver dependency): if bare `readlink`
    # exists, walk the symlink chain with it; either way, canonicalize the
    # final directory portion via `cd ... && pwd -P`. `readlink` (without
    # flags) is NOT POSIX — it's a BSD/GNU/busybox extension — so when it's
    # absent we degrade by skipping the symlink walk and canonicalizing the
    # input path directly. Non-symlinked paths get the correct answer either
    # way; symlinked paths on a system without any of `realpath`, GNU
    # `readlink -f`, or bare `readlink` return as-given, which is the best
    # we can do with only POSIX tools (`cd`, `pwd -P`, `dirname`, `basename`).
    # Arithmetic loop, not `seq` — `seq` is not POSIX.
    # Cap depth at 40, matching POSIX SYMLOOP_MAX.
    local target="$path" i=0 link dir
    if command -v readlink >/dev/null 2>&1; then
        while [ "$i" -lt 40 ]; do
            [ -L "$target" ] || break
            link="$(readlink "$target")"
            case "$link" in
                /*) target="$link" ;;
                *)  target="$(dirname "$target")/$link" ;;
            esac
            i=$((i + 1))
        done
    fi
    dir="$(cd "$(dirname "$target")" 2>/dev/null && pwd -P || true)"
    if [ -n "$dir" ]; then
        echo "$dir/$(basename "$target")"
    else
        echo "$path"
    fi
}

mode="source"
release_tag=""
# `mode_flag_seen` tracks whether a mode-selecting flag has been passed.
# `--from-release` and `--latest-release` are mutually exclusive — accepting
# both silently (last-wins) means an accidental extra flag changes the
# install source without warning. [LAW:no-mode-explosion] one mode per
# invocation; conflicts are a usage error, not last-wins.
mode_flag_seen=""
while [ $# -gt 0 ]; do
    case "$1" in
        --from-release)
            if [ -n "$mode_flag_seen" ]; then
                echo "error: cannot combine $mode_flag_seen with --from-release" >&2
                echo "usage: $0 [--from-release <tag>|--latest-release]" >&2
                exit 2
            fi
            mode_flag_seen="--from-release"
            mode="release"
            release_tag="${2:-}"
            if [ -z "$release_tag" ]; then
                echo "error: --from-release requires a tag (e.g. v0.1.0)" >&2
                exit 2
            fi
            shift 2
            ;;
        --latest-release)
            if [ -n "$mode_flag_seen" ]; then
                echo "error: cannot combine $mode_flag_seen with --latest-release" >&2
                echo "usage: $0 [--from-release <tag>|--latest-release]" >&2
                exit 2
            fi
            mode_flag_seen="--latest-release"
            mode="latest"
            shift
            ;;
        -h|--help)
            sed -n '3,15p' "$0"
            exit 0
            ;;
        *)
            echo "error: unknown flag: $1" >&2
            echo "usage: $0 [--from-release <tag>|--latest-release]" >&2
            exit 2
            ;;
    esac
done

# --- target-dir resolution: identical across all modes -----------------------
#
# Priority: existing-lit-on-PATH > $GOBIN env > `go env GOBIN/GOPATH` (if Go
# is installed) > $HOME/.local/bin (universal fallback). Release-download
# modes do NOT require Go, so the go-env path must degrade gracefully.

TARGET_DIR=""
# `type -P` returns ONLY a filesystem path — empty string for shell
# functions, aliases, and builtins. `command -v` would return the name
# itself in those cases, which `realpath_compat`/`dirname` would treat
# as relative to CWD and silently install into the repo. [LAW:types-are-the-program]
# the value flowing into TARGET_DIR has to be a real executable path or
# unset; we narrow the predicate so the wrong shape can't reach the body.
EXISTING="$(type -P lit 2>/dev/null || true)"
if [ -n "$EXISTING" ] && [ -x "$EXISTING" ]; then
    # Resolve symlinks so we update the real file, not a dangling link.
    REAL_EXISTING="$(realpath_compat "$EXISTING")"
    TARGET_DIR="$(dirname "$REAL_EXISTING")"
fi

if [ -z "$TARGET_DIR" ]; then
    TARGET_DIR="${GOBIN:-}"
fi

if [ -z "$TARGET_DIR" ] && command -v go >/dev/null 2>&1; then
    TARGET_DIR="$(go env GOBIN 2>/dev/null || true)"
    if [ -z "$TARGET_DIR" ]; then
        GOPATH_BIN="$(go env GOPATH 2>/dev/null | cut -d: -f1)"
        if [ -n "$GOPATH_BIN" ]; then
            TARGET_DIR="$GOPATH_BIN/bin"
        fi
    fi
fi

# Universal fallback. Works without Go installed.
TARGET_DIR="${TARGET_DIR:-$HOME/.local/bin}"
mkdir -p "$TARGET_DIR"

# --- mode dispatch -----------------------------------------------------------

case "$mode" in
    source)
        # ldflag-stamp the build so `lit version` reports meaningful identity
        # for source builds (releases stamp via goreleaser; this is the
        # equivalent for ad-hoc checkouts). If we have NO git metadata
        # (checkout from a tarball, broken repo, etc.), leave Version EMPTY
        # so `lit version` honestly reports `lit dev (...)`. A spoofed
        # placeholder like "dev" would make version.IsDev report FALSE and
        # mislead downstream tooling.
        ver="$(git describe --tags --always --dirty 2>/dev/null || true)"
        commit="$(git rev-parse --short HEAD 2>/dev/null || true)"
        date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        pkg="github.com/bmf/links-issue-tracker/internal/version"
        GOFLAGS="${GOFLAGS:+$GOFLAGS }-buildvcs=false" go build \
            -ldflags "-X ${pkg}.Version=${ver} -X ${pkg}.Commit=${commit} -X ${pkg}.Date=${date}" \
            -o "$TARGET_DIR/lit" ./cmd/lit
        ;;
    release|latest)
        # curl is the single transport for release-download mode. Probe it
        # explicitly so a missing-curl environment gets a targeted error
        # instead of `set -e` surfacing a generic "command not found" on the
        # first curl invocation. Mirrors the sha256sum/shasum probe shape.
        if ! command -v curl >/dev/null 2>&1; then
            echo "error: release-download mode requires curl (install curl, or use the source build: bash scripts/install.sh)" >&2
            exit 1
        fi
        if [ "$mode" = "latest" ]; then
            # jq is needed ONLY to parse the GitHub API response for the
            # latest-release tag lookup. --from-release <tag> does not need
            # jq (the tag is provided directly), so check is scoped here.
            if ! command -v jq >/dev/null 2>&1; then
                echo "error: --latest-release requires jq (install jq, or use --from-release <tag>)" >&2
                exit 1
            fi
            release_tag="$(curl -fsSL "$REPO_LATEST_API" | jq -r .tag_name)"
            if [ -z "$release_tag" ] || [ "$release_tag" = "null" ]; then
                echo "error: could not resolve latest release tag from $REPO_LATEST_API" >&2
                exit 1
            fi
            echo "Latest release: $release_tag"
        fi

        # Normalize the tag at the boundary: strip any leading `v` then
        # always prepend one. Idempotent — accepts both `v0.1.0` (canonical,
        # how the user reads tags on the Releases page) and `0.1.0` (what
        # they get from `git describe --abbrev=0 --tags` minus the prefix)
        # and produces the canonical v-prefixed form the rest of the script
        # speaks. Skipping this is what caused 404 download URLs when a
        # user passed `--from-release 0.1.0` (the URL path segment is the
        # *tag*, v-prefixed; the archive filename uses the *version*, v-
        # stripped — see archive_version below). [LAW:types-are-the-program]
        # the boundary normalizer makes the v-stripped input shape map to
        # the same canonical value as the v-prefixed input shape.
        release_tag="v${release_tag#v}"

        # Validate the normalized tag against the actual producer shape.
        # release.yml's job-guard rejects tags containing `-`, so the only
        # tags goreleaser ever publishes are exactly `vX.Y.Z` (numeric
        # semver). Reject anything else BEFORE the value flows into
        # filepath / curl / mv — a `--from-release ../x` or an API response
        # tainted with path separators would otherwise interpolate into
        # `"$tmp/$archive"` and write outside the temp directory.
        # [LAW:types-are-the-program] reject illegal tag shapes at the
        # boundary so downstream code can trust that release_tag is a
        # safe, expected canonical value.
        if [[ ! "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "error: release tag '$release_tag' is not a canonical semver release tag (expected vX.Y.Z)" >&2
            echo "       --from-release accepts only tags published by the release pipeline" >&2
            exit 1
        fi

        # Resolve current platform → goreleaser archive name. Must mirror
        # .goreleaser.yml's name_template: lit_<version>_<goos>_<goarch>.<ext>
        # where <version> is goreleaser's .Version — the tag with the leading
        # `v` STRIPPED. We strip it here too.
        archive_version="${release_tag#v}"

        os="$(uname -s | tr '[:upper:]' '[:lower:]')"
        arch_raw="$(uname -m)"
        case "$arch_raw" in
            x86_64|amd64)  arch="amd64" ;;
            arm64|aarch64) arch="arm64" ;;
            *) echo "error: unsupported architecture: $arch_raw" >&2; exit 1 ;;
        esac
        case "$os" in
            linux)  ext="tar.gz" ;;
            darwin) ext="tar.gz"
                # Reject Intel Mac immediately — the release pipeline does not
                # produce a darwin/amd64 artifact (tracked as links-downgrade-t244.6).
                if [ "$arch" = "amd64" ]; then
                    echo "error: Intel Mac (darwin/amd64) is not currently in the release matrix" >&2
                    echo "       build from source: bash scripts/install.sh   (omit --from-release)" >&2
                    exit 1
                fi
                ;;
            mingw*|msys*|cygwin*) os="windows"; ext="zip"
                # Same: windows not in the matrix yet (links-downgrade-t244.7).
                echo "error: Windows is not currently in the release matrix" >&2
                echo "       build from source: bash scripts/install.sh" >&2
                exit 1
                ;;
            *) echo "error: unsupported OS: $os" >&2; exit 1 ;;
        esac
        archive="lit_${archive_version}_${os}_${arch}.${ext}"
        url="${REPO_DOWNLOAD_BASE}/${release_tag}/${archive}"
        checksums_url="${REPO_DOWNLOAD_BASE}/${release_tag}/checksums.txt"

        # Temp dir on the SAME filesystem as TARGET_DIR so the final rename
        # is genuinely atomic (cross-FS mv is a copy+unlink, not atomic).
        tmp="$(mktemp -d "$TARGET_DIR/.lit-install.XXXXXX")"
        trap 'rm -rf "$tmp"' EXIT

        echo "Downloading $url ..."
        curl -fsSL -o "$tmp/$archive" "$url"
        echo "Downloading checksums ..."
        curl -fsSL -o "$tmp/checksums.txt" "$checksums_url"

        # awk match against the FULL second field — avoids grep's regex
        # interpretation (the dots in .tar.gz would otherwise be "any char").
        expected="$(awk -v want="$archive" '$2 == want { print $1 }' "$tmp/checksums.txt")"
        if [ -z "$expected" ]; then
            echo "error: $archive not found in checksums.txt" >&2
            exit 1
        fi
        # sha256sum (GNU coreutils / Linux) or shasum -a 256 (Perl / macOS).
        # Fail loudly with a specific error if neither is present, rather than
        # letting `set -e` surface the second tool's "command not found" —
        # that lower-level error doesn't tell the operator what to install.
        if command -v sha256sum >/dev/null 2>&1; then
            actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
        elif command -v shasum >/dev/null 2>&1; then
            actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
        else
            echo "error: install.sh release-download mode requires either 'sha256sum' (GNU coreutils) or 'shasum' (Perl, default on macOS)" >&2
            echo "       install one of those tools, or use the source build: bash scripts/install.sh   (omit --from-release)" >&2
            exit 1
        fi
        if [ "$actual" != "$expected" ]; then
            echo "error: SHA256 mismatch for $archive" >&2
            echo "  expected: $expected" >&2
            echo "  actual:   $actual" >&2
            exit 1
        fi
        echo "Checksum OK."

        # Validate the archive's structure BEFORE extracting. The checksum
        # verifies content integrity but NOT structural safety — a compromised
        # release artifact could carry path-traversal entries (`../`, absolute
        # paths) or non-regular entries (symlinks pointing outside $tmp,
        # hardlinks, devices) that escape extraction. goreleaser produces a
        # FLAT archive of regular files only (`lit` + LICENSE* + README*); we
        # enforce that accept shape at the boundary so the extraction body can
        # assume a safe input. [LAW:types-are-the-program] reject illegal
        # archive shapes by construction rather than guarding inside extraction.
        #
        # Pipe-into-while would run the loop in a subshell; `exit 1` would not
        # terminate the installer. Capture the listing first, then iterate via
        # a here-string so the loop runs in the main shell.
        case "$ext" in
            tar.gz)
                # Names must be flat: no `/`, no `..`, no leading `/`.
                entries="$(tar -tzf "$tmp/$archive")"
                while IFS= read -r name; do
                    [ -z "$name" ] && continue
                    case "$name" in
                        .|..|*/*|/*)
                            echo "error: archive entry has unsafe path: $name" >&2
                            exit 1 ;;
                    esac
                done <<< "$entries"
                # Types must be regular files only. Both GNU and BSD `tar
                # -tvzf` emit the file-type char in column 1: `-` regular,
                # `l` symlink, `h` hardlink, `d` directory.
                verbose="$(tar -tvzf "$tmp/$archive")"
                while IFS= read -r line; do
                    [ -z "$line" ] && continue
                    case "${line:0:1}" in
                        -) ;;
                        *)
                            echo "error: archive contains non-regular entry: $line" >&2
                            exit 1 ;;
                    esac
                done <<< "$verbose"
                ;;
            zip)
                # Flat-name check rejects the realistic path-traversal vector
                # (Zip Slip). `*\\*` rejects backslash separators that some
                # producers emit even though POSIX zip uses `/`.
                entries="$(unzip -Z1 "$tmp/$archive")"
                while IFS= read -r name; do
                    [ -z "$name" ] && continue
                    case "$name" in
                        .|..|*/*|/*|*\\*)
                            echo "error: archive entry has unsafe path: $name" >&2
                            exit 1 ;;
                    esac
                done <<< "$entries"
                ;;
        esac

        # Extract into the temp dir; goreleaser archives contain a top-level `lit` binary.
        if [ "$ext" = "tar.gz" ]; then
            tar -xzf "$tmp/$archive" -C "$tmp"
        else
            unzip -q "$tmp/$archive" -d "$tmp"
        fi
        if [ ! -x "$tmp/lit" ]; then
            echo "error: extracted archive did not contain a 'lit' binary" >&2
            exit 1
        fi

        # Atomic rename within $TARGET_DIR (mktemp put us on the same FS).
        mv -f "$tmp/lit" "$TARGET_DIR/lit"
        ;;
esac

# Stale `lnks` symlink/binary from previous installs is removed; `lit` is the
# only entrypoint going forward.
rm -f "$TARGET_DIR/lnks"

# Detect any *other* `lit` on PATH that we did NOT just overwrite — those are
# the stale binaries that cause "the fix landed but the bug came back" reports.
# Compare realpath-resolved candidates against the realpath of the just-
# installed binary; if $TARGET_DIR happens to live under a symlinked PATH
# entry (e.g. /usr/local/bin -> /opt/homebrew/bin on macOS), the literal
# "$TARGET_DIR/lit" string would never match the canonicalized candidate
# and the binary we just wrote would be flagged stale.
# [LAW:types-are-the-program] both sides of the comparison are canonical
# absolute paths, so "is this the same file?" is well-defined.
INSTALLED_REAL="$(realpath_compat "$TARGET_DIR/lit")"
STALE=()
IFS=':' read -r -a PATH_ENTRIES <<< "$PATH"
for dir in "${PATH_ENTRIES[@]}"; do
    [ -z "$dir" ] && continue
    candidate="$dir/lit"
    [ -x "$candidate" ] || continue
    real="$(realpath_compat "$candidate")"
    if [ "$real" != "$INSTALLED_REAL" ]; then
        STALE+=("$candidate")
    fi
done

echo "Installed lit -> $TARGET_DIR/lit"
"$TARGET_DIR/lit" version 2>/dev/null || true
if [ "${#STALE[@]}" -gt 0 ]; then
    echo
    echo "WARNING: other 'lit' binaries found on PATH that were NOT updated:"
    for s in "${STALE[@]}"; do echo "  $s"; done
    echo "Remove them or shadow them, or future fixes will not reach the binary you actually run."
fi
