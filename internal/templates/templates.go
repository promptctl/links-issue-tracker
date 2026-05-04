package templates

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/config"
)

const (
	AgentsSectionTemplateName = "agents-section.md"
	PrePushHookTemplateName   = "pre-push-hook.sh"
	QuickstartTemplateName    = "quickstart.md"

	guidanceNamePrefix = "guidance-"
)

var (
	//go:embed defaults/*
	defaultsFS embed.FS

	allNames = []string{
		AgentsSectionTemplateName,
		PrePushHookTemplateName,
		QuickstartTemplateName,
	}

	// shortAliases maps user-facing short names (CLI tokens) to canonical filenames.
	// [LAW:one-source-of-truth] CLI/UX mapping for template identity lives here, not spread across commands.
	shortAliases = map[string]string{
		"quickstart": QuickstartTemplateName,
		"agents":     AgentsSectionTemplateName,
		"hook":       PrePushHookTemplateName,
	}
)

// Names returns the canonical list of managed template filenames.
func Names() []string {
	out := make([]string, len(allNames))
	copy(out, allNames)
	return out
}

// ResolveShortName returns the canonical filename for a short alias
// ("quickstart", "agents", "hook"). Returns an error for unknown aliases.
func ResolveShortName(alias string) (string, error) {
	name, ok := shortAliases[strings.TrimSpace(alias)]
	if !ok {
		return "", fmt.Errorf("usage: unknown template %q (must be one of: quickstart, agents, hook)", alias)
	}
	return name, nil
}

// Load resolves a managed template with project > global > embedded precedence.
// It never writes; absence of a file at a given layer simply means that layer
// contributes nothing. The embedded default is always available as the final fallback.
func Load(name string, workspaceRoot string) (string, error) {
	projectContent, projectErr := readOptionalFile(projectTemplatePath(workspaceRoot, name))
	if projectErr != nil {
		return "", fmt.Errorf("load project template %s: %w", projectTemplatePath(workspaceRoot, name), projectErr)
	}
	globalContent, globalErr := readOptionalFile(GlobalPath(name))
	if globalErr != nil {
		return "", fmt.Errorf("load global template %s: %w", GlobalPath(name), globalErr)
	}
	embedded, err := EmbeddedDefault(name)
	if err != nil {
		return "", fmt.Errorf("load embedded template %s: %w", name, err)
	}
	resolved := firstNonEmpty(projectContent, globalContent, string(embedded))
	if resolved == "" {
		return "", fmt.Errorf("load template %s: no non-empty source", name)
	}
	return resolved, nil
}

// EmbeddedDefault returns the raw bytes of the embedded default for name.
func EmbeddedDefault(name string) ([]byte, error) {
	return defaultsFS.ReadFile(filepath.Join("defaults", name))
}

// GlobalPath returns the override path in the user's global config directory for name.
// Returns empty string when no global config directory is configured.
func GlobalPath(name string) string {
	dir := globalTemplatesDir()
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// ProjectPath returns the override path under the workspace's .lit/templates directory.
// Returns empty string when workspaceRoot is empty.
func ProjectPath(workspaceRoot string, name string) string {
	return projectTemplatePath(workspaceRoot, name)
}

// OverrideLayer identifies which override layer a resolved file came from.
type OverrideLayer string

const (
	OverrideLayerNone    OverrideLayer = ""
	OverrideLayerProject OverrideLayer = "project"
	OverrideLayerGlobal  OverrideLayer = "global"
)

// ActiveOverride returns the highest-priority existing override (project > global)
// for name. When neither layer has a file, the returned path is empty, content is
// nil, and Layer is OverrideLayerNone. Filesystem errors other than "not exist" are
// propagated.
func ActiveOverride(workspaceRoot string, name string) (path string, content []byte, layer OverrideLayer, err error) {
	// [LAW:dataflow-not-control-flow] Inspect both layers in fixed order; presence/absence is data, not branching.
	candidates := []struct {
		layer OverrideLayer
		path  string
	}{
		{OverrideLayerProject, projectTemplatePath(workspaceRoot, name)},
		{OverrideLayerGlobal, GlobalPath(name)},
	}
	for _, c := range candidates {
		if strings.TrimSpace(c.path) == "" {
			continue
		}
		raw, readErr := os.ReadFile(c.path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return "", nil, OverrideLayerNone, readErr
		}
		return c.path, raw, c.layer, nil
	}
	return "", nil, OverrideLayerNone, nil
}

func readOptionalFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(content), nil
}

func projectTemplatePath(workspaceRoot string, name string) string {
	trimmedRoot := strings.TrimSpace(workspaceRoot)
	if trimmedRoot == "" {
		return ""
	}
	return filepath.Join(trimmedRoot, ".lit", "templates", name)
}

func globalTemplatesDir() string {
	// [LAW:one-source-of-truth] Global template storage reuses config.ConfigDir as the canonical root.
	root := strings.TrimSpace(config.ConfigDir())
	if root == "" {
		return ""
	}
	return filepath.Join(root, "templates")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// GuidanceTemplateName returns the canonical template filename for a
// transition action's guidance phase (e.g. "guidance-done-pre.md").
func GuidanceTemplateName(action, phase string) string {
	return guidanceNamePrefix + action + "-" + phase + ".md"
}

// LoadGuidance resolves a guidance template for the given action and phase
// ("pre" or "post"). Unlike Load, guidance templates are optional — absence is
// not an error, it just deactivates the two-phase flow for that action.
// Real I/O errors (permission denied, path-is-a-dir) propagate so callers can
// surface them instead of silently skipping user-configured guidance.
func LoadGuidance(action, phase, workspaceRoot string) (string, error) {
	name := GuidanceTemplateName(action, phase)

	projectPath := projectTemplatePath(workspaceRoot, name)
	projectContent, projectErr := readOptionalFile(projectPath)
	if projectErr != nil {
		return "", fmt.Errorf("load guidance template %s: %w", projectPath, projectErr)
	}
	globalPath := GlobalPath(name)
	globalContent, globalErr := readOptionalFile(globalPath)
	if globalErr != nil {
		return "", fmt.Errorf("load guidance template %s: %w", globalPath, globalErr)
	}

	// Embedded default is optional for guidance — missing is not an error.
	var embedded string
	if raw, err := EmbeddedDefault(name); err == nil {
		embedded = string(raw)
	}

	resolved := firstNonEmpty(projectContent, globalContent, embedded)
	if resolved == "" {
		return "", nil
	}
	return resolved, nil
}
