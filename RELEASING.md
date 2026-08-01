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

## Cutting a release

A release is cut entirely by CI when a release-promotion merges to `master`. The
only manual step is in the PR:

1. **In the PR:** rename `## [Unreleased]` in [`CHANGELOG.md`](CHANGELOG.md) to
   `## [<version>] - <YYYY-MM-DD>` (`<version>` = `scripts/next-version.sh
   <minor|patch>` without the leading `v`) and add a fresh empty `## [Unreleased]`
   above it. Commit it with the work.
2. **Merge.** The master build
   ([`release-validate.yml`](.github/workflows/release-validate.yml)) sees the
   newest `CHANGELOG` version has no tag yet, builds + validates the real
   cross-platform artifact, then cuts the tag at that commit and publishes the
   release — all in one run. Watch it with `gh run watch`. No local tag push.

Docs/chore/refactor-only work cuts no release — leave `## [Unreleased]` as-is.

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

Release-mode downloads the per-platform archive, fetches `checksums.txt`,
verifies SHA256, extracts, and atomically renames into place. Same
target-directory resolution and stale-binary detector across all modes.

## Open follow-ups

These are out of scope for this ticket and tracked elsewhere or deferred to
follow-ups:

- **Signing.** `release.Signature` is reserved in the manifest schema; adding
  cosign/minisign verification later does not change the manifest format —
  unsigned manifests omit the `signature` field; signed ones populate it.
- **Pre-release / nightly channel.** Not configured. The workflow's
  `tags: v*.*.*` filter is a glob that also matches `v0.1.0-rc1`, so the
  release job carries a `!contains(github.ref_name, '-')` guard that
  rejects prerelease tags at the boundary. Introducing a prerelease
  channel requires relaxing that guard alongside any changelog/release
  config changes.
