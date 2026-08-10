package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// readSyncTraceRecords reads every record recordSyncTrace has written for ws,
// in filename (== chronological) order — trace.Write's timestamp-prefixed
// names sort that way. A missing directory (nothing recorded yet) reads as
// zero records, not an error.
func readSyncTraceRecords(t *testing.T, ws workspace.Info) []syncTraceRecord {
	t.Helper()
	dir := syncTraceDir(ws)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%q) error = %v", dir, err)
	}
	records := make([]syncTraceRecord, 0, len(entries))
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", entry.Name(), err)
		}
		var record syncTraceRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			t.Fatalf("json.Unmarshal(%q) error = %v", entry.Name(), err)
		}
		records = append(records, record)
	}
	return records
}

// TestRecordSyncTraceWritesCanonicalJSONUnconditionally is the direct proof of
// the ticket's mechanism: with NO LNKS_AUTOMATION_TRIGGER set — the interactive
// case maybeRecordAutomatedCommandTrace silently no-ops on — recordSyncTrace
// still writes a durable record, under the "sync" kind alongside "automation"
// and "workflows", with an empty Trigger naming that no automation fired it.
func TestRecordSyncTraceWritesCanonicalJSONUnconditionally(t *testing.T) {
	if v := os.Getenv(automationTriggerEnvVar); v != "" {
		t.Fatalf("test environment unexpectedly has %s=%q set", automationTriggerEnvVar, v)
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	path, err := recordSyncTrace(ws, syncTraceRecord{
		Command:   "lit sync fetch",
		Decision:  "fetched",
		Status:    "ok",
		BuildNote: "build: TEST-SENTINEL",
		Metadata:  map[string]string{"remote": "origin"},
	})
	if err != nil {
		t.Fatalf("recordSyncTrace() error = %v", err)
	}
	if filepath.Dir(path) != syncTraceDir(ws) {
		t.Fatalf("trace directory = %q, want %q", filepath.Dir(path), syncTraceDir(ws))
	}

	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(trace) error = %v", err)
	}
	var record syncTraceRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("json.Unmarshal(trace) error = %v", err)
	}
	if record.WorkspaceID != ws.WorkspaceID {
		t.Fatalf("workspace_id = %q, want %q", record.WorkspaceID, ws.WorkspaceID)
	}
	if record.Command != "lit sync fetch" {
		t.Fatalf("command = %q, want %q", record.Command, "lit sync fetch")
	}
	if record.Decision != "fetched" {
		t.Fatalf("decision = %q, want fetched", record.Decision)
	}
	if record.Trigger != "" {
		t.Fatalf("trigger = %q, want empty (no automation trigger set)", record.Trigger)
	}
	if record.Metadata["remote"] != "origin" {
		t.Fatalf("metadata[remote] = %q, want origin", record.Metadata["remote"])
	}
}

// TestRecordSyncTraceCarriesTriggerWhenAutomated proves the same record kind
// distinguishes an automated occasion from an interactive one via Trigger,
// rather than needing a second write — LNKS_AUTOMATION_TRIGGER is read fresh
// from the environment automationContext already reads, so this trace kind and
// the existing automation-gated one can never disagree about what fired an
// occasion.
func TestRecordSyncTraceCarriesTriggerWhenAutomated(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	t.Setenv(automationTriggerEnvVar, "git-pre-push")

	path, err := recordSyncTrace(ws, syncTraceRecord{Command: "lit sync push", Decision: "pushed", Status: "ok"})
	if err != nil {
		t.Fatalf("recordSyncTrace() error = %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(trace) error = %v", err)
	}
	var record syncTraceRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatalf("json.Unmarshal(trace) error = %v", err)
	}
	if record.Trigger != "git-pre-push" {
		t.Fatalf("trigger = %q, want git-pre-push", record.Trigger)
	}
}

