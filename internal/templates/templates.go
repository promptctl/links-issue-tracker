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
