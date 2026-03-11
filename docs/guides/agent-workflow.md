# Agent workflow

## Baseline loop

1. Read workspace context:
   - `lit workspace --json`
2. Find work:
   - `lit ready --json`
   - `lit ls --query "status:open type:task" --sort priority:asc,updated_at:asc --json`
   - `lit ls --query "status:open" --json`
3. Choose priority by impact/urgency when creating new work:
   - `lit new --title "..." --type task --priority <0..4> --json`
4. Update issue state as work starts:
   - `lit comment add <issue-id> --body "Starting: <plan>" --json`
5. Sync:
   - `lit sync pull ...` before work
6. Close out:
   - `lit comment add <issue-id> --body "Done: <summary>" --json`
   - `lit close <issue-id> --reason "<completion reason>" --json`
   - `git add -A && git commit -m "<summary>"`

## Required assumptions for reliable automation

- hooks are installed once (`lit hooks install`)
- remote names come from Git (`git remote -v`)
- retries are attempted before escalating user-visible failures

## Machine-readable mode

Prefer `--json` on read and write commands for deterministic parsing.
