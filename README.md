# links

`links` is a worktree-native issue tracker with a flat CLI: `lit`.

## Product Contract

- Human bootstrap boundary: a human runs `lit init` once per repository/worktree setup.
- Agent-primary operation after init: agents run day-to-day issue workflows (`ready`, `new`, `ls`, `show`, `comment`, `close`, `sync`) without additional manual setup.
- JSON-first automation: commands support machine-readable output via `--json` for deterministic agent behavior.

## Human Bootstrap (One Time)

Requirements:
- Git repository/worktree
- Dolt CLI `>= 1.81.10`

Install:

```sh
go install github.com/bmf/links-issue-tracker/cmd/lit@latest
```

Initialize in your repo:

```sh
lit init --json
git remote -v
lit sync remote ls --json
```

If needed, you can run migration directly:

```sh
lit migrate beads --apply --json
```

## Agent Session Loop (After Init)

```sh
lit quickstart --json
lit workspace --json
lit sync pull --remote origin --branch main --json
lit new --title "First task" --type task --priority 2 --json
lit ready --json
lit ls --json
lit show <issue-id> --json
```

Typical closeout:

```sh
lit comment add <issue-id> --body "Done: <summary>" --json
lit close <issue-id> --reason "<completion reason>" --json
lit sync push --remote origin --branch main
```

## Predictable Automation and Traceability

Automation is intentionally constrained and inspectable:

- Operational behavior is command-driven. Agents call explicit commands in a deterministic order, typically from `lit quickstart --json`.
- `--json` outputs provide stable machine-readable inputs for the next step.
- Hook behavior is explicit: `lit hooks install` sets up push-time sync attempts. A hook sync failure warns but does not block `git push`.

To inspect what happened and why:

- per-issue history and comments: `lit show <issue-id> --json`
- workspace identity/revision state: `lit workspace --json`
- sync mechanics and remote reconciliation: [docs/dolt-remote-sync.md](docs/dolt-remote-sync.md)

## Useful Commands

```sh
lit quickstart --json
lit workspace --json
lit doctor --json
lit help
```

## More Docs

- Docs index (recommended start): [docs/index.md](docs/index.md)
- Sync and remote behavior: [docs/dolt-remote-sync.md](docs/dolt-remote-sync.md)
- Full command reference: `lit help`
- Agent-focused workflow: `lit quickstart` / `lit quickstart --json`