// TestInitLeavesDurableSyncTraceInteractively is the end-to-end proof for
// init's remote-adopt outcome, the ticket's first named acceptance case: a
// directly-run `lit init`, with no automation trigger set, leaves a durable
// JSON trace record — where before this ticket, init_sync.go's adopt step
// reported its outcome only through progressf (stderr, never persisted).
func TestInitLeavesDurableSyncTraceInteractively(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runCLIInDir(t, repo, "init", "--skip-hooks", "--skip-agents")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	records := readSyncTraceRecords(t, ws)
	if len(records) != 1 {
		t.Fatalf("sync trace records for a bare `lit init` = %d, want exactly 1: %+v", len(records), records)
	}
	rec := records[0]
	if rec.Command != "lit init" {
		t.Fatalf("command = %q, want %q", rec.Command, "lit init")
	}
	if rec.Decision != string(initSyncNotConfigured) {
		t.Fatalf("decision = %q, want %q (no git remote in this repo)", rec.Decision, initSyncNotConfigured)
	}
	if rec.Status != "ok" {
		t.Fatalf("status = %q, want ok", rec.Status)
	}
	if rec.Trigger != "" {
		t.Fatalf("trigger = %q, want empty — this init was run directly, not under automation", rec.Trigger)
	}
	if !strings.HasPrefix(rec.BuildNote, "build:") {
		t.Fatalf("build_note = %q, want a resolved build status", rec.BuildNote)
	}
}

// TestExplicitSyncCommandsLeaveDurableTracesInteractively is the end-to-end
// proof for the ticket's second named acceptance case: `lit sync
// fetch/pull/push/reconcile`, run directly with no automation trigger set,
// each leave a durable trace record — the exact commands
// internal/cli/sync.go, sync_receive.go, and sync_bg.go's existing
// LNKS_AUTOMATION_TRIGGER-gated writer left no record for at all when run this
// way. It drives the real CLI over a real git remote (mirroring
// TestInitAdoptsExistingRemoteBacklog's fixture) and inspects the durable
// trace directory afterward, not just the commands' own exit codes.
func TestExplicitSyncCommandsLeaveDurableTracesInteractively(t *testing.T) {
	if v := os.Getenv(automationTriggerEnvVar); v != "" {
		t.Fatalf("test environment unexpectedly has %s=%q set", automationTriggerEnvVar, v)
	}
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
	runCLIInDir(t, producer, "new", "--title", "seed-ticket", "--topic", "demo", "--type", "task")
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
	ws, err := workspace.Resolve(consumer)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	// init already recorded its own trace; the four explicit commands below are
	// this test's subject, so start counting fresh from here.
	initRecordCount := len(readSyncTraceRecords(t, ws))

	runCLIInDir(t, consumer, "sync", "fetch")
	runCLIInDir(t, consumer, "sync", "pull")
	runCLIInDir(t, consumer, "sync", "push")
	runCLIInDir(t, consumer, "sync", "reconcile")

	records := readSyncTraceRecords(t, ws)[initRecordCount:]
	wantCommands := []string{"lit sync fetch", "lit sync pull", "lit sync push --remote origin", "lit sync reconcile"}
	if len(records) != len(wantCommands) {
		t.Fatalf("sync trace records after fetch/pull/push/reconcile = %d, want %d: %+v", len(records), len(wantCommands), records)
	}
	for i, rec := range records {
		if rec.Command != wantCommands[i] {
			t.Errorf("record %d command = %q, want %q", i, rec.Command, wantCommands[i])
		}
		if rec.Trigger != "" {
			t.Errorf("record %d (%s) trigger = %q, want empty — every one of these ran directly, not under automation", i, rec.Command, rec.Trigger)
		}
		if rec.Status != "ok" {
			t.Errorf("record %d (%s) status = %q, want ok: %+v", i, rec.Command, rec.Status, rec)
		}
	}
	if records[3].Decision != string(store.SyncReconcileNotDiverged) {
		t.Errorf("reconcile decision = %q, want %q (consumer just adopted; nothing has diverged)", records[3].Decision, store.SyncReconcileNotDiverged)
	}
	// Regression guard: the durable push record must not carry the
	// automation-trace's canned "managed automation requested sync push" —
	// this run was a human/agent typing `lit sync push` directly, and a durable
	// record that claims automation requested it would be a lie in the audit
	// trail this ticket exists to create. [LAW:no-silent-failure]
	pushRecord := records[2]
	if pushRecord.Reason != "" {
		t.Errorf("interactive push record reason = %q, want empty (not the automation-only canned reason)", pushRecord.Reason)
	}
	// reconcile's Reason should be populated even on a plain "ok" decision —
	// via reconcileCommandReasonForState, not reconcileReasonForState: this
	// record comes from an explicit `lit sync reconcile`, and
	// reconcileReasonForState's phrasing ("automatic reconcile...") belongs to
	// the inline auto-reconcile path alone, never this one.
	reconcileRecord := records[3]
	if reconcileRecord.Reason != reconcileCommandReasonForState(store.SyncReconcileNotDiverged) {
		t.Errorf("reconcile record reason = %q, want %q", reconcileRecord.Reason, reconcileCommandReasonForState(store.SyncReconcileNotDiverged))
	}
	if strings.Contains(reconcileRecord.Reason, "automatic reconcile") {
		t.Errorf("explicit `lit sync reconcile`'s trace reason falsely attributes itself to automation: %q", reconcileRecord.Reason)
	}
	// Every command that resolved a sync branch must spell the metadata key the
	// same way — "sync_branch" — so a reader filtering traces/sync/ by that key
	// sees every command's records, not a subset that happened to use "branch".
	for _, rec := range records {
		if _, hasWrongKey := rec.Metadata["branch"]; hasWrongKey {
			t.Errorf("record %q used metadata key \"branch\" instead of the shared \"sync_branch\": %+v", rec.Command, rec.Metadata)
		}
	}
}

