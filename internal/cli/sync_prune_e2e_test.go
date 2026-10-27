package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestAnAutomaticReceivePrunesABranchTheRemoteDeleted drives the ticket's
// whole path over a real git remote: a peer publishes a Dolt branch beside
// the sync branch, this checkout fetches it twice (so it holds the tracking
// ref and its fork point), the peer deletes it on the remote and pushes a
// ticket, and one automatic receive — no fetch run by hand, no --prune
// anywhere — leaves the live store without either ref. The receive's trace
// names what its landing deleted, and its event carries the clone's prune and
// the landing's own record of the sync branch's fork point, which the second
// fetch left at the head the live tracking ref still names.
func TestAnAutomaticReceivePrunesABranchTheRemoteDeleted(t *testing.T) {
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
	runCLIInDir(t, producer, "init", "--skip-hooks")
	epicID := newEpicInDir(t, producer, "shared-epic")
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")
	execOnStore(t, producer, "CALL DOLT_BRANCH('feature')", "CALL DOLT_PUSH('origin', 'feature')")

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks")
	runCLIInDir(t, consumer, "sync", "fetch")
	runCLIInDir(t, consumer, "sync", "fetch")

	execOnStore(t, producer, "CALL DOLT_PUSH('origin', ':feature')")
	runCLIInDir(t, producer, "new", "--title", "after-the-delete", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, producer, "sync", "push")

	ws, err := workspace.Resolve(testEnv(), consumer)
	if err != nil {
		t.Fatalf("resolve workspace %s: %v", consumer, err)
	}
	recorder := telemetry.Begin(telemetry.Event{})
	receiveOnce(telemetry.With(context.Background(), recorder), testStderr(t), testEnv(), ws)

	var landing *syncTraceRecord
	for _, record := range readSyncTraceRecords(t, ws) {
		if record.Command == receiveTraceCommand && record.Metadata["landed"] != "" {
			landing = &record
		}
	}
	if landing == nil {
		t.Fatalf("no receive trace with a landing; traces: %+v", readSyncTraceRecords(t, ws))
	}
	if landing.Metadata["landed_pruned"] != "feature" || landing.Metadata["landed_fork_points_dropped"] != "feature" {
		t.Fatalf("receive trace landed_pruned=%q landed_fork_points_dropped=%q; want feature for both (trace %+v)",
			landing.Metadata["landed_pruned"], landing.Metadata["landed_fork_points_dropped"], landing)
	}
	if landing.Metadata["fork_point"] != string(storage.ForkPointUnchanged) {
		t.Fatalf("receive trace fork_point=%q; want %q (trace %+v)", landing.Metadata["fork_point"], storage.ForkPointUnchanged, landing)
	}
	written := writtenEvent(t, recorder.Finish(invocationEnd(time.Now(), nil, nil)))
	landedForkPoints := written.Occurrences["receive.landed_fork_points"]
	if len(landedForkPoints) != 1 {
		t.Fatalf("receive.landed_fork_points = %#v; want the sync branch's one record", landedForkPoints)
	}
	if value, _ := landedForkPoints[0].(map[string]any); value["fork_point.remote"] != "origin" || value["fork_point.branch"] != "master" ||
		value["fork_point.outcome"] != string(storage.ForkPointUnchanged) || value["fork_point.head"] == "" {
		t.Fatalf("receive.landed_fork_points[0] = %#v; want origin's master unchanged at its head", landedForkPoints[0])
	}
	landedPruned := written.Occurrences["receive.landed_pruned"]
	if len(landedPruned) != 1 {
		t.Fatalf("receive.landed_pruned = %#v; want the one ref the landing deleted", landedPruned)
	}
	if value, _ := landedPruned[0].(map[string]any); value["pruned_ref.branch"] != "feature" || value["pruned_ref.remote"] != "origin" || value["pruned_ref.head"] == "" {
		t.Fatalf("receive.landed_pruned[0] = %#v; want origin's feature at its head", landedPruned[0])
	}
	dropped := written.Occurrences["receive.landed_fork_points_dropped"]
	if len(dropped) != 1 {
		t.Fatalf("receive.landed_fork_points_dropped = %#v; want feature's fork point", dropped)
	}
	if value, _ := dropped[0].(map[string]any); value["fork_point.branch"] != "feature" || value["fork_point.outcome"] != string(storage.ForkPointDropped) {
		t.Fatalf("receive.landed_fork_points_dropped[0] = %#v; want feature's fork point dropped", dropped[0])
	}
	pruned := written.Occurrences["fetch.pruned"]
	if len(pruned) != 1 {
		t.Fatalf("fetch.pruned = %#v; want the clone's one prune", pruned)
	}
	if value, ok := pruned[0].(map[string]any); !ok || value["pruned_ref.branch"] != "feature" || value["pruned_ref.remote"] != "origin" {
		t.Fatalf("fetch.pruned[0] = %#v; want origin's feature", pruned[0])
	}
}

// execOnStore runs each statement on the store of the workspace at dir.
func execOnStore(t *testing.T, dir string, statements ...string) {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("resolve workspace %s: %v", dir, err)
	}
	ctx := context.Background()
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open(%s) error = %v", dir, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close the store of %s: %v", dir, err)
		}
	}()
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	for _, stmt := range statements {
		if err := raw.ExecRawForTest(ctx, stmt); err != nil {
			t.Fatalf("%s on %s: %v", stmt, dir, err)
		}
	}
}
