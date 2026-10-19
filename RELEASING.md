# Releasing lit

This document describes how a tagged release is cut and what the published
artifacts look like. It is the operator's guide; the architectural reasoning
lives in the `links-downgrade-t244` epic.

## What a release publishes

Each tagged release (`vX.Y.Z`) creates a GitHub Release containing:

| Asset                                      | Purpose                                                                                       |
|--------------------------------------------|-----------------------------------------------------------------------------------------------|
| `lit_X.Y.Z_<goos>_<goarch>.<ext>` | Per-platform binary archive — `.tar.gz` for linux/darwin, `.zip` for windows (no leading `v` in version segment — goreleaser strips it). |
| `checksums.txt`                            | SHA256 of every archive above (`<sha256>  <filename>` per line).                              |
| `release-manifest.json`                    | Machine-readable index linking the version → its schema-support range → per-platform artifacts. |

The manifest schema is the Go type `release.Manifest` in
`internal/release/manifest.go`. The producer (`tools/mkmanifest`) emits
JSON conforming to that type; future consumers (the `lit downgrade`
command landing in `.4`) decode it back into the same type, so the JSON
on disk and the type in code cannot drift. (`lit version` reports
`version.Info` only — it does not currently embed the full manifest;
embedding can be added later via `go:embed` without changing the schema.)

## Versioning policy

Semver, with two deviations: **major is frozen** (never bumped), **minor** = a
feature or any presumed-breaking change, **patch** = a pure bugfix.
`scripts/next-version.sh <minor|patch>` computes the next tag under this policy
(`v0.1.0` → `v0.2.0` or `v0.1.1`).

## The Claude Code plugin has no version

Neither `claude-plugin/.claude-plugin/plugin.json` nor the plugin's entry in
`.claude-plugin/marketplace.json` sets `version`. Claude Code then versions the
plugin by commit SHA, so a user who has installed it gets every merged change at
their next plugin update. A change to the plugin needs no bump, and a lit
release does not touch it.