// TestReconcileAbortLeavesDurableTraceInteractively proves `lit sync reconcile
// abort` — the one reconcile subcommand that got no trace instrumentation in
// this ticket's first pass, caught by review — leaves a durable record like
// its resolve/take/combine siblings.
func TestReconcileAbortLeavesDurableTraceInteractively(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	runCLIInDir(t, repo, "init", "--skip-hooks", "--skip-agents")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	before := len(readSyncTraceRecords(t, ws))

	runCLIInDir(t, repo, "sync", "reconcile", "abort")

	records := readSyncTraceRecords(t, ws)[before:]
	if len(records) != 1 {
		t.Fatalf("sync trace records after `sync reconcile abort` = %d, want exactly 1: %+v", len(records), records)
	}
	rec := records[0]
	if rec.Command != "lit sync reconcile abort" {
		t.Fatalf("command = %q, want %q", rec.Command, "lit sync reconcile abort")
	}
	if rec.Trigger != "" {
		t.Fatalf("trigger = %q, want empty — run directly, not under automation", rec.Trigger)
	}
	if rec.Status != "ok" {
		t.Fatalf("status = %q, want ok", rec.Status)
	}
}

// TestReconcileTakeAndCombineLeaveDurableTracesInteractively is the trace
// counterpart to sync_unrelated_e2e_test.go's TestReconcileCombineUnionsUnrelatedHistories:
// `lit sync reconcile take` and `lit sync reconcile combine` are the two
// resolution commands new alongside this ticket's tracing work, and — unlike
// TestExplicitSyncCommandsLeaveDurableTracesInteractively's plain "sync
// reconcile" on a non-diverged clone — only fire their TookRemote/TookLocal/
// Combined trace branches against a real unrelated-histories divergence, so
// this drives the same disjoint-init fixture those e2e tests already use and
// inspects the actual trace files on disk (not just stdout/exit code, which
// sync_unrelated_test.go's direct reportTakeOutcome/reportReconcileResult
// calls already cover).
func TestReconcileTakeAndCombineLeaveDurableTracesInteractively(t *testing.T) {
	t.Setenv(DisableAutoSyncEnvVar, "1")

	newDisjointConsumer := func(t *testing.T) (consumer string, ws workspace.Info) {
		t.Helper()
		base := t.TempDir()
		runGit(t, base, "init", "--bare", "remote.git")
		remote := filepath.Join(base, "remote.git")

		producer := filepath.Join(base, "alpha")
		runGit(t, base, "clone", remote, "alpha")
		runGit(t, producer, "config", "user.email", "a@a.co")
		runGit(t, producer, "config", "user.name", "alpha")
		if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
			t.Fatalf("write readme error = %v", err)
		}
		runGit(t, producer, "add", "-A")
		runGit(t, producer, "commit", "-m", "seed")
		runGit(t, producer, "push", "origin", "HEAD")
		runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")
		runCLIInDir(t, producer, "new", "--title", "producer-ticket", "--topic", "demo", "--type", "task")

		consumer = filepath.Join(base, "bravo")
		runGit(t, base, "clone", remote, "bravo")
		runGit(t, consumer, "config", "user.email", "b@b.co")
		runGit(t, consumer, "config", "user.name", "bravo")
		runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")
		runCLIInDir(t, consumer, "new", "--title", "consumer-ticket", "--topic", "demo", "--type", "task")

		// Producer publishes last, so the consumer's independently-inited
		// history and the producer's are disjoint by the time consumer reconciles.
		runCLIInDir(t, producer, "sync", "push", "--set-upstream")

		resolvedWs, err := workspace.Resolve(consumer)
		if err != nil {
			t.Fatalf("workspace.Resolve() error = %v", err)
		}
		return consumer, resolvedWs
	}

	t.Run("take remote", func(t *testing.T) {
		consumer, ws := newDisjointConsumer(t)
		before := len(readSyncTraceRecords(t, ws))

		out, err := runCLIInDirErr(t, consumer, "sync", "reconcile", "take", "remote")
		if err != nil {
			t.Fatalf("`sync reconcile take remote` errored: %v\n%s", err, out)
		}

		records := readSyncTraceRecords(t, ws)[before:]
		if len(records) != 1 {
			t.Fatalf("sync trace records after `sync reconcile take remote` = %d, want exactly 1: %+v", len(records), records)
		}
		rec := records[0]
		if rec.Command != "lit sync reconcile take remote" {
			t.Fatalf("command = %q, want %q", rec.Command, "lit sync reconcile take remote")
		}
		if rec.Decision != string(store.SyncReconcileTookRemote) {
			t.Fatalf("decision = %q, want %q", rec.Decision, store.SyncReconcileTookRemote)
		}
		if rec.Trigger != "" {
			t.Fatalf("trigger = %q, want empty — run directly, not under automation", rec.Trigger)
		}
		if rec.Status != "ok" {
			t.Fatalf("status = %q, want ok: %+v", rec.Status, rec)
		}
	})

	t.Run("combine", func(t *testing.T) {
		consumer, ws := newDisjointConsumer(t)
		before := len(readSyncTraceRecords(t, ws))

		out, err := runCLIInDirErr(t, consumer, "sync", "reconcile", "combine")
		if err != nil {
			t.Fatalf("`sync reconcile combine` errored: %v\n%s", err, out)
		}

		records := readSyncTraceRecords(t, ws)[before:]
		if len(records) != 1 {
			t.Fatalf("sync trace records after `sync reconcile combine` = %d, want exactly 1: %+v", len(records), records)
		}
		rec := records[0]
		if rec.Command != reconcileCombineCommand {
			t.Fatalf("command = %q, want %q", rec.Command, reconcileCombineCommand)
		}
		if rec.Decision != string(store.SyncReconcileCombined) {
			t.Fatalf("decision = %q, want %q", rec.Decision, store.SyncReconcileCombined)
		}
		if rec.Trigger != "" {
			t.Fatalf("trigger = %q, want empty — run directly, not under automation", rec.Trigger)
		}
		if rec.Status != "ok" {
			t.Fatalf("status = %q, want ok: %+v", rec.Status, rec)
		}
	})
}

