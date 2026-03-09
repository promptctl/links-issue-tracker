# AGENTS

<!-- BEGIN LINKS INTEGRATION -->
## links Agent-Native Workflow

This repository is configured for agent-native issue tracking with `lit`.

Session bootstrap (every session / after compaction):
1. Run `lit quickstart --json`.
2. Run `lit workspace --json`.
3. If a remote branch is configured, run `lit sync pull --remote origin --branch <current-branch> --json`.

Work acquisition:
1. Use the issue ID already assigned in context when present.
2. Check current ready work with `lit ready --json`.
3. If no issue exists for the task, create one with `lit new ... --json`.
4. Record work start with `lit comment add <issue-id> --body "Starting: <plan>" --json`.

Execution:
- Prefer `--json` on reads and writes.
- Keep structure current with `lit parent` / `lit dep` / `lit label` / `lit comment`.

Closeout:
1. Add completion summary: `lit comment add <issue-id> --body "Done: <summary>" --json`.
2. Close completed issue: `lit close <issue-id> --reason "<completion reason>" --json`.
3. You MUST create a git commit for the completed work: `git add -A && git commit -m "<summary>"`.
4. Work is NOT complete until the commit exists. Do NOT start the next issue before committing.

Traceability:
- `git push` triggers hook-driven `lit sync push` attempts (warn-only on failure).
- On failure, follow command remediation output; do not invent hidden fallback behavior.

<!-- END LINKS INTEGRATION -->
