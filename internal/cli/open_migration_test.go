package cli

import (
	"context"
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
)

// A command whose open migrates the workspace and then cannot roll an old
// migration snapshot off still runs: it exits 0, its output begins with the
// warning, and its event names the snapshot and the failure. The command is a
// read, so this also drives the read open's hand-off to the write open that
// migrates.
func TestACommandWhoseMigratingOpenCannotPruneStillRuns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	isolateEvents(t)
	repo, stuck := workspaceWhosePruneWillFail(t)

	out, err := runCLIInDirAllowError(t, repo, "ls")
	if err != nil {
		t.Fatalf("lit ls error = %v, want it to run\noutput:\n%s", err, out)
	}
	if !strings.HasPrefix(out, "warning: old migration snapshots were not removed: prune migration snapshots: ") {
		t.Errorf("lit ls output = %q, want it to begin with the prune warning", out)
	}
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
	repo, _ := workspaceWhosePruneWillFail(t)

	out, err := runCLIInDirAllowError(t, repo, "stores", "--counts", repo)
	if err != nil {
		t.Fatalf("lit stores --counts error = %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, ": warning: old migration snapshots were not removed: prune migration snapshots: ") {
		t.Errorf("lit stores --counts output = %q, want the prune note", out)
	}
	if got := lastEvent(t).Counts["stores.snapshot_prune_failures"]; got != 1 {
		t.Errorf("counts.stores.snapshot_prune_failures = %d, want 1", got)
	}
}

// workspaceWhosePruneWillFail is a workspace one migration behind this binary,
// holding more old migration snapshots than the store keeps, the oldest of
// which cannot be removed: the next open migrates, and its prune fails on that
// snapshot, which it returns.
func workspaceWhosePruneWillFail(t *testing.T) (repo string, stuck string) {
	t.Helper()
	repo, ws := initBootstrapTestRepo(t)
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
	return repo, stuck
}
