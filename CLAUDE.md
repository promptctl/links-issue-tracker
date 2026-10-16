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

As you work a ticket, record its phase at each move: `lit phase <id> --to impl` when you start writing code, `lit phase <id> --to review` when its PR opens, `lit phase <id> --to merge` when review is done and the merge is under way. `lit start` puts it in `plan`, and `lit done <id>` ends it.

<!-- END LIT INTEGRATION -->

## One PR per ticket; one release per epic

Each ticket lands as its **own** reviewable PR, and merging a ticket PR cuts
**no** release. A release is cut only when an **epic** is finished, by a
dedicated `chore(release)` PR.

## The changelog is written once, at release

The changelog is a receipt printed at checkout, not a tab kept open through the
cycle. These are the owner's rules:

- **A ticket PR never touches `CHANGELOG.md`.** No entry, no note for later, no
  `## [Unreleased]` section. No part of the changelog is written until a release
  is cut.
- **The release PR writes the release's section whole**, in one pass, and states
  the **new behavior only**. It never describes old behavior.
  - WRONG: "`lit sync` now exits 3 when the remote's schema is newer; it used to
    exit 5."
  - RIGHT: "`lit sync` exits 3 when the remote's schema is newer."
- **Nobody audits the changelog, ever.** No sentence-by-sentence check, no
  verifying its claims against the last tag, no review pass over the notes.
- **One escape hatch.** If you don't like the section you wrote, you may have a
  subagent write it, once. Commit what it returns exactly as returned, however
  bad or good.

Changelog upkeep was costing more time than the code it described. The code is
worth the time; the changelog is not. The rules will be tested at three moments:

- You finish a ticket and think *"the release author will need to know about
  this, so I'll leave a line under Unreleased."* Don't. The release author
  writes from what merged.
- You are cutting a release, spot a sentence that is wrong for someone
  upgrading, and think *"released notes are permanent; checking every claim is
  the responsible thing."* That check is the audit this rule bans. The notes
  ship as written.
- The subagent's section has one sentence you could fix in ten seconds. Fixing
  "just this one" is an edit. Commit it as returned.

For the release steps, the CI mechanism, and the versioning policy, see
[RELEASING.md](RELEASING.md) — the single home for the release procedure.

## Workflows run to their end

Never end a workflow prematurely, and never dictate counter to the workflow in a
handoff.