Do not add `version` to either file. Claude Code reads a set `version` before
the commit SHA and skips the update while that string is unchanged, so a pinned
version keeps every installed user on old files until someone remembers to
change it
([how Claude Code computes the version](https://code.claude.com/docs/en/plugins/loading#how-claude-code-computes-the-version)).

## Cutting a release

A release is cut entirely by CI when a release PR merges to `master`. Releases
ship **once per epic**, not per ticket: a ticket PR never touches `CHANGELOG.md`
and cuts nothing on merge. When an epic's tickets are all merged, cut the release
with a dedicated `chore(release)` PR that writes the release's changelog section
and flips the design-doc statuses the release makes true, and changes nothing
else.

### The changelog is written once, whole, when the release is cut

These are the owner's rules, and they hold for every release:

1. **No changelog text before the release.** No ticket PR writes any part of
   `CHANGELOG.md`: no entry, no note for later, no `## [Unreleased]` section.
   The changelog is written only in the `chore(release)` PR.
2. **Write the release's section whole.** One author writes the entire section
   in one pass, from what merged since the last tag. It is not assembled from
   per-ticket notes and does not start from an earlier draft.
3. **State the new behavior, and only that.** Each entry says what lit does in
   this release. It never describes the old behavior: no "used to", no
   "previously", no "no longer", no before-and-after.
4. **Never audit the changelog.** No sentence-by-sentence check, no verifying
   claims against the previous tag or against `master`, no review pass over the
   notes, in the release PR or anywhere else.
5. **One optional rewrite, by a subagent, accepted as written.** If you don't
   like the section you wrote, you may have a subagent write it instead, once.
   Give it the version heading line to use. Whatever it returns goes into
   `CHANGELOG.md` unchanged, however bad or good: you don't edit it, fix it,
   audit it, or ask again.

Rule 3 in practice:

- WRONG: "`lit import` now refuses a `blocked_by` between two issues in one
  epic. It used to write the edge."
- RIGHT: "`lit import` refuses a `blocked_by` between two issues in one epic."

The rules exist because writing an entry per ticket and then auditing those
entries took more time than writing the code they described. The code is worth
the time; the changelog is not. You will see a sentence in the notes that is
wrong or vague and think *"released notes are permanent, so checking this one is
the responsible thing."* That check is the audit rule 4 forbids. The section
ships as written.

### Steps

1. **In the `chore(release)` PR:** add `## [<version>] - <YYYY-MM-DD>` to
   [`CHANGELOG.md`](CHANGELOG.md) above the newest released section
   (`<version>` = `scripts/next-version.sh <minor|patch>` without the leading
   `v`), and write the release's section under it by the rules above. Then flip
   every `built (unreleased)` status *value* under
   [`design-docs/`](design-docs/) to `built (v<version>)`: the bare occurrences
   in a status line or a state table, never the backticked one in design.md's
   convention paragraph, which defines the form and outlives every release. A
   grep for the phrase returns both, and flipping the definition turns it into
   gibberish. The version becomes a fact at this commit and nowhere earlier, so
   this is the one place the two can be written down together, and it is why a
   design doc never names the release it *expects* to ship in.
2. **Merge.** The master build
   ([`release-validate.yml`](.github/workflows/release-validate.yml)) sees the
   newest `CHANGELOG` version has no tag yet, builds + validates the real
   cross-platform artifact, then cuts the tag at that commit and publishes the
   release — all in one run. Watch it with `gh run watch`. No local tag push.

No ticket PR, whatever its type, cuts a release or writes to `CHANGELOG.md`.
Only the dedicated `chore(release)` PR above, merged at the end of an epic, does
either.

### A tagged section never changes

Once a version is tagged its `CHANGELOG` section is the record of what that
release contained, and it is immutable from then on.
[`scripts/changelog-frozen.sh`](scripts/changelog-frozen.sh) enforces that in
CI's `verify` job on every PR and master push: everything from the newest tag's
own heading down to end-of-file must be byte-identical to that region in the tag
itself.

A released section is never corrected, reworded, or brought up to date, even
where it is wrong: that is an audit after the fact. If the guard fires, the PR
edits a released section; revert that edit.

### How the pipeline is verified

Two tiers, split by cost so the per-PR loop stays fast:

- **Per PR (fast):** `.github/workflows/release-smoke.yml` builds the
  `linux/amd64` target with `goreleaser build --single-target --snapshot --clean`
  inside the release image's `smoke` stage (toolchain + linux/amd64 ICU only,
  layer-cached). It proves the things that break per code change — the code
  compiles, the cgo + ICU link works, `.goreleaser.yml` parses, and the
  resulting binary is fully static and executes on both glibc (the runner) and
  musl (an Alpine container). The full ~35-minute 5-platform build is
  deliberately NOT on the PR path.
- **Out-of-band (full):** `.github/workflows/release-validate.yml` builds the
  release-builder image and runs the goreleaser cross-build, producing a real
  cross-platform `dist/`, running `mkmanifest`, and asserting the manifest has
  every expected platform with a valid SHA256 — then executing both linux
  binaries (amd64 + arm64, the latter under qemu) on stock Alpine containers and
  the glibc runner, proving the static-musl universality claim by running, not
  just linking. It runs on every push to `master` and on demand via
  `workflow_dispatch` — never on `pull_request`.

  **This run is also the one and only build of a release.** When the newest
  `CHANGELOG` version has no tag yet — a release is pending on this commit — the
  `validate` job stamps that real version (via an ephemeral local tag, not
  `--snapshot`), and its downstream `publish` job, in the SAME run, cuts the tag
  at that commit and publishes the validated `dist/`. Build once, tag + publish
  from CI — what ships is exactly what was validated, and nothing rebuilds. An
  ordinary master push just runs the snapshot proof and publishes nothing.

If the run is red, no release is cut — fix forward, and the next master push (or
a `workflow_dispatch` re-run) picks the pending version back up. A published tag
also short-circuits it: once `v<version>` exists, the pending-release check is
false, so re-runs never double-publish.

That state-based gate is also what makes the master concurrency queue safe:
GitHub keeps only one *pending* `release-validate` run per ref — each newly
queued run cancels the previously queued one (`cancel-in-progress: false`
protects only the run already executing) — so a burst of quick merges evicts
every intermediate run, a pending promotion's included, and only the newest
survives. The surviving run re-derives the pending version from repository
state and publishes it (measured 2026-08-27: the v0.9.0 promotion run was
evicted from the queue, and the burst's last run published v0.9.0 unaided).
The residual failure — the surviving run goes red or is cancelled and no later
push arrives — is caught by
[`release-watchdog.yml`](.github/workflows/release-watchdog.yml): an hourly
check that goes red and files (or extends) an urgent `release-stuck` lit ticket whenever a
promoted version has had no tag for over two hours. Its recovery path is the
`workflow_dispatch` re-run below.

### Dry-run a release locally (optional)

Local dry-runs require a container runtime + the custom release-builder
image. The image starts from `golang:1.25.7-bookworm` and installs zig 0.14.0
as the single cross-compiler for every target (a pinned macOS SDK supplies
the Apple frameworks zig omits, used link-only), with ICU built from source
per target. The linux targets are musl and fully static — one
interpreter-free binary per arch that runs on both glibc distros and
musl/Alpine containers.

Build the image once, then use it:

```bash
# Build the release-builder image
podman build -f build/Dockerfile.release -t lit-release-builder:local .

# Run goreleaser in --snapshot mode (no publish)
podman run --rm -v "$PWD":/go/src/app -w /go/src/app \
  lit-release-builder:local \
  release --snapshot --clean

# Then run mkmanifest against dist/ to produce release-manifest.json.
# `tag` (v-prefixed) and `version` (v-stripped) are BOTH required —
# tag becomes the URL path segment, version goes into archive filenames.
# Mirrors the CI manifest step exactly so the dry-run matches CI.
VERSION=$(jq -r .version dist/metadata.json)
TAG=$(jq -r .tag dist/metadata.json)
COMMIT=$(jq -r .commit dist/metadata.json | cut -c1-7)
DATE=$(jq -r .date dist/metadata.json)
go run ./tools/mkmanifest \
  -version "$VERSION" \
  -tag "$TAG" \
  -commit "$COMMIT" \
  -date "$DATE" \
  -dist ./dist \
  -base-url https://github.com/promptctl/links-issue-tracker/releases/download \
  -out ./dist/release-manifest.json

# Inspect ./dist/
```

The first image build takes ~15 minutes (ICU is built from source per
target). Subsequent builds reuse layer cache. CI uses GitHub Actions cache
across runs for the same speedup.

### Re-running the pipeline on demand

`release-validate.yml` exposes `workflow_dispatch` to re-run the full build +
validation against the current commit. Note it is **not a dry-run**: if a release
is pending on that commit (the newest `CHANGELOG` version has no tag yet), the
dispatch run will cut the tag and publish, exactly as a master push would. It is
the recovery path when an automatic run was cancelled or failed — not a way to
rehearse without publishing. To inspect what a build produces without any
publish, read the `release-validate-dist-<sha>` artifact any run uploads.

## What `lit version` reports

After a tagged build, the binary's `lit version` reports the version
(goreleaser's `.Version` — the tag with the leading `v` stripped), the
short SHA, and the build timestamp — injected by goreleaser via
`-ldflags -X`:

```
$ lit version
lit 0.1.0 (commit abcdef0, built 2026-05-24T15:21:00Z)
schema versions supported: 1–1
```

The reported `version` is goreleaser's `.Version` template — the tag with the
leading `v` STRIPPED (vX.Y.Z → X.Y.Z). The same stripped string is used in
the archive filenames and the manifest `version` field, so `lit version`,
the archive name, and the manifest agree byte-for-byte. Same convention as
kubectl / helm / terraform.

For source builds, `scripts/install.sh` derives `version` from
`git describe --tags --always --dirty` (with any leading `v` stripped so
the value matches goreleaser's v-stripped convention) and `commit` from
`git rev-parse --short HEAD`, so source builds and release builds report
`Version` in one canonical format and ad-hoc checkouts still carry
meaningful identity.

For builds without ldflag stamping (plain `go build ./cmd/lit`),
`lit version` reports `lit dev (commit unknown, built unknown)`.

## How `scripts/install.sh` consumes a release

The same installer covers three sources:

```bash
# (default) build from this checkout, ldflag-stamped from git
bash scripts/install.sh

# install a specific tagged release for the current platform
bash scripts/install.sh --from-release v0.1.0

# install the most recent published release
bash scripts/install.sh --latest-release
```

Release-mode reads the release's `release-manifest.json`, downloads the
archive it lists for the current platform, verifies the SHA256 it lists, and
extracts. Every mode stages the binary in a temp dir
beside the target, runs its `lit version`, and only then atomically renames
it into place, so a binary that cannot run never replaces the installed one.
Same target-directory resolution and stale-binary detector across all modes.

## Open follow-ups

These are out of scope for this ticket and tracked elsewhere or deferred to
follow-ups:

- **Signing.** `release.Signature` is reserved in the manifest schema; adding
  cosign/minisign verification later does not change the manifest format —
  unsigned manifests omit the `signature` field; signed ones populate it.
  The change that adds signing adds `signature` to the key set
  `release-validate.yml`'s "Assert manifest shape" step expects.
- **Pre-release / nightly channel.** Not configured. The workflow's
  `tags: v*.*.*` filter is a glob that also matches `v0.1.0-rc1`, so the
  release job carries a `!contains(github.ref_name, '-')` guard that
  rejects prerelease tags at the boundary. Introducing a prerelease
  channel requires relaxing that guard alongside any changelog/release
  config changes.
