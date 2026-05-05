package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bmf/links-issue-tracker/internal/templates"
)

const (
	// [LAW:one-source-of-truth] Marker pairs are the canonical ownership boundary for AGENTS.md content.
	linksAgentsBeginMarker = "<!-- BEGIN LINKS INTEGRATION -->"
	linksAgentsEndMarker   = "<!-- END LINKS INTEGRATION -->"
)

type agentsInstallResult struct {
	Path    string
	Created bool
	Changed bool
	Source  templates.Source
}

func renderLinksAgentsSection(workspaceRoot string) (string, templates.Source, error) {
	return templates.LoadWithSource(templates.AgentsSectionTemplateName, workspaceRoot)
}

// writeManagedFile writes a managed marker-delimited section to filename.
// For new files, headerPrefix is prepended before the section.
// lit only owns the content between the BEGIN/END markers; everything else
// in the file is the user's and is preserved across installs and refreshes.
func writeManagedFile(rootDir, filename, headerPrefix, section, beginMarker, endMarker string) (agentsInstallResult, error) {
	filePath := filepath.Join(rootDir, filename)
	content, err := os.ReadFile(filePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return agentsInstallResult{}, fmt.Errorf("read %s: %w", filename, err)
		}
		initial := headerPrefix + section
		if writeErr := os.WriteFile(filePath, []byte(initial), 0o644); writeErr != nil {
			return agentsInstallResult{}, fmt.Errorf("write %s: %w", filename, writeErr)
		}
		return agentsInstallResult{Path: filePath, Created: true, Changed: true}, nil
	}

	updated, changed := upsertManagedSection(string(content), section, beginMarker, endMarker)
	if !changed {
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
		return agentsInstallResult{}, agentsInstallResult{}, fmt.Errorf("load agent section template: %w", err)
	}

	agentsResult, err := writeManagedFile(rootDir, "AGENTS.md", "# AGENTS\n\n", section, linksAgentsBeginMarker, linksAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	agentsResult.Source = source

	claudeResult, err := writeManagedFile(rootDir, "CLAUDE.md", "", section, linksAgentsBeginMarker, linksAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	claudeResult.Source = source

	return agentsResult, claudeResult, nil
}

