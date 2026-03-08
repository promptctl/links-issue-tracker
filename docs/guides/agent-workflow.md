# Agent workflow

## Baseline loop

1. Read workspace context:
   - `lit workspace --json`
2. Find work:
   - `lit ls --query "status:open" --json`
3. Mutate safely:
   - include `--expected-revision` on writes
4. Sync:
   - `lit sync pull ...` before work
   - `lit sync push ...` after work

## Required assumptions for reliable automation

- hooks are installed once (`lit hooks install`)
- remote names come from Git (`git remote -v`)
- retries are attempted before escalating user-visible failures

## Machine-readable mode

Prefer `--json` on read and write commands for deterministic parsing.
