#!/usr/bin/env bash
# Cross-build ICU for one target. Invoked from build/Dockerfile.release stages.
#
# Args:
#   $1 = goos
#   $2 = goarch
#   $3 = host triplet passed to ./configure --host=<triplet>
#        (e.g. `aarch64-linux-gnu`, `aarch64-apple-darwin`) — bare triplet,
#        no `--host=` prefix; the script adds the flag itself.
#   $4 = CC
#   $5 = CXX
#   $6 = AR  — archiver command (e.g. `zig-ar`, `aarch64-linux-gnu-ar`).
#        Must be a single executable name or path — no embedded spaces.
#   $7 = RANLIB — ranlib command (e.g. `zig-ranlib`). Same constraint as AR.
#
# Reads:
#   /tmp/icu-build/src/                       — ICU source tree
#   /tmp/icu-build/native-build/source/       — native build (--with-cross-build target)
#
# Writes:
#   /opt/icu/<goos>_<goarch>/{include,lib}    — installed cross-built ICU
#
# Parallelism: when invoked from multi-stage Dockerfile stages, several of
# these run concurrently across the build graph. The script uses
# `make -j$(nproc)` internally; with 3 parallel stages on a 4-vcpu runner
# this oversubscribes but the I/O-bound portions of ICU's build absorb it
# without significant slowdown vs the wall-clock win of parallelism.
set -euo pipefail

goos="$1"; goarch="$2"; triplet="$3"; cc="$4"; cxx="$5"; ar="$6"; ranlib="$7"
prefix="/opt/icu/${goos}_${goarch}"
builddir="/tmp/icu-build/cross-${goos}-${goarch}"

echo "=== ICU cross-build for ${goos}/${goarch}"
echo "    host=${triplet} cc=${cc} ar=${ar} ranlib=${ranlib}"
cp -r /tmp/icu-build/src "$builddir"
cd "$builddir/source"
CC="$cc" CXX="$cxx" AR="$ar" RANLIB="$ranlib" \
    ./configure --host="$triplet" --prefix="$prefix" \
        --with-cross-build=/tmp/icu-build/native-build/source \
        --enable-static --disable-shared \
        --disable-tests --disable-samples \
        --disable-tools --disable-extras --disable-icuio --disable-layoutex
        # --disable-tools: the only consumer of /opt/icu/<target>/ is
        #   go-icu-regex, which only links libicuuc/libicui18n/libicudata.
        #   ICU's own makeconv/genrb tools are build-time bootstrap (the
        #   native build provides them via --with-cross-build); building
        #   them again for the cross target failed for mingw because their
        #   target-side link line uses Windows-specific lib names that
        #   ICU's static-cross-build doesn't produce.
        # --disable-extras / --disable-icuio / --disable-layoutex: same
        #   reasoning — we don't link them, so don't build them.
make -j"$(nproc)"
# Pre-create the prefix subdirs. Windows static data (sicudt.a) installs to
# bin/ — ICU's makefile assumes `make install`'s earlier steps created it,
# but under --disable-tools nothing else writes to bin/, so we make it ourselves.
mkdir -p "$prefix"/{bin,lib,include}
make install
rm -rf "$builddir"
echo "=== ${goos}/${goarch} installed at ${prefix} ==="
