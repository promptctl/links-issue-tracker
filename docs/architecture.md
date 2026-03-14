# Architecture overview

## Storage model

`links` uses Dolt as an embedded SQL database with commit semantics. A normal write flow is:

1. command validates input
2. rows are updated in a transaction
3. workspace revision is updated
4. working set is committed

This keeps local state durable and auditable after every mutation.

## Sync model

Sync uses Dolt remotes, but remote configuration comes from Git remotes.

Before every `lnks sync` command, `links` reconciles Dolt remotes from `git remote -v` fetch URLs.

## Automation model

`lnks hooks install` installs a shared `pre-push` hook that:

- attempts one canonical `lnks sync push` per git push
- never blocks `git push`
- emits a yellow warning line with trigger, remote, retry command, and trace path on sync failure
- writes inspectable automatic-action traces under `$(lnks workspace --json | jq -r .traces_dir)`

## Failure model

`links` prefers explicit errors over silent fallback:

- invalid input -> validation error
- missing entities -> not found error
- stale revision -> stale write error
- integrity faults -> corruption error
