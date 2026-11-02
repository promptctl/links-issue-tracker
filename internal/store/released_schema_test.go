package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// registryMax is the highest migration this tree carries.
func registryMax(t *testing.T) int64 {
	t.Helper()
	max, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	return max
}

// A lit whose released-schema ceiling is below the schema it carries opens an
// existing workspace that trails it, and refuses to migrate it: the workspace
// keeps its schema, its commits, and its snapshots.
func TestAMigratingOpenRefusesToMoveAnExistingWorkspacePastTheReleasedSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, doltRoot := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	logBefore := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id")
	snapshotsBefore := snapshotCount(t, doltRoot)

	_, err := Open(ctx, testEnv().WithReleasedSchema(max-1), testStderr(), doltRoot, "test-workspace-id")
	var refused *UnreleasedMigrationError
	if !errors.As(err, &refused) {
		t.Fatalf("Open() with released schema %d = %v, want an UnreleasedMigrationError", max-1, err)
	}
	if want := (UnreleasedMigrationError{WorkspaceVersion: max - 1, TargetVersion: max, ReleasedSchema: max - 1}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if got := asConvergenceError(t, err).Step; got != stepRefuseUnreleased {
		t.Errorf("refusal step = %s, want %s", got, stepRefuseUnreleased)
	}
	if logAfter := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id"); !slices.Equal(logAfter, logBefore) {
		t.Errorf("the refused open changed the commit log:\nbefore %q\nafter  %q", logBefore, logAfter)
	}
	if got := snapshotCount(t, doltRoot); got != snapshotsBefore {
		t.Errorf("the refused open left %d snapshot(s), want %d", got, snapshotsBefore)
	}

	// At a ceiling that reaches the tree's schema, the same workspace migrates.
	migrated, err := Open(ctx, testEnv().WithReleasedSchema(max), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() with released schema %d = %v, want it to migrate", max, err)
	}
	defer migrated.Close()
	if got, err := migrated.recordedMigrationVersion(ctx); err != nil || got != max {
		t.Fatalf("recorded version after the released open = %d, %v; want %d", got, err, max)
	}
}

// A fresh store is created at the schema the binary carries whatever the
// ceiling: creating a scratch workspace moves no existing workspace's schema.
func TestCreatingAStoreIsNotBoundedByTheReleasedSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, testEnv().WithReleasedSchema(0), testStderr(), filepath.Join(t.TempDir(), "dolt"), "ws")
	if err != nil {
		t.Fatalf("Open() of a fresh store with released schema 0 = %v", err)
	}
	defer st.Close()
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != registryMax(t) {
		t.Fatalf("recorded version = %d, %v; want %d", got, err, registryMax(t))
	}
}

// A push of a store whose schema is above the released-schema ceiling is
// refused before anything reaches the remote.
func TestAPushOfAStoreAboveTheReleasedSchemaSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer := filepath.Join(base, "producer")
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	seeded, err := Open(ctx, testEnv(), testStderr(), producer, "ws")
	if err != nil {
		t.Fatalf("Open(producer) = %v", err)
	}
	if err := seeded.Close(); err != nil {
		t.Fatalf("Close(producer) = %v", err)
	}

	sync, err := OpenSync(ctx, testEnv().WithReleasedSchema(max-1), testStderr(), producer, "ws")
	if err != nil {
		t.Fatalf("OpenSync() = %v", err)
	}
	if err := sync.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote() = %v", err)
	}
	_, err = sync.SyncPush(ctx, origin, "master", true, false)
	if closeErr := sync.Close(); closeErr != nil {
		t.Fatalf("Close(sync) = %v", closeErr)
	}
	var refused *UnreleasedPushError
	if !errors.As(err, &refused) {
		t.Fatalf("SyncPush() with released schema %d = %v, want an UnreleasedPushError", max-1, err)
	}
	if want := (UnreleasedPushError{Remote: "origin", Branch: "master", CommitSchema: max, ReleasedSchema: max - 1}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "refused-clone"), "ws", origin, remoteURL, "master"); err == nil {
		t.Fatal("the remote holds master after the refused push, want no ref written")
	}

	// At a ceiling that reaches the store's schema, the same push lands, and
	// the clone that found nothing above finds it.
	released := openSyncOrFatal(t, ctx, producer)
	if _, err := released.SyncPush(ctx, origin, "master", true, false); err != nil {
		t.Fatalf("SyncPush() with released schema %d = %v", max, err)
	}
	if err := released.Close(); err != nil {
		t.Fatalf("Close(released sync) = %v", err)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the released push = %v", err)
	}
}

// A migrating open refuses an unreleased raise before it writes anything,
// drift repair included: a workspace that trails the binary and has lost a
// registered column keeps its commit log exactly as it was.
func TestTheUnreleasedRefusalRunsBeforeDriftRepair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, doltRoot := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check`)
	mustExec(t, ctx, st, `ALTER TABLE issues DROP CONSTRAINT issues_resolution_check`)
	mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN lane`)
	mustExec(t, ctx, st, `ALTER TABLE issues DROP COLUMN resolution`)
	mustCommit(t, ctx, st, "test: drift below the released schema")
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	logBefore := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id")

	_, err := Open(ctx, testEnv().WithReleasedSchema(max-1), testStderr(), doltRoot, "test-workspace-id")
	var refused *UnreleasedMigrationError
	if !errors.As(err, &refused) {
		t.Fatalf("Open() with released schema %d = %v, want an UnreleasedMigrationError", max-1, err)
	}
	if logAfter := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id"); !slices.Equal(logAfter, logBefore) {
		t.Errorf("the refused open wrote before refusing:\nbefore %q\nafter  %q", logBefore, logAfter)
	}
}

// A workspace that predates schema versioning is refused as one, not as a
// workspace at schema version 0.
func TestAnUnreleasedRaiseOfAPreVersionedWorkspaceNamesItsShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	first, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) = %v", err)
	}

	_, err = Open(ctx, testEnv().WithReleasedSchema(max-1), testStderr(), doltRoot, "test-workspace-id")
	var refused *UnreleasedMigrationError
	if !errors.As(err, &refused) {
		t.Fatalf("Open() with released schema %d = %v, want an UnreleasedMigrationError", max-1, err)
	}
	if want := (UnreleasedMigrationError{PreVersioned: true, TargetVersion: max, ReleasedSchema: max - 1}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if !strings.Contains(err.Error(), "unversioned pre-migration shape") || strings.Contains(err.Error(), "schema version 0") {
		t.Errorf("refusal %q does not name the pre-versioned shape", err)
	}
}

// The reconcile's lift raises a commit's schema on a scratch branch through the
// same stepper the migrating open does, and the ceiling binds it there too: a
// lift above the released schema is refused and leaves the schema where it was.
func TestTheReconcileLiftRefusesToRaisePastTheReleasedSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	st.env = testEnv().WithReleasedSchema(max - 1)

	err := st.liftWorkingSetToRegistry(ctx)
	var refused *UnreleasedMigrationError
	if !errors.As(err, &refused) {
		t.Fatalf("lift with released schema %d = %v, want an UnreleasedMigrationError", max-1, err)
	}
	if want := (UnreleasedMigrationError{WorkspaceVersion: max - 1, TargetVersion: max, ReleasedSchema: max - 1}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != max-1 {
		t.Fatalf("recorded version after the refused lift = %d, %v; want %d", got, err, max-1)
	}
}
