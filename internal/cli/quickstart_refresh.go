package cli

import (
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

type quickstartRefreshItem struct {
	Name    string `json:"name,omitempty"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Managed bool   `json:"managed"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
}

type quickstartRefreshReport struct {
	Agents     quickstartRefreshItem   `json:"agents"`
	Claude     quickstartRefreshItem   `json:"claude"`
	Hooks      quickstartRefreshItem   `json:"hooks"`
	Quickstart []quickstartRefreshItem `json:"quickstart"`
}

func refreshQuickstartManagedAssets(ws workspace.Info) (quickstartRefreshReport, error) {
	// [LAW:single-enforcer] Quickstart refresh reuses the existing managed writers so AGENTS, CLAUDE, and hook updates stay owned at one boundary.
	hookResult, hookErr := installHooks(ws)
	if hookErr != nil {
		return quickstartRefreshReport{}, hookErr
	}
	agentsResult, claudeResult, agentsErr := ensureLinksAgentFiles(ws.RootDir)
	if agentsErr != nil {
		return quickstartRefreshReport{}, agentsErr
	}
	quickstartItems, qsErr := refreshQuickstartTemplates(ws.RootDir)
	if qsErr != nil {
		return quickstartRefreshReport{}, qsErr
	}
	return quickstartRefreshReport{
		Hooks: quickstartHookRefreshItem(hookResult),
		Agents: quickstartRefreshItem{
			Path:    agentsResult.Path,
			Status:  managedAssetStatus(agentsResult.Changed, agentsResult.Created),
			Managed: true,
			Source:  string(agentsResult.Source),
		},
		Claude: quickstartRefreshItem{
			Path:    claudeResult.Path,
			Status:  managedAssetStatus(claudeResult.Changed, claudeResult.Created),
			Managed: true,
			Source:  string(claudeResult.Source),
		},
		Quickstart: quickstartItems,
	}, nil
}

// refreshQuickstartTemplates inspects the active override (project > global) for
// every quickstart guidance template and reports each status without overwriting.
// This is intentionally conservative: an override file exists because the user
// explicitly ejected, so refresh never mutates it. When content matches the
// embedded default, status is "unchanged". When content has drifted, status is
// "skipped" with reason "customized" and the override path is surfaced so the
// user can decide whether it is genuinely customized or a stale verbatim copy
// worth deleting / re-ejecting.
func refreshQuickstartTemplates(workspaceRoot string) ([]quickstartRefreshItem, error) {
	// [LAW:one-type-per-behavior] Every guidance template gets the identical inspection; the set derives from the topic table.
	names := quickstartGuidanceTemplateNames()
	items := make([]quickstartRefreshItem, 0, len(names))
	for _, name := range names {
		item, err := refreshQuickstartTemplate(workspaceRoot, name)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func refreshQuickstartTemplate(workspaceRoot string, name string) (quickstartRefreshItem, error) {
	embedded, err := templates.EmbeddedDefault(name)
	if err != nil {
		return quickstartRefreshItem{}, fmt.Errorf("refresh %s: read embedded default: %w", name, err)
	}
	path, content, err := templates.ActiveOverride(workspaceRoot, name)
	if err != nil {
		return quickstartRefreshItem{}, fmt.Errorf("refresh %s: read override: %w", name, err)
	}
	if path.IsEmpty() {
		return quickstartRefreshItem{
			Name:    name,
			Status:  "absent",
			Managed: false,
		}, nil
	}
	if string(content) == string(embedded) {
		return quickstartRefreshItem{
			Name:    name,
			Path:    path.String(),
			Status:  "unchanged",
			Managed: true,
		}, nil
	}
	return quickstartRefreshItem{
		Name:    name,
		Path:    path.String(),
		Status:  "skipped",
		Managed: true,
		Reason:  "customized",
	}, nil
}

func managedAssetStatus(changed bool, created bool) string {
	statuses := []string{"unchanged", "updated", "created"}
	index := 0
	if changed {
		index = 1
	}
	if created {
		index = 2
	}
	return statuses[index]
}

func formatQuickstartRefreshSummary(refresh quickstartRefreshReport) string {
	items := []labeledStatus{
		{"pre-push hook", refresh.Hooks.Status, refresh.Hooks.Reason},
		{"AGENTS.md", refresh.Agents.Status, composeSourceReason(refresh.Agents.Reason, refresh.Agents.Source, refresh.Agents.Status)},
		{"CLAUDE.md", refresh.Claude.Status, composeSourceReason(refresh.Claude.Reason, refresh.Claude.Source, refresh.Claude.Status)},
	}
	for _, q := range refresh.Quickstart {
		items = append(items, labeledStatus{fmt.Sprintf("%s template", strings.TrimSuffix(q.Name, ".md")), q.Status, q.Reason})
	}

	var updated, skipped, unchanged []string
	for _, item := range items {
		entry := formatLabeledEntry(item)
		switch {
		case item.status == "updated" || item.status == "created":
			updated = append(updated, entry)
		case item.status == "skipped":
			skipped = append(skipped, entry)
		case item.status == "unchanged":
			unchanged = append(unchanged, entry)
		}
	}

	var lines []string
	if len(updated) > 0 {
		lines = append(lines, fmt.Sprintf("  Refreshed: %s", strings.Join(updated, ", ")))
	}
	if len(skipped) > 0 {
		lines = append(lines, fmt.Sprintf("  Skipped: %s", strings.Join(skipped, ", ")))
	}
	if len(unchanged) > 0 {
		lines = append(lines, fmt.Sprintf("  Up to date: %s", strings.Join(unchanged, ", ")))
	}
	if len(lines) == 0 {
		return "  nothing to refresh"
	}
	return strings.Join(lines, "\n")
}

func quickstartHookRefreshItem(result hookInstallResult) quickstartRefreshItem {
	status := managedAssetStatus(result.Changed, false)
	if !result.Managed && result.Reason != "" {
		status = "skipped"
	}
	return quickstartRefreshItem{
		Path:    result.HookPath,
		Status:  status,
		Managed: result.Managed,
		Reason:  result.Reason,
	}
}

// soilSection is the SOIL marker guidance appended to quickstart output when
// quickstart.soil_mode = true in config.toml. Not in the default template because
// it is an opt-in personal convention, not consumer-facing guidance.
const soilSection = `
## Soil
<agent-instructions>When you notice how well — or poorly — this loop was *prepared* for you by what came before (the ticket's clarity, a doc's currency, a skill's presence, a deposit left), record it inline in chat as ` + "`" + `[SOIL:<judgement>]` + "`" + ` in your own words. This is about inherited preparation conditions, not transient in-the-moment friction or your own ongoing thinking — soil, not wind. Don't investigate; just express the judgement and continue. Chat only — never commit messages or code. The corpus is the only signal we have that garden-tending is doing anything; trivially cheap and disproportionately effective.</agent-instructions>`

func renderQuickstartGuidance(workspaceRoot string) (string, error) {
	tmpl, err := templates.Load(templates.QuickstartTemplateName, workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load quickstart guidance: %w", err)
	}
	cfg, err := config.Load(pathspec.New(workspaceRoot))
	if err != nil {
		return "", fmt.Errorf("load config for quickstart guidance: %w", err)
	}
	out := strings.TrimSpace(tmpl)
	if cfg.Quickstart.SoilMode {
		out += soilSection
	}
	return out, nil
}

// renderQuickstartTopic loads one topic guidance template (project > global > embedded).
// Topic output never carries the soil section: SOIL is a session-wide convention
// surfaced once at the session-start router read, not per-task guidance.
func renderQuickstartTopic(workspaceRoot string, name string) (string, error) {
	tmpl, err := templates.Load(name, workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load quickstart topic guidance: %w", err)
	}
	return strings.TrimSpace(tmpl), nil
}
