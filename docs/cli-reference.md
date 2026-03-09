# CLI reference

For full syntax, run:

```sh
lit help
```

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
