> **About this file.** This repository builds `lit` (the links issue tracker)
> and dogfoods it. The marked block below is **exactly what `lit init` writes
> into any repository you set `lit` up in** — a live example of the tool's
> output, not maintainer instructions aimed at you. Agents doing ticketed work
> *in this repo* do follow it.
>
> **Here to contribute to `lit` itself?** You don't need to start with the block
> below — read **[CONTRIBUTING.md](CONTRIBUTING.md)** (and
> **[docs/agent-setup.md](docs/agent-setup.md)** if you're pointing an agent at
> the repo).
>
> *Everything between the `BEGIN/END LIT INTEGRATION` markers is generated and
> refreshed by `lit init`; to change it, edit
> [`internal/templates/defaults/agents-section.md`](internal/templates/defaults/agents-section.md).*

<!-- BEGIN LIT INTEGRATION -->
## lit Agent-Native Workflow

This repository uses `lit` for agent-native issue tracking.

Start by running `lit quickstart` to load the workflow instructions. It prints how tickets are found, created, updated, and closed here, so running it first means the rest of your work follows the conventions this repo expects. It's a quick, read-only command — no need to check in before running it.

<!-- END LIT INTEGRATION -->

## Releases are cut on merge

To release a feature/fix, bump `CHANGELOG.md` in that PR: rename `## [Unreleased]`
to `## [0.2.0] - <date>` (`scripts/next-version.sh <minor|patch>` gives the tag
`v0.2.0`; the heading drops the `v`) and add a fresh empty `## [Unreleased]` above
it. Merging is the whole release — CI builds, validates, tags, and publishes
`v0.2.0`. No local tag push. One merged feature/fix PR, one release.
