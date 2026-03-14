package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// [LAW:one-source-of-truth] Marker pairs are the canonical ownership boundary for AGENTS.md content.
	linksAgentsBeginMarker = "<!-- BEGIN LINKS INTEGRATION -->"
	linksAgentsEndMarker   = "<!-- END LINKS INTEGRATION -->"
	beadsAgentsBeginMarker = "<!-- BEGIN BEADS INTEGRATION -->"
	beadsAgentsEndMarker   = "<!-- END BEADS INTEGRATION -->"
)

type agentsInstallResult struct {
	Path    string
	Created bool
	Changed bool
}

func renderLinksAgentsSection() string {
	return strings.TrimSpace(`
<!-- BEGIN LINKS INTEGRATION -->
## links Agent-Native Workflow

This repository is configured for agent-native issue tracking with `+"`lnks`"+`.

Session bootstrap (every session / after compaction):
1. Run `+"`lnks quickstart --json`"+`.
2. Run `+"`lnks workspace --json`"+`.
3. If remotes are configured, run `+"`lnks sync pull --json`"+` (uses upstream remote when configured, otherwise the single configured remote; debug override: `+"`LINKS_DEBUG_DOLT_SYNC_BRANCH`"+`).

Work acquisition:
1. Use the issue ID already assigned in context when present.
2. Check current ready work with `+"`lnks ready --json`"+`.
3. If no issue exists for the task, create one with `+"`lnks new ... --json`"+`.
4. Mark work in progress with `+"`lnks update <issue-id> --status in_progress --json`"+` (or `+"`lnks start ... --json`"+`).
5. Record work start with `+"`lnks comment add <issue-id> --body \"Starting: <plan>\" --json`"+`.

Execution:
- Prefer `+"`--json`"+` on reads and writes.
- Keep structure current with `+"`lnks parent`"+` / `+"`lnks dep`"+` / `+"`lnks label`"+` / `+"`lnks comment`"+`.

Closeout:
1. Add completion summary: `+"`lnks comment add <issue-id> --body \"Done: <summary>\" --json`"+`.
2. Close completed issue: `+"`lnks close <issue-id> --reason \"<completion reason>\" --json`"+`.
3. You MUST create a git commit for the completed work: `+"`git add -A && git commit -m \"<summary>\"`"+`.
4. Work is NOT complete until the commit exists. Do NOT start the next issue before committing.

Traceability:
- `+"`git push`"+` triggers hook-driven `+"`lnks sync push`"+` attempts (warn-only on failure).
- On failure, follow command remediation output; do not invent hidden fallback behavior.

<!-- END LINKS INTEGRATION -->
`) + "\n"
}

func ensureLinksAgentsSection(rootDir string) (agentsInstallResult, error) {
	agentsPath := filepath.Join(rootDir, "AGENTS.md")
	section := renderLinksAgentsSection()
	content, err := os.ReadFile(agentsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return agentsInstallResult{}, fmt.Errorf("read AGENTS.md: %w", err)
		}
		initial := "# AGENTS\n\n" + section
		if writeErr := os.WriteFile(agentsPath, []byte(initial), 0o644); writeErr != nil {
			return agentsInstallResult{}, fmt.Errorf("write AGENTS.md: %w", writeErr)
		}
		return agentsInstallResult{Path: agentsPath, Created: true, Changed: true}, nil
	}

	updated, changed := upsertManagedSection(string(content), section, linksAgentsBeginMarker, linksAgentsEndMarker)
	if !changed {
		return agentsInstallResult{Path: agentsPath, Created: false, Changed: false}, nil
	}
	if err := os.WriteFile(agentsPath, []byte(updated), 0o644); err != nil {
		return agentsInstallResult{}, fmt.Errorf("write AGENTS.md: %w", err)
	}
	return agentsInstallResult{Path: agentsPath, Created: false, Changed: true}, nil
}

func stripBeadsAgentsSection(content string) (string, bool) {
	return removeManagedSection(content, beadsAgentsBeginMarker, beadsAgentsEndMarker)
}
