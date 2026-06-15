# Dolt Remote Sync

`links` sync is Dolt-native and uses Dolt git-remote support directly.
Git remotes are the canonical remote configuration.

## Version requirement

- Required Dolt version: `>= 1.81.10`
- Enforced at app startup through `internal/doltcli.RequireMinimumVersion`.

## Local data location

The Links Dolt database is shared across all worktrees in the same clone:

```txt
$(git rev-parse --git-common-dir)/links/dolt
```

`lit sync` commands run in the current repo/worktree root and operate on that database.

## First clone (bootstrap)

On a fresh clone of a repo that already uses `lit`, run `lit init`: it detects that
the remote carries existing ticket data and adopts that history wholesale, so the
clone starts with the real backlog. This is the first-receive path — not `lit sync
pull`. A fresh store has its own unrelated root commit, so a pull against the remote
fails with `no common ancestor`; adoption resets the local branch to the remote head
instead. Once the clone has adopted, its history is shared with the remote and the
ordinary `lit sync pull` / `lit sync push` flow applies.

## Typical setup

```sh
lit hooks install
git remote add origin https://github.com/<org>/<repo>.git
lit sync remote ls --json
lit sync fetch
lit sync pull --json
```

## Daily workflow

```sh
lit sync status
lit sync pull --json
# ...work with lit commands...
lit sync push --json
```

## Commands

- `lit sync status [--json]`
- `lit sync remote ls [--json]`
- `lit sync fetch [--remote <name>] [--prune] [--verbose] [--json]`
- `lit sync pull [--remote <name>] [--verbose] [--json]`
- `lit sync push [--remote <name>] [--set-upstream] [--force] [--verbose] [--json]`

Sync branch selection:

- default: repository default branch from the configured remote
- debug override: set `LINKS_DEBUG_DOLT_SYNC_BRANCH=<branch>`

Sync remote selection for pull/push when `--remote` is omitted:

- branch upstream remote (when configured)
- otherwise, the single configured Git remote
- if no eligible remote exists, sync pull/push return `status=skipped` and do not run Dolt sync side effects

Text output behavior:

- default output is terse and hides remote-specific details
- use `--verbose` to include remote/branch details in text output

Before each `lit sync` command, `lit` reconciles Dolt remotes to exactly match `git remote -v` fetch URLs:

- add missing Dolt remotes
- update changed remote URLs
- remove Dolt remotes that no longer exist in Git

## Push automation

`lit hooks install` writes `$(git rev-parse --git-common-dir)/hooks/pre-push` and chains any existing user hook.
The hook auto-runs one canonical `lit sync push` per git push, never blocks the git push, and emits a warning that includes the trigger, remote, retry command, and trace path if DB sync fails.
Successful and failed automatic runs both write trace files under the workspace `traces_dir` returned by `lit workspace --json`.

## Push cadence

The cadence — how often lit mirrors the store to the remote — is a single
config policy you own, not a per-command behavior. Set it under `[sync]` in
`config.toml` (global at `~/.config/links-issue-tracker/config.toml`, or
per-project at `.lit/config.toml`):

```toml
[sync]
cadence = "on-push"   # default
```

| value       | meaning                                                                 |
| ----------- | ----------------------------------------------------------------------- |
| `on-push`   | mirror only when the managed pre-push hook runs (one push per `git push`). The default and historical behavior. |
| `on-change` | additionally mirror after every mutating lit command (`new`, `start`, `update`, `close`, `comment`, `rank`, …), shrinking the window where local ticket state is invisible to other clones. |

`on-change` runs the same `lit sync push` the pre-push hook runs, after the
command completes. It is best-effort: a push failure is surfaced on stderr and
recorded as an automation trace, but never fails the command — the ticket
change is already durable in the local Dolt store. An unknown cadence value is
rejected at config load.
