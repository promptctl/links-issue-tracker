package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

type initReport struct {
	Status       string              `json:"status"`
	WorkspaceID  string              `json:"workspace_id"`
	DatabasePath string              `json:"database_path"`
	Migrated     bool                `json:"migrated"`
	Migration    *migrateBeadsReport `json:"migration,omitempty"`
	Hooks        string              `json:"hooks"`
	Agents       string              `json:"agents"`
}

func runInit(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	skipHooks := fs.Bool("skip-hooks", false, "Skip git hook installation")
	skipAgents := fs.Bool("skip-agents", false, "Skip AGENTS.md integration update")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lnks init [--json] [--skip-hooks] [--skip-agents]")
	}

	if _, err := doltcli.RequireMinimumVersion(ctx, ws.RootDir, doltcli.MinSupportedVersion); err != nil {
		return err
	}
	if err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID); err != nil {
		return err
	}

	scan, err := scanBeadsResidue(ws)
	if err != nil {
		return err
	}

	report := initReport{
		Status:       "initialized",
		WorkspaceID:  ws.WorkspaceID,
		DatabasePath: ws.DatabasePath,
		Migrated:     false,
		Hooks:        "skipped",
		Agents:       "skipped",
	}

	if scan.HasResidue() {
		// [LAW:single-enforcer] init reuses migration engine; cleanup policy is not reimplemented locally.
		migration, migrateErr := migrateBeadsWithOptions(ws, true, migrateApplyOptions{InstallHooks: false, InstallAgents: false}, &scan)
		if migrateErr != nil {
			return migrateErr
		}
		report.Migrated = true
		report.Migration = &migration
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
			"%s workspace=%s db=%s migrated=%t hooks=%s agents=%s\n",
			payload.Status,
			payload.WorkspaceID,
			payload.DatabasePath,
			payload.Migrated,
			payload.Hooks,
			payload.Agents,
		)
		return printErr
	})
}

func shouldBypassBeadsPreflight(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "help", "-h", "--help", "completion", "init":
		return true
	default:
		if args[0] != "migrate" {
			return false
		}
		if len(args) < 2 {
			return false
		}
		return args[1] == "beads"
	}
}

func requireBeadsMigrationPreflight(ws workspace.Info, commandArgs []string) error {
	scan, err := scanBeadsResidue(ws)
	if err != nil {
		return err
	}
	if !scan.HasResidue() {
		return nil
	}
	blockedCommand := formatCommand(commandArgs)
	// [LAW:one-source-of-truth] Startup preflight reuses the shared automation trace record instead of inventing a second trace format.
	traceRef, traceErr := recordAutomationTrace(ws, automationTraceRecord{
		Trigger:    "startup-preflight",
		Command:    blockedCommand,
		SideEffect: "block non-init command execution until beads migration completes",
		Status:     "blocked",
		Reason:     "beads residue detected during startup preflight",
		Metadata: map[string]string{
			"blocked_command":     blockedCommand,
			"remediation_command": "lnks migrate beads --apply --json",
			"residue_summary":     scan.Summary(),
		},
	})
	preflightErr := BeadsMigrationRequiredError{
		Summary:            scan.Summary(),
		Trigger:            "startup-preflight",
		BlockedCommand:     blockedCommand,
		RemediationCommand: "lnks migrate beads --apply --json",
	}
	if traceErr != nil {
		preflightErr.TraceWriteError = traceErr.Error()
		return preflightErr
	}
	preflightErr.TraceRef = traceRef.Path
	return preflightErr
}
