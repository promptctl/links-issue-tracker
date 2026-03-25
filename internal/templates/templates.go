package templates

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bmf/links-issue-tracker/internal/config"
)

const (
	AgentsSectionTemplateName = "agents-section.md"
	PrePushHookTemplateName   = "pre-push-hook.sh"
)

var (
	//go:embed defaults/*
	defaultsFS embed.FS

	defaultTemplateNames = []string{
		AgentsSectionTemplateName,
		PrePushHookTemplateName,
	}
)

// Load resolves a managed template with project > global precedence.
func Load(name string, workspaceRoot string) (string, error) {
	if err := ensureGlobalTemplate(name); err != nil {
		return "", err
	}

	projectPath := projectTemplatePath(workspaceRoot, name)
	globalPath := globalTemplatePath(name)

	// [LAW:dataflow-not-control-flow] Sources are read in a fixed order on every call; only values decide precedence.
	projectContent, projectErr := readOptionalFile(projectPath)
	globalContent, globalErr := readOptionalFile(globalPath)

	resolved := firstNonEmpty(projectContent, globalContent)

	if projectErr != nil {
		return "", fmt.Errorf("load project template %s: %w", projectPath, projectErr)
	}
	if globalErr != nil {
		return "", fmt.Errorf("load global template %s: %w", globalPath, globalErr)
	}
	if resolved == "" {
		return "", fmt.Errorf("load template %s: no non-empty source", name)
	}
	return resolved, nil
}

// SeedGlobalDefaults writes embedded defaults into the global template directory.
func SeedGlobalDefaults() error {
	templatesDir := globalTemplatesDir()
	if strings.TrimSpace(templatesDir) == "" {
		return nil
	}
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		return fmt.Errorf("create templates dir %s: %w", templatesDir, err)
	}
	for _, name := range defaultTemplateNames {
		content, err := readEmbedded(name)
		if err != nil {
			return fmt.Errorf("read embedded template %s: %w", name, err)
		}
		path := filepath.Join(templatesDir, name)
		if err := writeFileIfMissing(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("seed template %s: %w", path, err)
		}
	}
	return nil
}

func ensureGlobalTemplate(name string) error {
	globalPath := globalTemplatePath(name)
	globalContent, globalErr := readOptionalBytes(globalPath)
	if globalErr == nil && isValidTemplateBytes(globalContent) {
		return nil
	}

	// [LAW:single-enforcer] Global template health checks (missing/invalid -> reset) are enforced once here.
	if err := resetGlobalTemplate(name); err != nil {
		return err
	}
	return nil
}

func resetGlobalTemplate(name string) error {
	defaultContent, err := readEmbeddedBytes(name)
	if err != nil {
		return fmt.Errorf("read embedded template %s: %w", name, err)
	}

	path := globalTemplatePath(name)
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("could not write template %s to global config directory: empty global template path", name)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("could not write template %s to %s: %w", name, path, err)
	}
	if err := os.WriteFile(path, defaultContent, 0o644); err != nil {
		return fmt.Errorf("could not write template %s to %s: %w", name, path, err)
	}
	return nil
}

func readOptionalFile(path string) (string, error) {
	content, err := readOptionalBytes(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func readOptionalBytes(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return content, nil
}

func isValidTemplateBytes(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	if !utf8.Valid(content) {
		return false
	}
	if bytes.IndexByte(content, 0x00) >= 0 {
		return false
	}
	return true
}

func readEmbedded(name string) (string, error) {
	content, err := readEmbeddedBytes(name)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func readEmbeddedBytes(name string) ([]byte, error) {
	content, err := defaultsFS.ReadFile(filepath.Join("defaults", name))
	if err != nil {
		return nil, err
	}
	return content, nil
}

func writeFileIfMissing(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
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

func globalTemplatePath(name string) string {
	dir := globalTemplatesDir()
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
