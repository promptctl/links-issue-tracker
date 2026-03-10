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

When output mode resolves to JSON, failures emit a stable envelope:

- `error.code`: `usage|validation|not_found|conflict|corruption|generic`
- `error.reason`: deterministic failure classifier
- `error.remediation`: actionable next step
- `error.trace_ref`: stable hash reference for the failure payload
- `error.exit_code`: numeric process exit code
- `error.message`: original error text

## Common commands

### Output mode

- Default mode is `auto`.
- `auto` renders text in terminals and JSON in non-interactive contexts.
- `--json` remains supported as shorthand for JSON mode.
- `--output auto|json|text` overrides everything else.
- Precedence is deterministic: `--output` > `--json` > `LIT_OUTPUT` > auto.
- Migration expectation: existing scripts should keep using `--json` (or set `LIT_OUTPUT=json`) for explicit machine-readable output.

### Create/list/show

```sh
lit new --title "..." --type task --priority 2 --json
lit ready --json
lit ls --query "status:open" --json
lit ls --json
lit show <issue-id> --json
```

### Lifecycle

```sh
lit start <issue-id> --reason "claim" --json
lit done <issue-id> --reason "implemented" --json
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
