# CLI reference

For full syntax, run:

```sh
lit help
```

## Global output mode

`lit` resolves output mode in one place with deterministic precedence:

1. global flags before the command (`--output auto|text|json` and `--json`, last flag wins)
2. `LIT_OUTPUT` environment default
3. automatic fallback (`text` on TTY, `json` otherwise)

Command-local `--json` remains supported for existing scripts.

## Common commands

### Create/list/show

```sh
lit new --title "..." --type task --priority 2 --json
lit ready --json
lit ls --json
lit show <issue-id> --json
```

### Lifecycle

```sh
lit close <issue-id> --reason "..." --json
lit open <issue-id> --reason "..." --json
lit archive <issue-id> --reason "..." --json
lit delete <issue-id> --reason "..." --json
```

### Relationships and labels

```sh
lit parent set <child-id> <parent-id> --json
lit dep add <src-id> <dst-id> --type related-to --json
lit label add <issue-id> <label> --json
```

### Sync and automation

```sh
lit migrate beads --apply --json
lit hooks install
lit sync status --json
lit sync pull --remote origin --branch main
lit sync push --remote origin --branch main
```

### Health and recovery

```sh
lit doctor --json
lit fsck --repair --json
lit backup create --json
lit backup restore --latest --json
```
