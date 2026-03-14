# links

`links` is a worktree-native issue tracker with a flat CLI: `lnks`.

## Inspiration and Credit

This project is directly inspired by [beads](https://github.com/steveyegge/beads).

The goal of `links` is to apply the same core idea in this codebase: treat issue tracking as part of the repository workflow so agents and humans can coordinate through a fast local CLI and syncable state.

Most of the credit for the ideas behind this workflow should go to the creator of beads, Steve Yegge.

## Quickstart

Requirements:
- Git repository/worktree
- Dolt CLI `>= 1.81.10`

Install:

```sh
./scripts/install.sh
```

Install from outside a checkout:

```sh
go install github.com/bmf/links-issue-tracker/cmd/lnks@latest
```

Output is auto-detected (text on TTY, JSON in pipes). Override with `--json`, `--output text|json`, or `LNKS_OUTPUT=text|json`.

- `--json` remains an explicit JSON shorthand for script compatibility
- failure output in JSON mode uses a stable envelope:
  - `error.code` (`usage|validation|not_found|conflict|corruption|generic`)
  - `error.reason`
  - `error.remediation`
  - `error.trace_ref`
  - `error.exit_code`
  - `error.message`

Initialize in your repo (auto-migrates Beads residue and installs defaults):

```sh
lnks init --json
git remote -v
lnks sync remote ls --json
```

If needed, you can run migration directly:

```sh
lnks migrate beads --apply --json
```

Create and inspect work:

```sh
lnks new --title "First task" --type task --priority 2 --json
lnks ready --json
lnks update <issue-id> --status in_progress --json
lnks start <issue-id> --reason "claim" --json
lnks done <issue-id> --reason "completed" --json
lnks ls --json
lnks show <issue-id> --json
```

Push/pull DB changes through Dolt remotes mirrored from Git remotes:

```sh
lnks sync pull --json
# ...make lnks changes...
lnks sync push --json
```

Useful commands:

```sh
lnks quickstart --json
lnks workspace --json
lnks doctor --json
```

## More docs

- Docs index (recommended start): [docs/index.md](docs/index.md)
- Sync and remote behavior: [docs/dolt-remote-sync.md](docs/dolt-remote-sync.md)
- Full command reference: `lnks help`
- Agent-focused workflow: `lnks quickstart` / `lnks quickstart --json`
