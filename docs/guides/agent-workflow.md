# Agent workflow

## Baseline loop

1. Read workspace context:
   - `lit workspace --json`
2. Find work:
   - `lit ready --json`
   - `lit ls --query "status:open" --json`
3. Update issue state as work starts:
   - `lit start <issue-id> --reason "claim" --json`
   - `lit comment add <issue-id> --body "Starting: <plan>" --json`
4. Sync:
   - `lit sync pull ...` before work
5. Close out:
   - `lit done <issue-id> --reason "<completion summary>" --json`
   - `lit comment add <issue-id> --body "Done: <summary>" --json`
   - `git add -A && git commit -m "<summary>"`

## Required assumptions for reliable automation

- hooks are installed once (`lit hooks install`)
- remote names come from Git (`git remote -v`)
- retries are attempted before escalating user-visible failures

## Machine-readable mode

Prefer `--json` on read and write commands for deterministic parsing.
