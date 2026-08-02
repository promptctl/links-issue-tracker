# Changelog

All notable changes to `links-issue-tracker` (the `lit` CLI) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The release is now hard-gated on the license posture: a dedicated `license-gate` CI job runs the policy check over every component compiled into the release binary, and the publish job depends on it. If any dependency (Go module or native library) carries a non-allowlisted license, the release aborts before any tag, archive, or asset is published — a non-free build cannot be released, even from a branch that skipped the PR-time check.
- The SBOM, `THIRD_PARTY_LICENSES` bundle, license report, and license-policy gate now also cover the statically-linked native C libraries that cgo compiles into release binaries but no go.mod tool can see: ICU 75.1 (Unicode-3.0), zstd 1.5.6 (BSD-3-Clause), musl 1.2.5 (MIT), and compiler-rt via zig 0.14.0 (MIT). Each appears with its name, version, license, `pkg:generic` PURL (SBOM), and verbatim notice text (bundle). Versions are pinned to the release build config, with a CI check tying ICU/zig to `build/Dockerfile.release`.
- CI license-policy gate: every linked module's license is checked against a committed allowlist (`tools/licenses/policy.json`) of permissive licenses plus documented per-module exceptions, and the build fails if a dependency bump pulls in a non-allowlisted license (GPL/AGPL/SSPL/BUSL, etc.). Runs on every pull request and master push via `go test` (and standalone via `go run ./tools/licenses -check`). Shares the classifier with the license report/SBOM, so the gate checks the exact licenses those artifacts document.
- Every release now ships a CycloneDX SBOM (`lit_<version>_sbom.cdx.json`) as a standalone downloadable asset: a machine-readable bill of materials listing every Go module and statically-linked native C library compiled into the binary, with its version, package URL, and resolved license. It is generated from the same linked-module inventory as `THIRD_PARTY_LICENSES`/`LICENSE-REPORT.md`, validated as CycloneDX 1.6 in CI, and lets vulnerability scanners audit a given `lit` version against CVE feeds.
- CI gate (`TestReleasedMigrationsAreContentPinned`) that refuses any change reusing a released migration version number under different content — the mechanism that bricked workspaces in the migrate-drift epic. Every non-baseline migration's content is pinned by sha256; reusing, editing, deleting, or leaving a version number unpinned fails the build with a message naming the collision.

### Changed

### Fixed

- Sync reconciliation of branches with unrelated histories no longer depends on a silent error-swallowing bug in the embedded Dolt driver. The driver's first-row pre-read discarded any error, so a query that failed on its first row (such as `DOLT_MERGE_BASE` raising "no common ancestor" for refs that share no history) could surface as an empty result set instead of the real error. The driver now propagates that error, and merge-base resolution recognizes the "no common ancestor" backend error as the unrelated-histories state directly — the same domain outcome it already handled via an empty result set.

### Removed

- The `ready` and `queue` workable views are retired from the command surface, leaving `next` (the single leaf to start) and `backlog` (the full ranked queue with blocked items shown inline) as the only named workable views — `ls` remains the power query and `orphaned` its own staleness view. `lit ready` and `lit queue` no longer appear in `lit --help` or shell completion, and now fail with a documented pointer to `lit backlog` / `lit next` instead of running: the break is deliberate and explained, never silent. The retired presentations (ready's blocked-to-bottom re-sort and coaching preamble; queue's terse pullable-only list) are dropped — `backlog` and `next` carry the surviving intent.

### Security

## [0.2.1] - 2026-08-01

### Changed

- Rewrote lit's agent-facing prompt surfaces (the CLAUDE.md integration block `lit init` writes, and the `lit quickstart` guidance) into calm, plain, second-person tool documentation. The previous text used the fingerprint of a prompt-injection attack — ALL-CAPS imperatives, threats, and act-behind-the-user framing — which led a security-conscious agent to flag lit as untrustworthy and advise uninstalling it. The agent-native workflow is unchanged; only the register is.

## [0.2.0] - 2026-07-30

### Added

- In-repo issue tracking backed by an embedded [Dolt](https://www.dolthub.com/) database — the backlog ships with the code and every mutation is a versioned commit.
- Agent-first core loop: `lit ready`, `lit start`, `lit done`, and `lit followup` return guidance alongside data so a coding agent can drive the workflow unattended.
- `lit quickstart` prints the live command reference for agents.
- One write boundary and one identity per clone, keyed off `git rev-parse --git-common-dir`, so all worktrees of a clone share a single issue view.
- `lit version` command with build-time `-ldflags` version stamping.
- `lit doctor [--fix]` for diagnosing and repairing workspace issues.
- `lit lifeboat` recovery path for schema-ahead workspaces.
- `scripts/install.sh` builds `lit` from source and installs it onto `PATH`, warning about stale shadowing binaries.

### Changed

- `lit new` and `lit followup` now rank new tickets to the **top** of the order by default (fresh work surfaces first) instead of the bottom. Pass `--bottom` to append at the bottom — use it when authoring a batch in order so creation order is preserved. Importing an existing backlog still preserves its order.

### Fixed

### Removed

- The `--json` flag is removed from every command. The default human-readable text is the one canonical surface — it is already the agent interface, and per-line output is parseable when a script needs one field (e.g. `lit workspace | sed -n 's/^traces_dir: //p'`). `lit export` and `lit lifeboat dump` are unchanged: they emit a JSON data structure as their sole output (no flag), because a full export / raw dump has no text form. Passing `--json` now fails with a usage error.

### Security
