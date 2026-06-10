# Getting started

A first working session with `lit`, start to finish: initialize a repository, file a
ticket, claim it, and close it. Every command here is real — you can follow along in any
git repository in about five minutes.

This page assumes `lit` is already on your `PATH`. If it isn't, do
[Installation](installation.md) first and come back.

## 1. Initialize the repository

From inside any git repository (a scratch repo is fine):

```sh
lit init
```

This does three things:

- Creates the issue store under `$(git rev-parse --git-common-dir)/links/` — inside your
  clone's git directory, shared by all of its worktrees, and never committed to your repo.
- Adds a managed `lit` section to `AGENTS.md` and `CLAUDE.md` so coding agents working in
  the repo discover the tracker on their own.
- Installs a git `pre-push` hook that keeps issue data synced (skip with `--skip-hooks`).

`lit init` is idempotent — running it again reconciles the managed files rather than
duplicating them.

## 2. File your first ticket

```sh
lit new --title "Try out lit" --topic onboarding
```

Two things to notice:

- `--topic` is required. It's a short, immutable slug naming the stable area of work
  (`auth`, `docs`, `refactor`) — it groups related tickets over time.
- The command **prints the new ticket's ID**. Use the printed ID in every later command;
  IDs are generated, not guessable.

By default the new ticket ranks at the **top** of the queue — fresh work surfaces first.
Pass `--bottom` when you're filing a batch in order and want creation order preserved.

## 3. See what's workable

```sh
lit ready
```

`ready` is the pull view: open work in rank order, blocked items excluded, the top item
being what should be picked up next. With one ticket filed, yours is the top item.

To read a ticket in full before touching anything:

```sh
lit show <id>
```

`show` prints the description, status, labels, comments, and full history — and for a
ticket inside an epic, the epic plan with siblings in rank order.

## 4. Claim it

```sh
lit start <id>
```

This moves the ticket to `in_progress` and assigns it to you. Claiming is what keeps two
people (or two agents) from silently working the same ticket; an `in_progress` ticket that
goes quiet shows up in `lit orphaned` so the work is never lost.

Leave a trail as you work:

```sh
lit comment add <id> --body "Plan: poke at the basic loop, then read the concepts doc"
```

## 5. Finish it

```sh
lit done <id>
```

This **does not close the ticket**. It prints a pre-completion checklist and an exact
`lit done <id> --apply=<token>` command. Run that printed command to actually close:

```sh
lit done <id> --apply=<token-from-the-output>
```

The two-phase close is deliberate: the pause is a checkpoint to confirm the work is
genuinely complete, and the token is derived from the ticket's current state, so a stale
token from an earlier look at the ticket won't apply. If the ticket should be closed
*without* being finished — obsolete, duplicate, won't-fix — use `lit close <id>` instead,
which records that distinction honestly.

## You've done the whole loop

```text
lit init → lit new → lit ready → lit show → lit start → lit comment add → lit done (twice)
```

That loop is the product. Everything else — dependencies, epics, lanes, sync — is
structure layered on top of it.

## Where to go next

- [Core concepts](../concepts.md) — the workspace model, the single write boundary, and
  first-class relationships.
- [CLI reference](../cli-reference.md) — every command, flag, and exit code.
- [Agent setup & workflow](../agent-setup.md) — onboarding an AI coding agent to a
  `lit`-tracked repo.
- [Sync and remotes](../dolt-remote-sync.md) — sharing one backlog across clones.
