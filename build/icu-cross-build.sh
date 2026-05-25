#!/usr/bin/env bash
# Cross-build ICU for one target. Invoked from build/Dockerfile.release stages.
#
# Args:
#   $1 = goos
#   $2 = goarch
#   $3 = configure --host=<triplet>
#   $4 = CC
#   $5 = CXX
#   $6 = AR_GLOB — shell glob that resolves to the cross-ar binary. ICU
#        needs the cross-toolchain's ar (osxcross / mingw / gnu-binutils) so
#        its static archives are in the format the cross-linker accepts; the
#        host's GNU ar produces archives macOS ld rejects.
#   $7 = extra PATH dir (e.g. /usr/local/osxcross/bin) or empty
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

goos="$1"; goarch="$2"; triplet="$3"; cc="$4"; cxx="$5"; ar_glob="$6"; path_extra="$7"
prefix="/opt/icu/${goos}_${goarch}"
builddir="/tmp/icu-build/cross-${goos}-${goarch}"

# Resolve AR/RANLIB from the glob.
shopt -s nullglob
ar_matches=( ${ar_glob}-ar )
ranlib_matches=( ${ar_glob}-ranlib )
shopt -u nullglob
if [ ${#ar_matches[@]} -eq 0 ] || [ ${#ranlib_matches[@]} -eq 0 ]; then
    echo "FATAL: no ar/ranlib for glob '${ar_glob}'" >&2
    echo "available tools matching the prefix:" >&2
    ls -1 ${ar_glob}* 2>/dev/null | head -20 >&2 || echo "(none)" >&2
    exit 1
fi
AR_BIN="${ar_matches[0]}"
RANLIB_BIN="${ranlib_matches[0]}"

echo "=== ICU cross-build for ${goos}/${goarch}"
echo "    host=${triplet} cc=${cc} ar=${AR_BIN} ranlib=${RANLIB_BIN}"
cp -r /tmp/icu-build/src "$builddir"
cd "$builddir/source"
PATH="${path_extra:+${path_extra}:}${PATH}" \
    CC="$cc" CXX="$cxx" AR="$AR_BIN" RANLIB="$RANLIB_BIN" \
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
make install
rm -rf "$builddir"
echo "=== ${goos}/${goarch} installed at ${prefix} ==="
