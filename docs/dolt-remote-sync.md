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

`lnks sync` commands run in the current repo/worktree root and operate on that database.

## Typical setup

```sh
lnks hooks install
git remote add origin https://github.com/<org>/<repo>.git
lnks sync remote ls --json
lnks sync fetch
lnks sync pull --json
```

## Daily workflow

```sh
lnks sync status
lnks sync pull --json
# ...work with lnks commands...
lnks sync push --json
```

## Commands

- `lnks sync status [--json]`
- `lnks sync remote ls [--json]`
- `lnks sync fetch [--remote <name>] [--prune] [--verbose] [--json]`
- `lnks sync pull [--remote <name>] [--verbose] [--json]`
- `lnks sync push [--remote <name>] [--set-upstream] [--force] [--verbose] [--json]`

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

Before each `lnks sync` command, `lnks` reconciles Dolt remotes to exactly match `git remote -v` fetch URLs:

- add missing Dolt remotes
- update changed remote URLs
- remove Dolt remotes that no longer exist in Git

## Push automation

`lnks hooks install` writes `$(git rev-parse --git-common-dir)/hooks/pre-push` and chains any existing user hook.
The hook auto-runs one canonical `lnks sync push` per git push, never blocks the git push, and emits a warning that includes the trigger, remote, retry command, and trace path if DB sync fails.
Successful and failed automatic runs both write trace files under the workspace `traces_dir` returned by `lnks workspace --json`.
