# CLI reference

For full syntax, run:

```sh
lnks help
```

## Global output mode

`lnks` resolves output mode in one place with deterministic precedence:

1. global flags before the command (`--output auto|text|json` and `--json`, last flag wins)
2. `LNKS_OUTPUT` environment default
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
- Precedence is deterministic: `--output` > `--json` > `LNKS_OUTPUT` > auto.
- Migration expectation: existing scripts should keep using `--json` (or set `LNKS_OUTPUT=json`) for explicit machine-readable output.

### Create/list/show

```sh
lnks new --title "..." --type task --priority 2 --json
lnks ready --json
lnks ls --query "status:open" --json
lnks ls --json
lnks show <issue-id> --json
```

### Lifecycle

```sh
lnks update <issue-id> --status in_progress --json
lnks start <issue-id> --reason "claim" --json
lnks done <issue-id> --reason "implemented" --json
lnks close <issue-id> --reason "..." --json
lnks open <issue-id> --reason "..." --json
lnks archive <issue-id> --reason "..." --json
lnks delete <issue-id> --reason "..." --json
```

`lnks update` also supports field edits in one command:

```sh
lnks update <issue-id> --title "..." --priority 1 --assignee "alice" --labels api,urgent --json
```

### Relationships and labels

```sh
lnks parent set <child-id> <parent-id> --json
lnks dep add <src-id> <dst-id> --type related-to --json
lnks label add <issue-id> <label> --json
```

### Sync and automation

```sh
lnks migrate beads --apply --json
lnks hooks install
lnks sync status --json
lnks sync pull --json
lnks sync push --json
# add --verbose for remote/branch details in text mode
```

Debug override:

- `LINKS_DEBUG_DOLT_SYNC_BRANCH=<branch>` forces `lnks sync pull` and `lnks sync push` to target that branch.
- when `--remote` is omitted, `lnks sync pull` / `lnks sync push` use upstream remote first, then the single configured remote.
- when no eligible remote exists, pull/push return `status=skipped` and do not attempt Dolt sync side effects.
- text output is intentionally terse by default; pass `--verbose` to show remote details.

### Health and recovery

```sh
lnks doctor --json
lnks fsck --repair --json
lnks backup create --json
lnks backup restore --latest --json
```
