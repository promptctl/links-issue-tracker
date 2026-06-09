# Changelog

All notable changes to `links-issue-tracker` (the `lit` CLI) are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

### Security
