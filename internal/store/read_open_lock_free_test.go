package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// TestOpenForReadDoesNotWaitOnCommitLock pins links-scale-om3r.k1k's
// contract: a read open answers its schema check with reads alone and never
// queues behind the commit lock. The lock is held for the whole open by a
// stand-in for any writer (a mutation's 70ms, a receive's network round
// trip); the reader's deadline is far shorter than the commit-lock waiter
// budget, so an open that still took the lock would fail here instead of
// serving rows. [LAW:behavior-not-structure] the assertion is the served
// count, not which lock was or was not touched.
func TestOpenForReadDoesNotWaitOnCommitLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "served under a held commit lock", Topic: "scale"}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	release, err := LockCommitPath(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockCommitPath() error = %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release commit lock: %v", err)
		}
	}()

	// 30s is far below the 15-minute commit-lock waiter budget an open that
	// took the lock would sit in, and far above what a loaded box adds to an
	// open that does not, so a deadline here separates the two.
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reader, err := OpenForRead(readCtx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() under a held commit lock error = %v; a read open must not wait on the commit lock", err)
	}
	defer reader.Close()
	count, err := reader.LocalIssueCount(readCtx)
	if err != nil {
		t.Fatalf("LocalIssueCount() under a held commit lock error = %v", err)
	}
	if count != 1 {
		t.Fatalf("LocalIssueCount() = %d, want 1", count)
	}
}

// openOneVersionBehind creates a workspace at registry max and steps it one
// migration back, the state a read open hands to Open, then closes it.
func openOneVersionBehind(t *testing.T, ctx context.Context, doltRoot string) {
	t.Helper()
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	versions, err := registryVersionsDescending()
	if err != nil {
		t.Fatalf("enumerate migration versions: %v", err)
	}
	if len(versions) < 2 {
		t.Fatalf("registry has %d migrations; this test needs a next-lower version to land on", len(versions))
	}
	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	if _, err := provider.DownTo(ctx, versions[1]); err != nil {
		t.Fatalf("DownTo(%d) error = %v", versions[1], err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestOpenForReadBringsTrailingSchemaForwardThroughOpen pins the other half
// of the contract: a read open never applies a migration itself, and a
// workspace one migration behind this binary is still brought forward by a
// read command — through the write open, the one migration boundary — so
// the first `lit backlog` after a binary upgrade serves current rows. A
// trailing schema is never served silently, and never served stale.
func TestOpenForReadBringsTrailingSchemaForwardThroughOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	openOneVersionBehind(t, ctx, doltRoot)

	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() on a trailing schema error = %v; want the workspace brought forward", err)
	}
	applied, err := reader.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if applied != headVersion(t) {
		t.Fatalf("recordedMigrationVersion() = %d after a read open, want registry max %d", applied, headVersion(t))
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader Close() error = %v", err)
	}
}

// TestOpenForReadRepairsContentDrift pins that the read open's handoff is
// keyed on the same assessment the write open repairs from: a workspace whose
// goose log is complete but whose live schema lost a registered column is a
// trailing workspace to a reader too, so a read command repairs it through
// Open rather than serving rows from a schema the registry does not describe.
func TestOpenForReadRepairsContentDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	withStore(t, ctx, doltRoot, func(st *Store) {
		mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN lane`)
		mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN resolution`)
		mustCommit(t, ctx, st, "test: simulate version-slot reuse (goose fully applied, v2/v3 content missing)")
	})

	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() of a drifted workspace error = %v; want a transparent repair, not a refusal", err)
	}
	defer reader.Close()
	cols, err := reader.tableColumns(ctx, "issues")
	if err != nil {
		t.Fatalf("tableColumns(issues) error = %v", err)
	}
	if !cols["lane"] || !cols["resolution"] {
		t.Fatalf("issues columns after a read open = lane:%v resolution:%v; want both repaired", cols["lane"], cols["resolution"])
	}
}
