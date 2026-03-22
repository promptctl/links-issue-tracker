# Agent workflow

## Baseline loop

1. Read workspace context:
   - `lit workspace --json`
2. Find work:
   - `lit ready --json` — this is the **only** correct source for work selection.
   - Do NOT fall back to `lit ls` if `lit ready` fails. The two commands have different semantics: `lit ready` filters by readiness (required fields, unblocked dependencies) and orders by readiness then priority. `lit ls` returns all matching issues with no readiness filtering. A silent fallback from one to the other changes what the agent works on without any signal that something went wrong.
   - If `lit ready` fails or returns empty, **stop and report the error**. Do not improvise a replacement query.
   - Do NOT extract bare ID lists (e.g., `jq -r '.[].id'`) and feed them to an agent. The full `--json` output contains priority, status, annotations, and readiness information that agents need to make informed decisions about what to work on and why. Stripping that context turns a rich work queue into an opaque list where ordering mistakes are invisible.
3. Update issue state as work starts:
   - `lit update <issue-id> --status in_progress --json`
   - `lit start <issue-id> --reason "claim" --json`
   - `lit comment add <issue-id> --body "Starting: <plan>" --json`
4. Sync:
   - `lit sync pull --json` before work
5. Close out:
   - `lit done <issue-id> --reason "<completion summary>" --json`
   - `lit comment add <issue-id> --body "Done: <summary>" --json`
   - `git add -A && git commit -m "<summary>"`

## Required assumptions for reliable automation

- hooks are installed once (`lit hooks install`)
- remote names come from Git (`git remote -v`)
- sync targets the remote default branch (override only with `LINKS_DEBUG_DOLT_SYNC_BRANCH`)
- retries are attempted before escalating user-visible failures
- automatic traces live under `traces_dir` from `lit workspace --json`; startup preflight and managed hooks both write there

## Machine-readable mode

Prefer `--json` on read and write commands for deterministic parsing.
