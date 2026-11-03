#!/usr/bin/env bash
# cross-version-check.sh — proves a published release still works on a store
# this tree stepped down to it (design-docs/schema-reversibility.md, "How it is
# proven"). The round-trip gate proves our own Down SQL against our own Up; this
# is the one check that runs the code that actually shipped.
#
#   1. Build this tree as a release: Origin=release, so it may raise a remote,
#      and ReleasedSchema=carried, so its released ceiling is its own registry
#      max. The stamps live here and nowhere else, so every caller proves the
#      same binary. [LAW:one-source-of-truth] Only goreleaser otherwise claims
#      release provenance; this binary lives in a temporary directory removed
#      on exit, so no one ever runs it as an installed lit.
#   2. Pick the target: the newest published release whose registry max is
#      below this tree's. "The previous release" alone can carry the same max as
#      the tree, which would step down nothing and leave a Down fixed in place
#      unproven; the newest release below steps across every migration the tree
#      adds, and always across the newest Down.
#   3. Fill a workspace with a row in every table a Down reads, push it to a
#      local remote, export it, and `lit downgrade --schema --remote` it to the
#      target.
#   4. Run the release against it, in the stepped store and in a fresh clone of
#      the remote: read, write, sync.
#   5. Lift the hold and raise the store with this tree, export again, and
#      compare: the export must equal the first one plus exactly the ticket the
#      release wrote.
#
# Any failure exits non-zero. Needs git, go (and the cgo flags the engine
# needs), gh (authenticated; downloads the release), jq, and the repository's
# release tags (a full-history checkout).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/release-tags.sh
. "$root/scripts/release-tags.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() {
    echo "::error::cross-version-check: $*" >&2
    exit 1
}

# registry_max prints the highest migration version in a registry listing:
# one path per line, `.../000NN_<name>.sql`.
registry_max() {
    sed -n 's|.*/0*\([0-9][0-9]*\)_[^/]*\.sql$|\1|p' | sort -n | tail -1
}

# --- 1. the candidate -------------------------------------------------------
pkg=github.com/promptctl/links-issue-tracker/internal/version
candidate="$work/candidate/lit"
(cd "$root" && go build -ldflags "-X $pkg.Origin=release -X $pkg.ReleasedSchema=carried" -o "$candidate" ./cmd/lit)
tree_max="$(cd "$root" && git ls-files 'internal/store/migrations/*.sql' | registry_max)"
[ -n "$tree_max" ] || fail "no migrations found in internal/store/migrations"

# --- 2. the target release --------------------------------------------------
tag=""
target=""
while read -r candidate_tag; do
    max="$(cd "$root" && git ls-tree --name-only "$candidate_tag" internal/store/migrations/ | registry_max)"
    if [ -n "$max" ] && [ "$max" -lt "$tree_max" ]; then
        tag="$candidate_tag"
        target="$max"
        break
    fi
done < <(cd "$root" && git tag --list | { grep -E "$RELEASE_TAG_ERE" || [ $? -eq 1 ]; } | sort -rV)
[ -n "$tag" ] || fail "no release tag carries a schema below this tree's v$tree_max; fetch the tags (a full-history checkout)"
echo "cross-version-check: tree schema v$tree_max, stepping down to $tag at v$target"

case "$(uname -s)_$(uname -m)" in
    Linux_x86_64) platform=linux_amd64 ;;
    Linux_aarch64) platform=linux_arm64 ;;
    Darwin_arm64) platform=darwin_arm64 ;;
    Darwin_x86_64) platform=darwin_amd64 ;;
    *) fail "no release archive for $(uname -s) $(uname -m)" ;;
esac
archive="lit_${tag#v}_${platform}.tar.gz"
mkdir -p "$work/release"
gh release download "$tag" --repo promptctl/links-issue-tracker --dir "$work/release" --pattern "$archive" --pattern checksums.txt
(cd "$work/release" && grep " $archive\$" checksums.txt | shasum -a 256 -c -)
tar -xzf "$work/release/$archive" -C "$work/release" lit
previous="$work/release/lit"

