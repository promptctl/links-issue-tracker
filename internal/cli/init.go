package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

type initReport struct {
	Status       string `json:"status"`
	WorkspaceID  string `json:"workspace_id"`
	DatabasePath string `json:"database_path"`
	DBCreated    bool   `json:"db_created"`
	Hooks        string `json:"hooks"`
	Agents       string `json:"agents"`
	Claude       string `json:"claude"`
	AgentsSource string `json:"agents_source,omitempty"`
	ClaudeSource string `json:"claude_source,omitempty"`
}

func runInit(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("init")
	jsonOut := fs.Bool("json", false, "Output JSON")
	skipHooks := fs.Bool("skip-hooks", false, "Skip git hook installation")
	skipAgents := fs.Bool("skip-agents", false, "Skip AGENTS.md integration update")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit init [--json] [--skip-hooks] [--skip-agents]")
	}

	dbCreated, err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		return err
	}

	report := initReport{
		Status:       "initialized",
		WorkspaceID:  ws.WorkspaceID,
		DatabasePath: ws.DatabasePath,
		DBCreated:    dbCreated,
		Hooks:        "skipped",
		Agents:       "skipped",
		Claude:       "skipped",
	}

	if !*skipHooks {
		hookResult, hookErr := installHooks(ws)
		if hookErr != nil {
			return hookErr
		}
		if hookResult.Changed {
			report.Hooks = "installed"
		} else {
			report.Hooks = "unchanged"
		}
	}

	if !*skipAgents {
		agentsResult, claudeResult, agentsErr := ensureLinksAgentFiles(ws.RootDir)
		if agentsErr != nil {
			return agentsErr
		}
		report.AgentsSource = string(agentsResult.Source)
		report.ClaudeSource = string(claudeResult.Source)
		if agentsResult.Created {
			report.Agents = "created"
		} else if agentsResult.Changed {
			report.Agents = "updated"
		} else {
			report.Agents = "unchanged"
		}
		if claudeResult.Created {
			report.Claude = "created"
		} else if claudeResult.Changed {
			report.Claude = "updated"
		} else {
			report.Claude = "unchanged"
		}
	}

	return printValue(stdout, report, *jsonOut, func(w io.Writer, v any) error {
		payload := v.(initReport)
		return writeInitHumanOutput(w, payload)
	})
}

type labeledStatus struct {
	label  string
	status string
	reason string
}

func sourceDetail(source string, status string) string {
	return composeSourceReason("", source, status)
}

func composeSourceReason(reason, source, status string) string {
	if source == "" || status == "skipped" {
		return reason
	}
	if reason != "" {
		return reason + ", via " + source
	}
	return "via " + source
}

func formatLabeledEntry(item labeledStatus) string {
	entry := item.label
	if item.reason != "" {
		entry += " (" + item.reason + ")"
	}
	return entry
}

func writeInitHumanOutput(w io.Writer, report initReport) error {
	items := []labeledStatus{
		{"pre-push hook", report.Hooks, ""},
		{"AGENTS.md", report.Agents, sourceDetail(report.AgentsSource, report.Agents)},
		{"CLAUDE.md", report.Claude, sourceDetail(report.ClaudeSource, report.Claude)},
	}

	var updated, skipped, unchanged []string
	for _, item := range items {
		entry := formatLabeledEntry(item)
		switch item.status {
		case "created", "updated", "installed":
			updated = append(updated, entry)
		case "skipped":
			skipped = append(skipped, entry)
		case "unchanged":
			unchanged = append(unchanged, entry)
		}
	}

	if report.DBCreated {
		if _, err := fmt.Fprintf(w, "Initialized lit workspace\n"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "lit workspace already initialized\n"); err != nil {
			return err
		}
	}
	if len(updated) > 0 {
		if _, err := fmt.Fprintf(w, "  Updated: %s\n", strings.Join(updated, ", ")); err != nil {
			return err
		}
	}
	if len(unchanged) > 0 {
		if _, err := fmt.Fprintf(w, "  Up to date: %s\n", strings.Join(unchanged, ", ")); err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		if _, err := fmt.Fprintf(w, "  Skipped: %s\n", strings.Join(skipped, ", ")); err != nil {
			return err
		}
	}
	return nil
}
