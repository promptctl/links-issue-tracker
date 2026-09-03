package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

const (
	// [LAW:one-source-of-truth] Marker pairs are the canonical ownership boundary for AGENTS.md content.
	litAgentsBeginMarker    = "<!-- BEGIN LIT INTEGRATION -->"
	litAgentsEndMarker      = "<!-- END LIT INTEGRATION -->"
	legacyAgentsBeginMarker = "<!-- BEGIN LINKS INTEGRATION -->"
	legacyAgentsEndMarker   = "<!-- END LINKS INTEGRATION -->"
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return agentsInstallResult{}, fmt.Errorf("read %s: %w", filename, err)
	}
	// Create-vs-adopt keys on whether the file holds meaningful content, not on
	// whether it exists: a whitespace-only file adopted as-is would receive the
	// managed section without headerPrefix — for SKILL.md, a skill file with no
	// frontmatter, which the harness cannot discover.
	if strings.TrimSpace(string(content)) == "" {
		initial := headerPrefix + section
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
	migrated := migrateMarkers(original, legacyAgentsBeginMarker, legacyAgentsEndMarker, litAgentsBeginMarker, litAgentsEndMarker)
	updated, _ := upsertManagedSection(migrated, section, beginMarker, endMarker)
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
		return agentsInstallResult{}, agentsInstallResult{}, fmt.Errorf("load agent section template: %w", err)
	}

	agentsResult, err := writeManagedFile(rootDir, "AGENTS.md", "# AGENTS\n\n", section, litAgentsBeginMarker, litAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	agentsResult.Source = source

	claudeResult, err := writeManagedFile(rootDir, "CLAUDE.md", "", section, litAgentsBeginMarker, litAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, agentsInstallResult{}, err
	}
	claudeResult.Source = source

	return agentsResult, claudeResult, nil
}

// nextSkillRelPath is where the harness discovers the project /next skill.
const nextSkillRelPath = ".claude/skills/next/SKILL.md"

// nextSkillFrontmatter is the harness-parsed skill identity. It must sit at
// byte 0 of SKILL.md, so it cannot live inside the managed markers: like the
// "# AGENTS" heading, it is written once at creation and is the user's
// afterward, while lit owns only the marker-delimited body below it.
const nextSkillFrontmatter = "---\nname: next\ndescription: Pull the next ticket\n---\n\n"

// normalizeManagedSection parses resolved template content into a
// marker-delimited managed section — the only shape upsertManagedSection
// reconciles instead of growing. [LAW:parse-dont-validate] Exactly two shapes
// are legal: no markers at all (plain guidance text, the quickstart-template
// convention) is wrapped, and exactly one BEGIN/END pair spanning the whole
// content passes through. Every other shape — a lone marker, a stray extra
// one, text outside the pair — would duplicate part of itself on every
// subsequent reconcile, so it is rejected loudly instead of laundered into a
// file that grows without bound. [LAW:no-silent-failure]
func normalizeManagedSection(content, beginMarker, endMarker string) (string, error) {
	begins := strings.Count(content, beginMarker)
	ends := strings.Count(content, endMarker)
	if begins == 0 && ends == 0 {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return beginMarker + "\n" + content + endMarker + "\n", nil
	}
	// The pass-through re-emits the trimmed block, not the raw bytes:
	// whitespace outside the marker span would sit outside the region
	// upsertManagedSection replaces and compound on every reconcile.
	trimmed := strings.TrimSpace(content)
	if begins == 1 && ends == 1 && strings.HasPrefix(trimmed, beginMarker) && strings.HasSuffix(trimmed, endMarker) {
		return trimmed + "\n", nil
	}
	return "", fmt.Errorf("content must either contain no %s/%s markers or be exactly one such block; found %d begin and %d end marker(s)", beginMarker, endMarker, begins, ends)
}

// ensureNextSkillFile writes the managed /next skill body to
// .claude/skills/next/SKILL.md, resolved project > global > embedded like
// every managed template. [LAW:single-enforcer] All writes of the shipped
// /next skill go through this one function.
func ensureNextSkillFile(rootDir string) (agentsInstallResult, error) {
	section, source, err := templates.LoadWithSource(templates.NextSkillTemplateName, rootDir)
	if err != nil {
		return agentsInstallResult{}, fmt.Errorf("load next skill template: %w", err)
	}
	section, err = normalizeManagedSection(section, litAgentsBeginMarker, litAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, fmt.Errorf("next skill template (via %s): %w", source, err)
	}
	result, err := writeManagedFile(rootDir, filepath.FromSlash(nextSkillRelPath), nextSkillFrontmatter, section, litAgentsBeginMarker, litAgentsEndMarker)
	if err != nil {
		return agentsInstallResult{}, err
	}
	result.Source = source
	return result, nil
}
