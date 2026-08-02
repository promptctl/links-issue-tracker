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

## One PR per ticket; one release per epic

Each ticket lands as its **own** reviewable PR. In a ticket PR, do **not** promote
the release version — just add your `CHANGELOG.md` entries under the existing
`## [Unreleased]` heading; merging a ticket PR cuts **no** release. A release is
cut only when an **epic** is finished, by a dedicated `chore(release)` PR that
promotes the changelog.

For the promotion steps, the CI mechanism, and the versioning policy, see
[RELEASING.md](RELEASING.md) — the single home for the release procedure.
