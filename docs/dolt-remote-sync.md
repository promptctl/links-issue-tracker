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

## Typical setup

```sh
lit hooks install
git remote add origin https://github.com/<org>/<repo>.git
lit sync remote ls --json
lit sync fetch --remote origin
lit sync pull --remote origin --branch main
```

## Daily workflow

```sh
lit sync status
lit sync pull --remote origin --branch main
# ...work with lit commands...
lit sync push --remote origin --branch main
```

## Commands

- `lit sync status [--json]`
- `lit sync remote ls [--json]`
- `lit sync fetch [--remote <name>] [--prune] [--json]`
- `lit sync pull [--remote <name>] [--branch <name>] [--json]`
- `lit sync push [--remote <name>] [--branch <name>] [--set-upstream] [--force] [--json]`

Before each `lit sync` command, `lit` reconciles Dolt remotes to exactly match `git remote -v` fetch URLs:

- add missing Dolt remotes
- update changed remote URLs
- remove Dolt remotes that no longer exist in Git

## Push automation

`lit hooks install` writes `$(git rev-parse --git-common-dir)/hooks/pre-push` and chains any existing user hook.
The hook auto-runs `lit sync push` for pushed branches, never blocks the git push, and emits a warning that includes the trigger, remote, branch, retry command, and trace path if DB sync fails.
Successful and failed automatic runs both write trace files under the workspace `traces_dir` returned by `lit workspace --json`.
