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

func renderLinksAgentsFile() string {
	// [LAW:one-source-of-truth] Full-file refresh derives AGENTS.md from the canonical managed section renderer.
	return "# AGENTS\n\n" + renderLinksAgentsSection()
}

func renderLinksAgentsSection() string {
	return strings.TrimSpace(`
<!-- BEGIN LINKS INTEGRATION -->
## links Agent-Native Workflow

This repository is configured for agent-native issue tracking with `+"`lit`"+`.

Session bootstrap (every session / after compaction):
1. Run `+"`lit quickstart --refresh`"+`.
2. Run `+"`lit workspace`"+`.
3. If remotes are configured, run `+"`lit sync pull`"+` (uses upstream remote when configured, otherwise the single configured remote; debug override: `+"`LINKS_DEBUG_DOLT_SYNC_BRANCH`"+`).

Work acquisition:
1. Use the issue ID already assigned in context when present.
2. Check current ready work with `+"`lit ready`"+`. This is the only correct source for work selection — do NOT fall back to `+"`lit ls`"+` or other queries if it fails. If `+"`lit ready`"+` fails or returns empty, stop and report the error.
3. Do NOT extract bare ID lists from `+"`--json`"+` output and discard the rest. The full output contains priority, status, annotations, and readiness context that you need to make informed decisions. Stripping it to a list of IDs loses the information that distinguishes "ready high-priority work" from "everything open."
4. Create or claim an issue only when the work needs tracking. Do not create tickets for trivial drive-by edits like one-line doc fixes that will be resolved immediately.
5. For tracked work, mark it in progress with `+"`lit update <issue-id> --status in_progress`"+` (or `+"`lit start ...`"+`).
6. For tracked work, record work start with `+"`lit comment add <issue-id> --body \"Starting: <plan>\"`"+`.

Execution:
- Keep structure current with `+"`lit parent`"+` / `+"`lit dep`"+` / `+"`lit label`"+` / `+"`lit comment`"+`.

Closeout:
1. For tracked work, add completion summary: `+"`lit comment add <issue-id> --body \"Done: <summary>\"`"+`.
2. For tracked work, close completed issue: `+"`lit close <issue-id> --reason \"<completion reason>\"`"+`.
3. You MUST create a git commit for the completed work: `+"`git add -A && git commit -m \"<summary>\"`"+`.
4. Work is NOT complete until the commit exists. Do NOT start the next issue before committing.

Traceability:
- `+"`git push`"+` triggers hook-driven `+"`lit sync push`"+` attempts (warn-only on failure).
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

func rewriteLinksAgentsFile(rootDir string) (agentsInstallResult, error) {
	agentsPath := filepath.Join(rootDir, "AGENTS.md")
	rendered := renderLinksAgentsFile()
	content, err := os.ReadFile(agentsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return agentsInstallResult{}, fmt.Errorf("read AGENTS.md: %w", err)
		}
		if writeErr := os.WriteFile(agentsPath, []byte(rendered), 0o644); writeErr != nil {
			return agentsInstallResult{}, fmt.Errorf("write AGENTS.md: %w", writeErr)
		}
		return agentsInstallResult{Path: agentsPath, Created: true, Changed: true}, nil
	}
	if string(content) == rendered {
		return agentsInstallResult{Path: agentsPath, Created: false, Changed: false}, nil
	}
	if err := os.WriteFile(agentsPath, []byte(rendered), 0o644); err != nil {
		return agentsInstallResult{}, fmt.Errorf("write AGENTS.md: %w", err)
	}
	return agentsInstallResult{Path: agentsPath, Created: false, Changed: true}, nil
}

func stripBeadsAgentsSection(content string) (string, bool) {
	return removeManagedSection(content, beadsAgentsBeginMarker, beadsAgentsEndMarker)
}
