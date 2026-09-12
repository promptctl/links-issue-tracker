package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

var (
	// [LAW:one-source-of-truth] Marker pairs are the canonical ownership boundary for AGENTS.md content.
	litAgentsMarkers    = markerPair{begin: "<!-- BEGIN LIT INTEGRATION -->", end: "<!-- END LIT INTEGRATION -->"}
	legacyAgentsMarkers = markerPair{begin: "<!-- BEGIN LINKS INTEGRATION -->", end: "<!-- END LINKS INTEGRATION -->"}
)

type agentsInstallResult struct {
	Path    string
	Created bool
	Changed bool
	Source  templates.Source
}

// renderLinksAgentsSection is the crossing where resolved template content — an
// override the user wrote, or the embedded default — becomes a managed section.
// [LAW:parse-dont-validate] Nothing downstream re-checks the shape, because
// downstream only ever holds a managedSection.
func renderLinksAgentsSection(workspaceRoot string) (managedSection, templates.Source, error) {
	content, source, err := templates.LoadWithSource(templates.AgentsSectionTemplateName, workspaceRoot)
	if err != nil {
		return managedSection{}, source, fmt.Errorf("load agent section template: %w", err)
	}
	section, err := litAgentsMarkers.parse(content)
	if err != nil {
		return managedSection{}, source, fmt.Errorf("agent section template (via %s): %w", source, err)
	}
	return section, source, nil
}

// writeManagedFile writes a managed marker-delimited section to filename.
// For new files, headerPrefix is prepended before the section.
// lit only owns the content between the BEGIN/END markers; everything else
// in the file is the user's and is preserved across installs and refreshes.
func writeManagedFile(rootDir, filename, headerPrefix string, section managedSection) (agentsInstallResult, error) {
	filePath := filepath.Join(rootDir, filename)
	content, err := os.ReadFile(filePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return agentsInstallResult{}, fmt.Errorf("read %s: %w", filename, err)
	}
	// Create-vs-adopt keys on whether the file holds meaningful content, not on
	// whether it exists: a whitespace-only file adopted as-is would receive the
	// managed section without headerPrefix — for AGENTS.md, a file whose "# AGENTS"
	// heading never lands, so the managed block is all the file ever has.
	if strings.TrimSpace(string(content)) == "" {
		initial := headerPrefix + section.text
		if mkdirErr := os.MkdirAll(filepath.Dir(filePath), 0o755); mkdirErr != nil {
			return agentsInstallResult{}, fmt.Errorf("create directory for %s: %w", filename, mkdirErr)
		}
		if writeErr := os.WriteFile(filePath, []byte(initial), 0o644); writeErr != nil {
			return agentsInstallResult{}, fmt.Errorf("write %s: %w", filename, writeErr)
		}
		return agentsInstallResult{Path: filePath, Created: true, Changed: true}, nil
	}

	// [LAW:one-source-of-truth] The change signal compares the final content
	// against the original file bytes — not against the post-migration content.
	// A marker-only migration (legacy markers, body identical to the template)
	// mutates the file yet leaves upsertManagedSection's own diff empty; comparing
	// against the original is the only definition of "changed" that persists it.
	original := string(content)
	migrated := migrateMarkers(original, legacyAgentsMarkers, litAgentsMarkers)
	updated, _ := upsertManagedSection(migrated, section)
	if updated == original {
		return agentsInstallResult{Path: filePath, Created: false, Changed: false}, nil
	}
	if err := os.WriteFile(filePath, []byte(updated), 0o644); err != nil {
		return agentsInstallResult{}, fmt.Errorf("write %s: %w", filename, err)
	}
	return agentsInstallResult{Path: filePath, Created: false, Changed: true}, nil
}

// ensureLinksAgentFiles is the single enforcer for agent config file updates
// (AGENTS.md and CLAUDE.md). lit only owns the content between the BEGIN/END
// markers; everything else in each file is the user's and is preserved.
// [LAW:single-enforcer] All agent config file writes go through this one function.
func ensureLinksAgentFiles(rootDir string) (agents agentsInstallResult, claude agentsInstallResult, err error) {
	section, source, err := renderLinksAgentsSection(rootDir)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}

	agentsResult, err := writeManagedFile(rootDir, "AGENTS.md", "# AGENTS\n\n", section)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	agentsResult.Source = source

	claudeResult, err := writeManagedFile(rootDir, "CLAUDE.md", "", section)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	claudeResult.Source = source

	return agentsResult, claudeResult, nil
}
