package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

type initReport struct {
	Status       string          `json:"status"`
	WorkspaceID  string          `json:"workspace_id"`
	DatabasePath string          `json:"database_path"`
	DBCreated    bool            `json:"db_created"`
	Hooks        string          `json:"hooks"`
	Agents       string          `json:"agents"`
	Claude       string          `json:"claude"`
	AgentsSource string          `json:"agents_source,omitempty"`
	ClaudeSource string          `json:"claude_source,omitempty"`
	Sync         initSyncOutcome `json:"sync"`
}

func runInit(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	fs := newCobraFlagSet("init")
	skipHooks := fs.Bool("skip-hooks", false, "Skip git hook installation")
	skipAgents := fs.Bool("skip-agents", false, "Skip AGENTS.md integration update")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit init [--skip-hooks] [--skip-agents]"}
	}

	// Adopt runs BEFORE creating an empty store: when the remote carries a
	// backlog, adopt clones it directly into the target path, so the path's
	// first on-disk state is the cloned data (a pre-created empty store would
	// poison dolt's in-process chunk-store cache). [LAW:types-are-the-program]
	// adoptRemoteTicketsOnInit owns the whole decision and returns the
	// discriminated outcome; only initSyncAdopted means the store now exists.
	syncOutcome := adoptRemoteTicketsOnInit(ctx, ws)
	recordInitSyncTrace(ws, syncOutcome, time.Now())

	// A failed adopt is a genuinely uncertain result — lit could not confirm
	// whether it is safe to start fresh, whether because reading the local
	// store or resolving/probing the remote itself errored — never a
	// confirmed-empty one like the outcomes below. Falling through to create a
	// fresh store here would risk silently stranding whatever backlog the
	// remote (or the unreadable local store) actually holds behind a problem
	// lit merely failed to diagnose. So init hard-stops before any store
	// exists, surfacing the real underlying failure (already carried in
	// syncOutcome.Error) with no flag or prompt to proceed past it — the
	// caller re-runs `lit init` once the cause is fixed.
	// [LAW:no-silent-failure] [LAW:dataflow-not-control-flow] one sealed
	// discriminant decides both branches; no second "is this bad" check drifts
	// from the one adoptRemoteTicketsOnInit already made.
	if syncOutcome.State == initSyncFailed {
		// The wrapper stays deliberately generic — "confirm the workspace
		// state" rather than "confirm the remote" — because initSyncFailed also
		// covers a local store read failing (store.LocalHasTickets erroring),
		// which has nothing to do with the remote; syncOutcome.Error carries
		// the specific cause either way. buildNote rides along too, same as the
		// adopted/failure lines this replaces: a stale local binary silently
		// missing a landed fix was the suspected root cause of the field
		// incident this epic exists to prevent, so a failure names it without a
		// second `lit version` round trip. [LAW:effects-at-boundaries]
		return fmt.Errorf(
			"could not confirm the workspace state, so init is refusing to create a fresh store: %s (%s)",
			syncOutcome.Error, resolveBuildStatusNote(time.Now()),
		)
	}

	// Every remaining non-adopt outcome (greenfield, local tickets already
	// present, no eligible remote, remote empty, no remote data) leaves the
	// workspace needing a local store; EnsureDatabase is idempotent, so a store
	// that already exists reports created=false. [LAW:dataflow-not-control-flow]
	// the create runs on a single value (did we adopt?), not a scatter of cases.
	dbCreated := true
	if syncOutcome.State != initSyncAdopted {
		created, err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID)
		if err != nil {
			return err
		}
		dbCreated = created
	}

	report := initReport{
		Status:       "initialized",
		WorkspaceID:  ws.WorkspaceID,
		DatabasePath: ws.DatabasePath,
		DBCreated:    dbCreated,
		Hooks:        "skipped",
		Agents:       "skipped",
		Claude:       "skipped",
		Sync:         syncOutcome,
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

	// Resolved for the human output, here at the boundary, and threaded through
	// as a value so writeInitHumanOutput/writeInitSyncLine stay pure renderers
	// over an already-known build status. This is a separate resolution from
	// the one adoptRemoteTicketsBlocking makes for the progress-line
	// announcement — that one covers the "start fresh" decision on the
	// progress channel, this one covers the adopted/failed line here.
	// [LAW:effects-at-boundaries]
	buildNote := resolveBuildStatusNote(time.Now())
	return writeInitHumanOutput(stdout, report, buildNote)
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

func writeInitHumanOutput(w io.Writer, report initReport, buildNote string) error {
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
	if err := writeInitSyncLine(w, report.Sync, buildNote); err != nil {
		return err
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
	if _, err := fmt.Fprintf(w, "  Guidance: `lit workflows` shows the work lifecycle and the guidance active at each point (`lit workflows edit <id-or-point>` to customize)\n"); err != nil {
		return err
	}
	return nil
}
