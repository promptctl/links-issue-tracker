# Releasing lit

This document describes how a tagged release is cut and what the published
artifacts look like. It is the operator's guide; the architectural reasoning
lives in the `links-downgrade-t244` epic.

## What a release publishes

Each tagged release (`vX.Y.Z`) creates a GitHub Release containing:

| Asset                                      | Purpose                                                                                       |
|--------------------------------------------|-----------------------------------------------------------------------------------------------|
| `lit_vX.Y.Z_<goos>_<goarch>.tar.gz` / `.zip` | Per-platform binary archive. The single file inside is `lit`.                                |
| `checksums.txt`                            | SHA256 of every archive above (`<sha256>  <filename>` per line).                              |
| `release-manifest.json`                    | Machine-readable index linking the version → its schema-support range → per-platform artifacts. |

The manifest schema is the Go type `release.Manifest` in
`internal/release/manifest.go`. Producer (`tools/mkmanifest`) and consumers
(`lit version`'s embedded view, future `lit downgrade`) share that one type;
the JSON on disk and the type in code cannot drift.

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

### How the pipeline is verified before a tag is ever cut

`.github/workflows/release-validate.yml` runs on every PR and on every push
to `master`. It executes the SAME goreleaser-cross container release.yml uses,
produces a real cross-platform `dist/`, runs `mkmanifest` against it, and
asserts the manifest has every expected platform with a valid SHA256.

If `release-validate` is green, the next `git push <tag>` will produce a
working GitHub Release. If it's red, the PR doesn't merge. This is the
single answer to "did the pipeline survive my change."

The workflow also uploads `dist/` as a 7-day workflow artifact on every run,
so a reviewer can inspect what would be published without re-running the
workflow.

### Dry-run a release locally (optional)

Local dry-runs require a container runtime — the project uses
`ghcr.io/goreleaser/goreleaser-cross:v2.16.0` to provide the CGO
cross-compilers (osxcross for darwin, mingw for windows, glibc for linux).

```bash
podman run --rm -v "$PWD":/go/src/app -w /go/src/app \
  ghcr.io/goreleaser/goreleaser-cross:v2.16.0 \
  release --snapshot --clean
# Inspect ./dist/ — archives, checksums.txt; mkmanifest then writes release-manifest.json.
```

Note: cross-compilation requires the goreleaser-cross image; a host without
it (or without osxcross etc. installed natively) cannot build darwin
artifacts. The CI workflow always has the right environment; local dry-run
is convenience, not required.

### Dry-run via the release workflow

The release workflow exposes `workflow_dispatch` for re-running the full
pipeline against the current commit. Trigger it from the GitHub Actions UI;
it runs the same cross-compile path in --snapshot mode and uploads `dist/`
as a 7-day workflow artifact. No release is published.

## What `lit version` reports

After a tagged build, the binary's `lit version` reports the tag, short SHA,
and build timestamp — injected by goreleaser via `-ldflags -X`:

```
$ lit version
lit v0.1.0 (commit abcdef0, built 2026-05-24T15:21:00Z)
schema versions supported: 1–1

$ lit version --json
{
  "version": "v0.1.0",
  "commit": "abcdef0",
  "date": "2026-05-24T15:21:00Z",
  "is_dev": false,
  "schema_support": { "min": 1, "max": 1 }
}
```

For source builds, `scripts/install.sh` derives `version` from
`git describe --tags --always --dirty` and `commit` from `git rev-parse
--short HEAD`, so even ad-hoc checkouts report meaningful identity.

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
- **Pre-release / nightly channel.** Not configured. The workflow only fires
  on `v*.*.*` tags; introducing a `v*-rc*` channel requires extending the
  `tags` filter and the changelog/release config.
