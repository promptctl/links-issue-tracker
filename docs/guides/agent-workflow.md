# Agent workflow

## Baseline loop

1. Read workspace context:
   - `lnks workspace --json`
2. Find work:
   - `lnks ready --json`
   - `lnks ls --query "status:open" --json`
3. Update issue state as work starts:
   - `lnks update <issue-id> --status in_progress --json`
   - `lnks start <issue-id> --reason "claim" --json`
   - `lnks comment add <issue-id> --body "Starting: <plan>" --json`
4. Sync:
   - `lnks sync pull --json` before work
5. Close out:
   - `lnks done <issue-id> --reason "<completion summary>" --json`
   - `lnks comment add <issue-id> --body "Done: <summary>" --json`
   - `git add -A && git commit -m "<summary>"`

## Required assumptions for reliable automation

- hooks are installed once (`lnks hooks install`)
- remote names come from Git (`git remote -v`)
- sync targets the remote default branch (override only with `LINKS_DEBUG_DOLT_SYNC_BRANCH`)
- retries are attempted before escalating user-visible failures
- automatic traces live under `traces_dir` from `lnks workspace --json`; startup preflight and managed hooks both write there

## Machine-readable mode

Prefer `--json` on read and write commands for deterministic parsing.
