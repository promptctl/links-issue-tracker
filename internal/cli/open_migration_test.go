package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// A command whose open migrates the workspace and then cannot roll an old
// migration snapshot off still runs: it exits 0, its stdout is its own output
// alone, the warning is on stderr, and its event names the snapshot and the
// failure. The command is a read, so this also drives the read open's hand-off
// to the write open that migrates.
func TestACommandWhoseMigratingOpenCannotPruneStillRuns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	isolateEvents(t)
	repo, _, stuck := workspaceWhosePruneWillFail(t)

	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = runCLIInDirAllowError(t, repo, "export") })
	if err != nil {
		t.Fatalf("lit export error = %v, want it to run\noutput:\n%s", err, out)
	}
	var export map[string]any
	if jerr := json.Unmarshal([]byte(out), &export); jerr != nil {
		t.Errorf("lit export stdout is not the export alone: %v\n%s", jerr, out)
	}
	if !strings.HasPrefix(stderr, "warning: old migration snapshots were not removed: prune migration snapshots: ") {
		t.Errorf("stderr = %q, want it to begin with the prune warning", stderr)
	}
	assertMigrationPruneFailureRecorded(t, stuck)
}

// lit init's adopt gate counts the local tickets through a read open, which
// migrates a store behind this binary: the gate records that open's migration
// and warns of a failed prune like any other open.
func TestInitsAdoptGateReportsItsOpensMigration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	isolateEvents(t)
	repo, _, stuck := workspaceWhosePruneWillFail(t)

	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = runCLIInDirAllowError(t, repo, "init") })
	if err != nil {
		t.Fatalf("lit init error = %v, want it to run\noutput:\n%s", err, out)
	}
	if !strings.Contains(stderr, "warning: old migration snapshots were not removed: prune migration snapshots: ") {
		t.Errorf("stderr = %q, want the prune warning", stderr)
	}
	assertMigrationPruneFailureRecorded(t, stuck)
}

// assertMigrationPruneFailureRecorded is the last command's event naming the
// snapshot its open took and the prune that then failed on stuck.
func assertMigrationPruneFailureRecorded(t *testing.T, stuck string) {
	t.Helper()
	ev := lastEvent(t)
	if ev.Outcome != telemetry.OutcomeOK {
		t.Errorf("event outcome = %q, want ok", ev.Outcome)
	}
	if snap, _ := ev.Facts["migration.snapshot"].(string); !strings.Contains(snap, "-pre-migrate-") || snap == filepath.Base(stuck) {
		t.Errorf("migration.snapshot = %v, want the snapshot the open took", ev.Facts["migration.snapshot"])
	}
	if pruneErr, _ := ev.Facts["migration.snapshot_prune_error"].(string); !strings.HasPrefix(pruneErr, "prune migration snapshots: ") {
		t.Errorf("migration.snapshot_prune_error = %v, want the prune failure", ev.Facts["migration.snapshot_prune_error"])
	}
}

// The rollup opens every store it finds, so its event counts the stores whose
// open migrated and then could not prune, and its output notes each.
func TestTheRollupCountsStoresWhoseMigratingOpenCannotPrune(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	isolateEvents(t)
	repo, ws, _ := workspaceWhosePruneWillFail(t)

	out, err := runCLIInDirAllowError(t, repo, "stores", "--counts", repo)
	if err != nil {
		t.Fatalf("lit stores --counts error = %v\noutput:\n%s", err, out)
	}
	if want := "~ " + ws.StorageDir + ": warning: old migration snapshots were not removed: prune migration snapshots: "; !strings.Contains(out, want) {
		t.Errorf("lit stores --counts output = %q, want the prune note %q", out, want)
	}
	counts := lastEvent(t).Counts
	for key, want := range map[string]int{"stores.migrated": 1, "stores.snapshot_prune_failures": 1} {
		if counts[key] != want {
			t.Errorf("counts.%s = %d, want %d", key, counts[key], want)
		}
	}
}

// workspaceWhosePruneWillFail is a workspace one migration behind this binary,
// holding more old migration snapshots than the store keeps, the oldest of
// which cannot be removed: the next open migrates, and its prune fails on that
// snapshot. It returns the repo, its workspace, and that snapshot.
func workspaceWhosePruneWillFail(t *testing.T) (repo string, ws workspace.Info, stuck string) {
	t.Helper()
	repo, ws = initBootstrapTestRepo(t)
	info, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ctx := context.Background()
	st, err := engine.Open(ctx, engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		t.Fatalf("schema migration: %v", err)
	}
	if _, err := migrator.Downgrade(ctx, info.Schema.Max-1); err != nil {
		t.Fatalf("Downgrade() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	// More than the store's retention budget of ten.
	const planted = 12
	dir := dbsnapshot.DirFor(ws.DatabasePath)
	const base = int64(1700000000000000000)
	stuck = filepath.Join(dir, fmt.Sprintf("%d-pre-migrate-%d", base, base))
	for i := int64(0); i < planted; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d-pre-migrate-%d", base+i, base+i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("plant snapshot: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "held"), nil, 0o644); err != nil {
			t.Fatalf("plant snapshot file: %v", err)
		}
	}
	if err := os.Chmod(stuck, 0o555); err != nil {
		t.Fatalf("chmod stuck snapshot: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o755) })
	return repo, ws, stuck
}
