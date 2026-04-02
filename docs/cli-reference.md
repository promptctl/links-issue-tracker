# CLI reference

For full syntax, run:

```sh
lit help
```

## Global output mode

`lit` resolves output mode in one place:

1. exact `--json` enables JSON output
2. otherwise output stays in text mode

Command-local `--json` remains supported for scripts and tool integrations.

When output mode resolves to JSON, failures emit a stable envelope:

- `error.code`: `usage|validation|not_found|conflict|corruption|generic`
- `error.reason`: deterministic failure classifier
- `error.remediation`: actionable next step
- `error.trace_ref`: stable hash reference for the failure payload
- `error.exit_code`: numeric process exit code
- `error.message`: original error text

## Common commands

### Output mode

- Default mode is text.
- `--json` is the only supported output-mode flag.
- `--output` is no longer supported.
- `--json=false` is rejected; omit `--json` when you want text output.
- Existing scripts should keep using `--json` for explicit machine-readable output.

### Create/list/show

```sh
lit new --title "..." --type task --priority 2 --json
lit ready
lit ls --query "status:open" --json
lit ls --json
lit show <issue-id> --json
```

### Lifecycle

```sh
lit update <issue-id> --status in_progress --json
lit start <issue-id> --reason "claim" --json
lit done <issue-id> --reason "implemented" --json
lit close <issue-id> --reason "..." --json
lit open <issue-id> --reason "..." --json
lit archive <issue-id> --reason "..." --json
lit delete <issue-id> --reason "..." --json
```

`lit update` also supports field edits in one command:

```sh
lit update <issue-id> --title "..." --priority 1 --assignee "alice" --labels api,urgent --json
```

### Relationships and labels

```sh
lit parent set <child-id> <parent-id> --json
lit dep add <src-id> <dst-id> --type related-to --json
lit label add <issue-id> <label> --json
```

### Sync and automation

```sh
lit migrate --apply --json
lit hooks install
lit sync status --json
lit sync pull --json
lit sync push --json
# add --verbose for remote/branch details in text mode
```

Debug override:

- `LINKS_DEBUG_DOLT_SYNC_BRANCH=<branch>` forces `lit sync pull` and `lit sync push` to target that branch.
- when `--remote` is omitted, `lit sync pull` / `lit sync push` use upstream remote first, then the single configured remote.
- when no eligible remote exists, pull/push return `status=skipped` and do not attempt Dolt sync side effects.
- text output is intentionally terse by default; pass `--verbose` to show remote details.

### Health and recovery

```sh
lit doctor --json
lit fsck --repair --json
lit backup create --json
lit backup restore --latest --json
```
