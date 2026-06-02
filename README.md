# links (`lit`)

**An issue tracker that lives inside your git repo and is built for AI agents to drive.**

`links-issue-tracker` — the CLI is `lit` — keeps your backlog *in the repository*, next
to the code it describes. There's no server to run, no account to create, and no web app
to keep in sync. Issues live in an embedded [Dolt](https://www.dolthub.com/) database, so
every change to a ticket is a versioned, committed database mutation that travels with
your code through the git remotes you already use.

It's designed so an autonomous coding agent can pull the next piece of work, claim it,
finish it, and file follow-ups — coordinating with you and with other agents through one
small, predictable CLI.

## Why it's interesting

- **The backlog ships with the code.** Clone the repo, get the issues. No external service,
  no drift between "what the tracker says" and "what the branch contains."
- **Every mutation is a commit.** State is a Dolt database, so issue history is real,
  auditable version history — not an activity log bolted onto a SaaS.
- **Built agent-first.** Commands like `lit ready`, `lit start`, and `lit done` return
  guidance, not just data — the workflow an agent should follow is baked into the output.
- **One write boundary, one identity per clone.** All worktrees of a clone share one issue
  view (keyed off `git rev-parse --git-common-dir`). You never edit the database by hand.
- **Minimal and deterministic.** Inspired by [beads](https://github.com/steveyegge/beads),
  `lit` deliberately keeps a small surface area and prefers explicit errors over silent
  fallbacks.

## Get started

### Requirements

- A git repository (or worktree)
- The Go toolchain — `lit` builds from source

> The `dolt` CLI is **not** required to run `lit`; the Dolt storage engine is compiled in.
> (It's only used as a test oracle when developing `lit` itself.)
> On macOS, building the embedded engine needs ICU and zstd headers — see
> [docs/introduction/installation.md](docs/introduction/installation.md) if `go build`
> hits ICU/zstd errors.

### Install

```sh
git clone https://github.com/promptctl/links-issue-tracker
cd links-issue-tracker
./scripts/install.sh
```

`install.sh` builds `lit` from this checkout and installs it onto your `PATH` (it also warns
you about any stale `lit` binaries shadowing the new one). Confirm it landed:

```sh
lit version
```

### A 60-second tour

From inside any git repository:

```sh
lit init                                              # set up the workspace for this clone
lit new --title "Build the landing page" \
        --topic landing --type task                   # file your first issue
lit ready                                             # see what's workable, in priority order
lit start <id>                                        # claim a ticket
lit done <id>                                         # finish it (a two-step confirm follows)
```

`lit init` also wires this repo for agents: it adds a short `lit` section to `AGENTS.md`
and `CLAUDE.md` telling any coding agent to run `lit quickstart` before touching work.

## Pointing an AI agent at it

If you want an agent (Claude Code, Cursor, etc.) to do the work, give it
**[docs/agent-setup.md](docs/agent-setup.md)** — a concrete, copy-pasteable install +
workspace-init + core-loop guide written for agents. In a repo that's already initialized,
the agent's entry point is simply `lit quickstart`, which prints the live command reference.

## How it works

- **Storage** — issues are rows in an embedded Dolt SQL database under
  `$(git rev-parse --git-common-dir)/links/`. Each `lit` write validates input, updates rows
  in a transaction, and commits the working set, so local state is durable after every change.
- **Sync** — `lit sync` mirrors that Dolt data through your existing git remotes; remote
  config is derived from `git remote -v`, so there's nothing extra to configure.
- **Automation** — `lit hooks install` adds a shared `pre-push` hook (once per clone) that
  attempts a sync on push without ever blocking it.

More depth: [docs/introduction/what-is-links.md](docs/introduction/what-is-links.md) ·
[docs/concepts.md](docs/concepts.md) · [docs/architecture.md](docs/architecture.md).

## Credit

`links` is directly inspired by [beads](https://github.com/steveyegge/beads) by Steve Yegge.
The core idea — treat issue tracking as part of the repository workflow so humans and agents
coordinate through a fast local CLI and syncable state — is his. `links` is an independent
implementation of that idea with a deliberately minimal, agent-native surface.
