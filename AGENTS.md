# AGENTS

<!-- BEGIN LINKS INTEGRATION -->
## links Agent-Native Workflow

This repository is configured for agent-native issue tracking with `lit`.

Session bootstrap (every session / after compaction):
1. Run `lit quickstart --refresh`.
2. Run `lit workspace`.
3. If remotes are configured, run `lit sync pull` (uses upstream remote when configured, otherwise the single configured remote; debug override: `LINKS_DEBUG_DOLT_SYNC_BRANCH`).

Work acquisition:
1. Use the issue ID already assigned in context when present.
2. Check current ready work with `lit ready`. This is the only correct source for work selection — do NOT fall back to `lit ls` or other queries if it fails. If `lit ready` fails or returns empty, stop and report the error.
3. Create or claim an issue only when the work needs tracking. Do not create tickets for trivial drive-by edits like one-line doc fixes that will be resolved immediately.
4. For tracked work, mark it in progress with `lit update <issue-id> --status in_progress` (or `lit start ...`).
5. For tracked work, record work start with `lit comment add <issue-id> --body "Starting: <plan>"`.

Execution:
- Keep structure current with `lit parent` / `lit dep` / `lit label` / `lit comment`.

Closeout:
1. For tracked work, add completion summary: `lit comment add <issue-id> --body "Done: <summary>"`.
2. For tracked work, close completed issue: `lit close <issue-id> --reason "<completion reason>"`.
3. You MUST create a git commit for the completed work: `git add -A && git commit -m "<summary>"`.
4. Work is NOT complete until the commit exists. Do NOT start the next issue before committing.

Traceability:
- `git push` triggers hook-driven `lit sync push` attempts (warn-only on failure).
- On failure, follow command remediation output; do not invent hidden fallback behavior.

<!-- END LINKS INTEGRATION -->
