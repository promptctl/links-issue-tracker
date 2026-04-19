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
	Agents quickstartRefreshItem `json:"agents"`
	Hooks  quickstartRefreshItem `json:"hooks"`
}

func refreshQuickstartManagedAssets(ws workspace.Info) (quickstartRefreshReport, error) {
	// [LAW:single-enforcer] Quickstart refresh reuses the existing managed writers so AGENTS and hook updates stay owned at one boundary.
	hookResult, hookErr := installHooks(ws)
	if hookErr != nil {
		return quickstartRefreshReport{}, hookErr
	}
	agentsResult, agentsErr := ensureLinksAgentsSection(ws.RootDir)
	if agentsErr != nil {
		return quickstartRefreshReport{}, agentsErr
	}
	return quickstartRefreshReport{
		Hooks: quickstartHookRefreshItem(hookResult),
		Agents: quickstartRefreshItem{
			Path:    agentsResult.Path,
			Status:  managedAssetStatus(agentsResult.Changed, agentsResult.Created),
			Managed: true,
		},
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
	return fmt.Sprintf("hooks=%s agents=%s", formatQuickstartRefreshItemSummary(refresh.Hooks), formatQuickstartRefreshItemSummary(refresh.Agents))
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

func formatQuickstartRefreshItemSummary(item quickstartRefreshItem) string {
	if item.Reason == "" {
		return item.Status
	}
	return fmt.Sprintf("%s(%s)", item.Status, item.Reason)
}

func renderQuickstartGuidance(workspaceRoot string) (string, error) {
	template, err := templates.Load(templates.QuickstartTemplateName, workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("load quickstart guidance: %w", err)
	}
	return strings.TrimSpace(template), nil
}
