package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

type initReport struct {
	Status       string `json:"status"`
	WorkspaceID  string `json:"workspace_id"`
	DatabasePath string `json:"database_path"`
	Hooks        string `json:"hooks"`
	Agents       string `json:"agents"`
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

	if err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID); err != nil {
		return err
	}

	report := initReport{
		Status:       "initialized",
		WorkspaceID:  ws.WorkspaceID,
		DatabasePath: ws.DatabasePath,
		Hooks:        "skipped",
		Agents:       "skipped",
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
		agentsResult, agentsErr := ensureLinksAgentsSection(ws.RootDir)
		if agentsErr != nil {
			return agentsErr
		}
		if agentsResult.Created {
			report.Agents = "created"
		} else if agentsResult.Changed {
			report.Agents = "updated"
		} else {
			report.Agents = "unchanged"
		}
	}

	return printValue(stdout, report, *jsonOut, func(w io.Writer, v any) error {
		payload := v.(initReport)
		_, printErr := fmt.Fprintf(
			w,
			"%s workspace=%s db=%s hooks=%s agents=%s\n",
			payload.Status,
			payload.WorkspaceID,
			payload.DatabasePath,
			payload.Hooks,
			payload.Agents,
		)
		return printErr
	})
}