# --- 3. a filled workspace, stepped down ------------------------------------
# Every command names its actor with --by, so the session's identity, if any,
# never reaches the fixture.
unset CLAUDE_CODE_SESSION_ID
git init -q --bare "$work/remote.git"
git clone -q "$work/remote.git" "$work/producer" 2>/dev/null
cd "$work/producer"
git config user.email ci@example.invalid
git config user.name cross-version-check
echo seed > readme.md
git add readme.md
git commit -qm seed
git push -q origin HEAD
"$candidate" init --skip-hooks

first_word() { awk '{print $1; exit}'; }
epic="$("$candidate" new --type epic --title epic --topic demo --by ci | first_word)"
other_epic="$("$candidate" new --type epic --title other-epic --topic demo --by ci | first_word)"
planned="$("$candidate" new --title planned --topic demo --parent "$epic" --lane a --labels x,y --plan "the plan" --by ci | first_word)"
open="$("$candidate" new --title open --topic demo --parent "$epic" --lane b --by ci | first_word)"
duplicate="$("$candidate" new --title duplicate --topic demo --parent "$epic" --by ci | first_word)"
blocked="$("$candidate" new --title blocked --topic demo --parent "$other_epic" --by ci | first_word)"
digest="$("$candidate" show "$planned" | sed -n 's/.*digest \([0-9a-f]*\)).*/\1/p')"
[ -n "$digest" ] || fail "could not read the plan digest of $planned"
# Their stdout is guidance for an agent; a failure still prints on stderr.
{
    "$candidate" dep add "$blocked" --blocked-by "$planned" --by ci
    "$candidate" dep add "$duplicate" --related-to "$open" --by ci
    "$candidate" comment add "$planned" --body "a comment" --by ci
    "$candidate" verdict "$planned" pass --plan "$digest" --law one-source-of-truth --by ci
    "$candidate" start "$planned" --by ci
    "$candidate" done "$planned" --by ci
    "$candidate" close "$duplicate" --resolution duplicate --of "$open" --by ci
} >/dev/null
"$candidate" sync push --set-upstream
"$candidate" export > "$work/before.json"
"$candidate" downgrade --schema "$target" --remote origin

# --- 4. the release reads, writes and syncs ---------------------------------
# The release predates --by; with no session id it records its default actor.
"$previous" ls
"$previous" show "$open"
written="$("$previous" new --title written-by-the-release --topic demo --parent "$epic" | first_word)"
[ -n "$written" ] || fail "$tag created no ticket"
"$previous" sync push
git clone -q "$work/remote.git" "$work/consumer"
(cd "$work/consumer" && "$previous" init --skip-hooks && "$previous" show "$written")

# --- 5. back up, and compare ------------------------------------------------
"$candidate" upgrade --remote origin
"$candidate" sync push
"$candidate" export > "$work/after.json"

# The release's write is removed from the second export by its id; what is left
# must equal the first export. exported_at is when, not what.
normalize='del(.exported_at)
  | .issues |= map(select(.id != $w))
  | .events |= map(select(.issue_id != $w))
  | .comments |= map(select(.issue_id != $w))
  | .labels |= map(select(.issue_id != $w))
  | .relations |= map(select(.src_id != $w and .dst_id != $w))'
jq -e --arg w "$written" '.issues | any(.id == $w)' "$work/after.json" >/dev/null ||
    fail "the ticket $tag wrote ($written) is missing after the raise"
if ! diff -u <(jq -S --arg w "$written" "$normalize" "$work/before.json") \
    <(jq -S --arg w "$written" "$normalize" "$work/after.json"); then
    fail "stepping down to $tag and back up changed the export (diff above)"
fi
echo "cross-version-check: $tag reads, writes and syncs a store stepped down from v$tree_max to v$target, and the raise back loses nothing"
