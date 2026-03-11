# links

`links` is a worktree-native issue tracker with a flat CLI: `lit`.

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
go install github.com/bmf/links-issue-tracker/cmd/lit@latest
```

Output mode defaults to `auto`:
- TTY sessions emit text
- non-TTY sessions emit JSON
- `--json` remains an explicit JSON shorthand for script compatibility
- `--output auto|text|json` and `LIT_OUTPUT` control overrides
- failure output in JSON mode uses a stable envelope:
  - `error.code` (`usage|validation|not_found|conflict|corruption|generic`)
  - `error.reason`
  - `error.remediation`
  - `error.trace_ref`
  - `error.exit_code`
  - `error.message`

Initialize in your repo (auto-migrates Beads residue and installs defaults):

```sh
lit init --json
git remote -v
lit sync remote ls --json
```

If needed, you can run migration directly:

```sh
lit migrate beads --apply --json
```

Create and inspect work:

```sh
lit new --title "First task" --type task --priority 2 --json
lit ready --json
lit update <issue-id> --status in_progress --json
lit start <issue-id> --reason "claim" --json
lit done <issue-id> --reason "completed" --json
lit ls --json
lit show <issue-id> --json
```

Push/pull DB changes through Dolt remotes mirrored from Git remotes:

```sh
lit sync pull --json
# ...make lit changes...
lit sync push --json
```

Useful commands:

```sh
lit quickstart --json
lit workspace --json
lit doctor --json
```

## More docs

- Docs index (recommended start): [docs/index.md](docs/index.md)
- Sync and remote behavior: [docs/dolt-remote-sync.md](docs/dolt-remote-sync.md)
- Full command reference: `lit help`
- Agent-focused workflow: `lit quickstart` / `lit quickstart --json`
