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

## Cutting a release

Prerequisites:
- All work for the release is on `master` and CI is green.
- You have `git tag` and `git push` rights to the repo.

```bash
# 1. Decide the version (semver — patch increment is fine for early shakeout).
TAG=v0.1.0

# 2. Tag from master.
git checkout master
git pull --rebase
git tag -a "$TAG" -m "lit $TAG"
git push origin "$TAG"

# 3. Watch .github/workflows/release.yml run.
gh run watch
```

When the workflow finishes, the GitHub Release is published with all artifacts
above. No manual steps.

### How the pipeline is verified

Two tiers, split by cost so the per-PR loop stays fast:

- **Per PR (fast, ~2-3 min):** `.github/workflows/release-smoke.yml` builds the
  native `linux/amd64` target with `goreleaser build --single-target --snapshot`.
  It proves the things that break per code change — the code compiles, the
  cgo + ICU link works, and `.goreleaser.yml` parses — without the cross-build.
  The full ~35-minute 5-platform build is deliberately NOT on the PR path.
- **Out-of-band (full):** `.github/workflows/release-validate.yml` builds the
  release-builder image and runs the SAME goreleaser invocation release.yml uses,
  producing a real cross-platform `dist/`, running `mkmanifest`, and asserting
  the manifest has every expected platform with a valid SHA256. It runs on every
  push to `master` (catching a broken pipeline before any tag is cut) and on
  demand via `workflow_dispatch` — never on `pull_request`.

If `release-validate` is green on `master`, the next `git push <tag>` will
produce a working GitHub Release. That workflow also uploads `dist/` as a
workflow artifact on every run, so you can inspect what would be published
without re-running it.

### Dry-run a release locally (optional)

Local dry-runs require a container runtime + the custom release-builder
image. The image starts from `golang:1.25.7-bookworm` and installs zig 0.14.0
as the single cross-compiler for every non-native target (a pinned macOS SDK
supplies the Apple frameworks zig omits, used link-only), with ICU built from
source per target; linux/amd64 uses the system libicu-dev directly.

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
# Mirrors the release.yml step exactly so the dry-run matches CI.
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

### Dry-run via the release workflow

The release workflow exposes `workflow_dispatch` for re-running the full
pipeline against the current commit. Trigger it from the GitHub Actions UI;
it runs the same cross-compile path in --snapshot mode and uploads `dist/`
as a 7-day workflow artifact. No release is published.

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
