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
	beadsAgentsBeginMarker = "<!-- BEGIN BEADS INTEGRATION -->"
	beadsAgentsEndMarker   = "<!-- END BEADS INTEGRATION -->"
)

type agentsInstallResult struct {
	Path    string
	Created bool
	Changed bool
}

func renderLinksAgentsFile(workspaceRoot string) (string, error) {
	// [LAW:one-source-of-truth] Full-file refresh derives AGENTS.md from the canonical managed section renderer.
	section, err := renderLinksAgentsSection(workspaceRoot)
	if err != nil {
		return "", err
	}
	return "# AGENTS\n\n" + section, nil
}

func renderLinksAgentsSection(workspaceRoot string) (string, error) {
	return templates.Load(templates.AgentsSectionTemplateName, workspaceRoot)
}

func ensureLinksAgentsSection(rootDir string) (agentsInstallResult, error) {
	agentsPath := filepath.Join(rootDir, "AGENTS.md")
	section, err := renderLinksAgentsSection(rootDir)
	if err != nil {
		return agentsInstallResult{}, fmt.Errorf("load AGENTS section template: %w", err)
	}
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
	rendered, err := renderLinksAgentsFile(rootDir)
	if err != nil {
		return agentsInstallResult{}, fmt.Errorf("render AGENTS file: %w", err)
	}
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
