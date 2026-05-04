package cli

import (
	"fmt"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/templates"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

type quickstartRefreshItem struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Managed bool   `json:"managed"`
	Reason  string `json:"reason,omitempty"`
}

type quickstartRefreshReport struct {
	Agents     quickstartRefreshItem `json:"agents"`
	Claude     quickstartRefreshItem `json:"claude"`
	Hooks      quickstartRefreshItem `json:"hooks"`
	Quickstart quickstartRefreshItem `json:"quickstart"`
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
	quickstartItem, qsErr := refreshQuickstartTemplate(ws.RootDir)
	if qsErr != nil {
		return quickstartRefreshReport{}, qsErr
	}
	return quickstartRefreshReport{
		Hooks: quickstartHookRefreshItem(hookResult),
		Agents: quickstartRefreshItem{
			Path:    agentsResult.Path,
			Status:  managedAssetStatus(agentsResult.Changed, agentsResult.Created),
			Managed: true,
		},
		Claude: quickstartRefreshItem{
			Path:    claudeResult.Path,
			Status:  managedAssetStatus(claudeResult.Changed, claudeResult.Created),
			Managed: true,
		},
		Quickstart: quickstartItem,
	}, nil
}

// refreshQuickstartTemplate inspects the active quickstart.md override (project > global)
// and reports its status without overwriting it. This is intentionally conservative:
// the override file exists because the user explicitly ejected, so refresh never
// mutates it. When content matches the embedded default, status is "unchanged".
// When content has drifted, status is "skipped" with reason "customized" and the
// override path is surfaced so the user can decide whether it is genuinely customized
// or a stale verbatim copy worth deleting / re-ejecting.
func refreshQuickstartTemplate(workspaceRoot string) (quickstartRefreshItem, error) {
	embedded, err := templates.EmbeddedDefault(templates.QuickstartTemplateName)
	if err != nil {
		return quickstartRefreshItem{}, fmt.Errorf("refresh quickstart: read embedded default: %w", err)
	}
	path, content, _, err := templates.ActiveOverride(workspaceRoot, templates.QuickstartTemplateName)
	if err != nil {
		return quickstartRefreshItem{}, fmt.Errorf("refresh quickstart: read override: %w", err)
	}
	if path == "" {
		return quickstartRefreshItem{
			Status:  "absent",
			Managed: false,
		}, nil
	}
	if string(content) == string(embedded) {
		return quickstartRefreshItem{
			Path:    path,
			Status:  "unchanged",
			Managed: true,
		}, nil
	}
	return quickstartRefreshItem{
		Path:    path,
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
		{"AGENTS.md", refresh.Agents.Status, refresh.Agents.Reason},
		{"CLAUDE.md", refresh.Claude.Status, refresh.Claude.Reason},
		{"quickstart template", refresh.Quickstart.Status, refresh.Quickstart.Reason},
	}

	var updated, skipped, unchanged []string
	for _, item := range items {
		switch {
		case item.status == "updated" || item.status == "created":
			updated = append(updated, item.label)
		case item.status == "skipped":
			entry := item.label
			if item.reason != "" {
				entry += fmt.Sprintf(" (%s)", item.reason)
			}
			skipped = append(skipped, entry)
		case item.status == "unchanged":
			unchanged = append(unchanged, item.label)
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

func renderQuickstartGuidance(workspaceRoot string) (string, error) {
	template, err := templates.Load(templates.QuickstartTemplateName, workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load quickstart guidance: %w", err)
	}
	return strings.TrimSpace(template), nil
}
