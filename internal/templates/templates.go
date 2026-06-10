package templates

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

const (
	AgentsSectionTemplateName    = "agents-section.md"
	PrePushHookTemplateName      = "pre-push-hook.sh"
	QuickstartTemplateName       = "quickstart.md"
	QuickstartReadyTemplateName  = "quickstart-ready.md"
	QuickstartNewTemplateName    = "quickstart-new.md"
	QuickstartUpdateTemplateName = "quickstart-update.md"
	QuickstartDoneTemplateName   = "quickstart-done.md"
	QuickstartDoctorTemplateName = "quickstart-doctor.md"

	guidanceNamePrefix = "guidance-"
)

var (
	//go:embed defaults/*
	defaultsFS embed.FS

	allNames = []string{
		AgentsSectionTemplateName,
		PrePushHookTemplateName,
		QuickstartTemplateName,
		QuickstartReadyTemplateName,
		QuickstartNewTemplateName,
		QuickstartUpdateTemplateName,
		QuickstartDoneTemplateName,
		QuickstartDoctorTemplateName,
	}

	// shortAliases maps user-facing short names (CLI tokens) to canonical filenames.
	// [LAW:one-source-of-truth] CLI/UX mapping for template identity lives here, not spread across commands.
	shortAliases = map[string]string{
		"quickstart":        QuickstartTemplateName,
		"quickstart-ready":  QuickstartReadyTemplateName,
		"quickstart-new":    QuickstartNewTemplateName,
		"quickstart-update": QuickstartUpdateTemplateName,
		"quickstart-done":   QuickstartDoneTemplateName,
		"quickstart-doctor": QuickstartDoctorTemplateName,
		"agents":            AgentsSectionTemplateName,
		"hook":              PrePushHookTemplateName,
	}
)

// Names returns the canonical list of managed template filenames.
func Names() []string {
	out := make([]string, len(allNames))
	copy(out, allNames)
	return out
}

// ResolveShortName returns the canonical filename for a short alias
// (e.g. "quickstart", "agents", "hook"). Returns an error for unknown aliases.
func ResolveShortName(alias string) (string, error) {
	name, ok := shortAliases[strings.TrimSpace(alias)]
	if !ok {
		// [LAW:one-source-of-truth] The valid-alias list in the error is derived from the map, never hand-maintained.
		return "", fmt.Errorf("usage: unknown template %q (must be one of: %s)", alias, strings.Join(sortedAliasNames(), ", "))
	}
	return name, nil
}

func sortedAliasNames() []string {
	names := make([]string, 0, len(shortAliases))
	for alias := range shortAliases {
		names = append(names, alias)
	}
	sort.Strings(names)
	return names
}

// Source describes which layer a resolved template came from.
type Source string

const (
	SourceProject  Source = "project"
	SourceGlobal   Source = "global"
	SourceEmbedded Source = "embedded"
)

// Load resolves a managed template with project > global > embedded precedence.
// It never writes; absence of a file at a given layer simply means that layer
// contributes nothing. The embedded default is always available as the final fallback.
func Load(name string, workspaceRoot string) (string, error) {
	content, _, err := LoadWithSource(name, workspaceRoot)
	return content, err
}

// LoadWithSource is like Load but also returns which layer the content came from.
func LoadWithSource(name string, workspaceRoot string) (string, Source, error) {
	projectContent, projectErr := readOptionalFile(projectTemplatePath(workspaceRoot, name))
	if projectErr != nil {
		return "", "", fmt.Errorf("load project template %s: %w", projectTemplatePath(workspaceRoot, name), projectErr)
	}
	globalContent, globalErr := readOptionalFile(GlobalPath(name))
	if globalErr != nil {
		return "", "", fmt.Errorf("load global template %s: %w", GlobalPath(name), globalErr)
	}
	embedded, err := EmbeddedDefault(name)
	if err != nil {
		return "", "", fmt.Errorf("load embedded template %s: %w", name, err)
	}
	type candidate struct {
		content string
		source  Source
	}
	candidates := []candidate{
		{projectContent, SourceProject},
		{globalContent, SourceGlobal},
		{string(embedded), SourceEmbedded},
	}
	for _, c := range candidates {
		if c.content != "" {
			return c.content, c.source, nil
		}
	}
	return "", "", fmt.Errorf("load template %s: no non-empty source", name)
}

// EmbeddedDefault returns the raw bytes of the embedded default for name.
func EmbeddedDefault(name string) ([]byte, error) {
	return defaultsFS.ReadFile(filepath.Join("defaults", name))
}

// GlobalPath returns the override path in the user's global config directory for name.
// Absent when no global config directory is configured.
func GlobalPath(name string) pathspec.PathSpec {
	return globalTemplatesDir().Join(name)
}

// ProjectPath returns the override path under the workspace's .lit/templates directory.
// Absent when workspaceRoot is empty.
func ProjectPath(workspaceRoot string, name string) pathspec.PathSpec {
	return projectTemplatePath(workspaceRoot, name)
}

// ActiveOverride returns the highest-priority existing override (project > global)
// for name. When neither layer has a file, the returned path is absent and content
// is nil. Filesystem errors other than "not exist" are propagated.
// [LAW:one-source-of-truth] This helper resolves only the project/global override
// layers; callers that need to know which override layer was selected can infer it
// from the returned path.
func ActiveOverride(workspaceRoot string, name string) (path pathspec.PathSpec, content []byte, err error) {
	// [LAW:dataflow-not-control-flow] Inspect both layers in fixed order; presence/absence is data, not branching.
	candidatePaths := []pathspec.PathSpec{
		projectTemplatePath(workspaceRoot, name),
		GlobalPath(name),
	}
	for _, p := range candidatePaths {
		if p.IsEmpty() {
			continue
		}
		raw, readErr := os.ReadFile(p.String())
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return pathspec.PathSpec{}, nil, readErr
		}
		return p, raw, nil
	}
	return pathspec.PathSpec{}, nil, nil
}

func readOptionalFile(path pathspec.PathSpec) (string, error) {
	if path.IsEmpty() {
		return "", nil
	}
	content, err := os.ReadFile(path.String())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return string(content), nil
}

func projectTemplatePath(workspaceRoot string, name string) pathspec.PathSpec {
	return pathspec.New(workspaceRoot).Join(".lit", "templates", name)
}

func globalTemplatesDir() pathspec.PathSpec {
	// [LAW:one-source-of-truth] Global template storage reuses config.ConfigDir as the canonical root.
	return pathspec.New(config.ConfigDir()).Join("templates")
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
