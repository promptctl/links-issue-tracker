# shellcheck shell=bash
# release-tags.sh — the one definition of a release tag in this repository.
# Source it, then select the newest release from any list of tag names:
#
#     source scripts/release-tags.sh
#     newest="$(git tag --list | newest_release_tag)"
#
# A release tag is exactly `vX.Y.Z`, the shape release-validate.yml cuts. origin
# also carries `v0.2.0.1`-shaped tags; those are not releases, and matching the
# shape exactly is what drops them. [LAW:one-source-of-truth] changelog-frozen.sh,
# next-version.sh, and install.sh's source mode all select through this file.

if [ "${BASH_SOURCE[0]:-}" = "${0}" ]; then
    echo "release-tags.sh is meant to be sourced: 'source scripts/release-tags.sh'" >&2
    exit 64
fi

RELEASE_TAG_ERE='^v[0-9]+\.[0-9]+\.[0-9]+$'

# newest_release_tag reads tag names on stdin, one per line, and prints the
# newest release tag by version order, or nothing when none is a release. grep's
# exit 1 is its "no lines matched", the real state of a list with no release;
# every other grep status still fails the pipeline. [LAW:no-silent-failure]
newest_release_tag() {
    { grep -E "$RELEASE_TAG_ERE" || [ $? -eq 1 ]; } | sort -V | tail -1
}
