# links

`links` is a worktree-native issue tracker with a flat CLI: `lit`.

## Quickstart

Requirements:
- Git repository/worktree
- Dolt CLI `>= 1.81.10`

Install:

```sh
go install github.com/bmf/links-issue-tracker/cmd/lit@latest
```

Initialize in your repo and install sync automation:

```sh
lit hooks install
git remote -v
lit sync remote ls --json
```

Create and inspect work:

```sh
lit new --title "First task" --type task --priority 2 --json
lit ls --json
lit show <issue-id> --json
```

Push/pull DB changes through Dolt remotes mirrored from Git remotes:

```sh
lit sync pull --remote origin --branch main
# ...make lit changes...
lit sync push --remote origin --branch main
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