// TestRecordSyncCommandTraceErrorPath is the direct unit-test counterpart to
// the end-to-end coverage above: every recordSyncCommandTrace call site across
// this package relies on its error-path translation (a non-nil err always
// forces decision/status to "error" and Reason to err.Error(), regardless of
// the caller-supplied decision label), so that translation gets its own
// focused test rather than only being exercised incidentally through
// happy-path e2e runs.
func TestRecordSyncCommandTraceErrorPath(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}

	cause := fmt.Errorf("simulated backend failure")
	recordSyncCommandTrace(ws, "lit sync fetch", "fetched", cause, map[string]string{"remote": "origin"})

	records := readSyncTraceRecords(t, ws)
	if len(records) != 1 {
		t.Fatalf("sync trace records = %d, want exactly 1: %+v", len(records), records)
	}
	rec := records[0]
	if rec.Decision != "error" {
		t.Fatalf("decision = %q, want %q — the caller-supplied \"fetched\" must not survive a non-nil err", rec.Decision, "error")
	}
	if rec.Status != "error" {
		t.Fatalf("status = %q, want error", rec.Status)
	}
	if rec.Reason != cause.Error() {
		t.Fatalf("reason = %q, want %q", rec.Reason, cause.Error())
	}
	if rec.Metadata["remote"] != "origin" {
		t.Fatalf("metadata[remote] = %q, want origin — the caller's metadata must still pass through on the error path", rec.Metadata["remote"])
	}
}
