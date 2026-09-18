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
	IssuePrefix  string          `json:"issue_prefix"`
	DatabasePath string          `json:"database_path"`
	DBCreated    bool            `json:"db_created"`
	Hooks        string          `json:"hooks"`
	Agents       string          `json:"agents"`
	Claude       string          `json:"claude"`
	AgentsSource string          `json:"agents_source,omitempty"`
	ClaudeSource string          `json:"claude_source,omitempty"`
	Sync         initSyncOutcome `json:"sync"`
}

// initLeaf declares `lit init` together with the acquisition it needs, because
// --prefix is an input to CREATING the workspace rather than something the work
// below could apply once it has one: in a repository whose name yields no legal
// prefix, acquiring the workspace is the step that fails. Returning both from
// one declaration keeps the flag's parsed storage in a single place that both
// the acquisition and the work read. [LAW:one-source-of-truth]
func initLeaf() (wsLeaf, wsAcquire) {
	fs := newCobraFlagSet("init").Detail(helpText("init"))
	skipHooks := fs.Bool("skip-hooks", false, "Skip git hook installation")
	skipAgents := fs.Bool("skip-agents", false, "Skip AGENTS.md integration update")
	prefix := fs.String("prefix", "", "Issue ID prefix for a new workspace (default: derived from the repository name)")

	// A flag the caller never typed is the zero request, which is the derivation
	// that has always run. A flag they DID type is minted through the same
	// boundary `lit prefix set` uses, so `--prefix ""` is refused here instead of
	// being demoted to "no flag" and then failing further in with a message
	// telling them to pass the flag they just passed. [LAW:no-silent-failure]
	acquire := func() (workspace.Info, error) {
		// Arity is settled before anything is created. The pipeline acquires
		// BEFORE it runs the leaf's work, and acquiring resolves the workspace,
		// which writes config.json -- so an arity check living in work() runs
		// only after the prefix is already on disk. A `--prefix` typed alongside
		// a bad argument would be persisted by a command that then reports
		// failure, and clearing it needs `lit prefix set` rather than a
		// corrected re-run. The effect must not precede the check that refuses
		// it. [LAW:effects-at-boundaries] [LAW:parse-dont-validate]
		// This is init's ONLY arity check; work() does not repeat it.
		// [LAW:single-enforcer]
		if !fs.Changed("prefix") {
			return resolveWorkspaceFromWD(workspace.PrefixRequest{})
		}
		requested, err := workspace.RequestPrefix(*prefix)
		if err != nil {
			return workspace.Info{}, ValidationError{Message: fmt.Sprintf("invalid --prefix %q: %v", *prefix, err)}
		}
		return resolveWorkspaceFromWD(requested)
	}

	return wsLeaf{fs: fs, usage: initUsage, positionals: 0, work: func(ctx context.Context, stdout io.Writer, ws workspace.Info, positional []string) error {
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
			IssuePrefix:  ws.IssuePrefix.Value(),
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
			report.Agents = managedAssetStatus(agentsResult.Changed, agentsResult.Created)
			report.Claude = managedAssetStatus(claudeResult.Changed, claudeResult.Created)
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
	}}, acquire
}

// initUsage is the one spelling of init's surface. [LAW:one-source-of-truth]
const initUsage = "usage: lit init [--prefix <prefix>] [--skip-hooks] [--skip-agents]"

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
	// Always printed, for every init, because the prefix lit STORED is not
	// always the prefix the caller typed: ConfiguredPrefix slugifies and
	// truncates at PrefixMaxLength, so `--prefix payment_service` becomes
	// `payment-serv`. Reporting it is how the caller learns the real value here
	// rather than from the first issue id — `lit prefix set` already echoes its
	// normalized result, and the two siblings should not differ on that.
	// [LAW:no-silent-failure] input we materially changed is reported, not
	// swallowed. [LAW:dataflow-not-control-flow] one unconditional line.
	if _, err := fmt.Fprintf(w, "  issue_prefix: %s\n", report.IssuePrefix); err != nil {
		return err
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
