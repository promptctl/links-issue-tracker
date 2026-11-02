package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// holdStore writes meta.schema_hold = v into st's working set and commits it,
// as a remote downgrade leaves a lowered head.
func holdStore(t *testing.T, ctx context.Context, st *Store, v int64) {
	t.Helper()
	mustExec(t, ctx, st, `INSERT INTO meta (meta_key, meta_value) VALUES (?, ?) ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`, schemaHoldKey, fmt.Sprint(v))
	mustCommit(t, ctx, st, fmt.Sprintf("test: hold the schema at %d", v))
}

// holdBy is the ceiling a hold at v held by holder reads as.
func holdBy(holder string, v int64) version.SchemaCeiling {
	return version.SchemaCeiling{Max: v, Source: fmt.Sprintf("%s holds its schema at %d (meta.schema_hold), which a remote downgrade set", holder, v), Bound: version.BoundHold}
}

// A store with no hold row reads as the unbounded ceiling, a held store as a
// ceiling at the hold naming it, and a row that is not a schema version is an
// error, never read as no hold.
func TestASchemaHoldReadsAsACeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)

	hold, err := st.schemaHold(ctx)
	if err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHold() of an unheld store = %+v, %v; want the unbounded ceiling", hold, err)
	}

	mustExec(t, ctx, st, `INSERT INTO meta (meta_key, meta_value) VALUES (?, '7')`, schemaHoldKey)
	hold, err = st.schemaHold(ctx)
	if err != nil || hold != holdBy("this store", 7) {
		t.Fatalf("schemaHold() of a held store = %+v, %v; want %+v", hold, err, holdBy("this store", 7))
	}

	for _, raw := range []string{"seven", "0", "-1"} {
		mustExec(t, ctx, st, `UPDATE meta SET meta_value = ? WHERE meta_key = ?`, raw, schemaHoldKey)
		if _, err := st.schemaHold(ctx); err == nil || !strings.Contains(err.Error(), fmt.Sprintf(`meta schema_hold = %q is not a schema version`, raw)) {
			t.Fatalf("schemaHold() of hold %q = %v, want the malformed row named", raw, err)
		}
	}
}

// A binary whose registry is above a held store's schema refuses to open it:
// the hold is the ceiling, named as such, and the store keeps its commits.
func TestAMigratingOpenRefusesToRaiseAHeldStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, doltRoot := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	holdStore(t, ctx, st, max-1)
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	logBefore := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id")

	_, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	var refused *SchemaCeilingError
	if !errors.As(err, &refused) {
		t.Fatalf("Open() of a store held at %d = %v, want a SchemaCeilingError", max-1, err)
	}
	if want := (SchemaCeilingError{Raise: RaiseMigrate, WorkspaceVersion: max - 1, TargetVersion: max, Ceiling: holdBy("this store", max-1)}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if logAfter := observedCommitMessages(t, ctx, doltRoot, "test-workspace-id"); !slices.Equal(logAfter, logBefore) {
		t.Errorf("the refused open changed the commit log:\nbefore %q\nafter  %q", logBefore, logAfter)
	}
}

// A hold at the schema the store already has refuses nothing: every open
// serves the store and reports the hold, the read-only and sync opens as the
// migrating open does.
func TestAnOpenAtTheHoldServesTheStoreAndReportsTheHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, doltRoot := openWorkspaceForDowngrade(t)
	holdStore(t, ctx, st, max)
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	type opener func(context.Context, environ.Env, io.Writer, string, string) (*Store, error)
	for name, open := range map[string]opener{"Open": Open, "OpenForRead": OpenForRead, "OpenSync": OpenSync} {
		held, err := open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
		if err != nil {
			t.Fatalf("%s() of a store held at its own schema %d = %v", name, max, err)
		}
		if got := held.Migrated().SchemaHold; got != max {
			t.Errorf("%s: Migrated().SchemaHold = %d, want %d", name, got, max)
		}
		if got := held.Migrated().EventRecord()["migration.schema_hold"]; got != max {
			t.Errorf("%s: migration.schema_hold = %v, want %d", name, got, max)
		}
		if err := held.Close(); err != nil {
			t.Fatalf("%s: Close() = %v", name, err)
		}
	}
}

// The reconcile's lift runs on a working set reset to the fetched head, so
// the hold that head carries bounds the lift: a held head is lifted no
// further than its hold, whatever this lit's registry carries.
func TestTheReconcileLiftRefusesToRaisePastAHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	holdStore(t, ctx, st, max-1)

	err := st.liftWorkingSetToRegistry(ctx)
	var refused *SchemaCeilingError
	if !errors.As(err, &refused) {
		t.Fatalf("lift of a working set held at %d = %v, want a SchemaCeilingError", max-1, err)
	}
	if want := (SchemaCeilingError{Raise: RaiseLift, WorkspaceVersion: max - 1, TargetVersion: max, Ceiling: holdBy("this store", max-1)}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != max-1 {
		t.Fatalf("recorded version after the refused lift = %d, %v; want %d", got, err, max-1)
	}
}

// seedHeldRemote publishes a remote whose head is one schema below this
// tree's registry max and holds it there, as `lit downgrade --remote` leaves
// it. It returns the remote head's hash.
func seedHeldRemote(t *testing.T, ctx context.Context, root, remoteURL string) string {
	t.Helper()
	max := registryMax(t)
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	holdStore(t, ctx, sync, max-1)
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed) = %v", err)
	}
	// The lowering push itself lands: its head is at the hold, not above it.
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed) = %v", err)
	}
	head := headCommit(t, ctx, sync)
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(seed) = %v", err)
	}
	return head
}

// A push judges the hold at the remote head it read: a store at this lit's
// schema pushing over a remote held one below is refused, forced or not,
// naming the remote head's hold, and the remote keeps its head.
func TestAPushAboveTheRemotesHoldSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	heldHead := seedHeldRemote(t, ctx, producer, remoteURL)

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	if err := st.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(consumer) = %v", err)
	}
	_, err := st.SyncPush(ctx, origin, "master", false, true)
	var refused *SchemaCeilingPushError
	if !errors.As(err, &refused) {
		t.Fatalf("forced SyncPush over a remote held at %d = %v, want a SchemaCeilingPushError", max-1, err)
	}
	if want := (SchemaCeilingPushError{Remote: "origin", Branch: "master", CommitSchema: max, Ceiling: holdBy("origin/master's head", max-1)}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if head := trackingSchema(t, ctx, st); head.version != max-1 || head.hold != holdBy("origin/master's head", max-1) {
		t.Fatalf("remote after the refused push = %+v, want schema %d held there", head, max-1)
	}
	if got := headAt(t, ctx, producer); got != heldHead {
		t.Fatalf("producer head moved to %s, want %s", got, heldHead)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused push = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != heldHead {
		t.Fatalf("remote head after the refused push = %s, want %s untouched", got, heldHead)
	}
}

// The leased push judges the hold at the head the lease names, so a lease
// over a held head is refused before it reaches the remote.
func TestALeasedPushAboveTheRemotesHoldSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	heldHead := seedHeldRemote(t, ctx, producer, remoteURL)

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	if err := st.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(consumer) = %v", err)
	}
	var outcome storage.LeaseOutcome
	err := st.runSyncMutation(ctx, func(ctx context.Context) error {
		var err error
		outcome, err = st.pushLeasedWithinLock(ctx, origin, "master", "master", heldHead)
		return err
	})
	var refused *SchemaCeilingPushError
	if !errors.As(err, &refused) {
		t.Fatalf("leased push over a remote held at %d = %v (outcome %q), want a SchemaCeilingPushError", max-1, err, outcome)
	}
	if outcome != "" {
		t.Fatalf("outcome = %q, want none: the push was refused before it reached the remote", outcome)
	}
	if want := (SchemaCeilingPushError{Remote: "origin", Branch: "master", CommitSchema: max, Ceiling: holdBy("origin/master's head", max-1)}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused lease = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != heldHead {
		t.Fatalf("remote head after the refused lease = %s, want %s untouched", got, heldHead)
	}
}

// A sync that merges a held remote head stops at the lift of that head, with
// the hold named, and the remote keeps its head: the reconcile never re-raises
// a head a remote downgrade lowered. The consumer's history is unrelated to
// the remote's, so the reconcile merges rather than fast-forwards; a
// fast-forward onto a held head steps the store down, where the hold then
// keeps it (TestAMigratingOpenRefusesToRaiseAHeldStore).
func TestASyncAgainstAHeldRemoteStopsAtTheLift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	heldHead := seedHeldRemote(t, ctx, producer, remoteURL)

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	if err := st.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(consumer) = %v", err)
	}
	_, err := st.SyncReconcileCombine(ctx, origin, "master")
	var refused *SchemaCeilingError
	if !errors.As(err, &refused) {
		t.Fatalf("reconcile against a remote held at %d = %v, want a SchemaCeilingError from the lift", max-1, err)
	}
	if refused.Raise != RaiseLift || refused.Ceiling != holdBy("this store", max-1) {
		t.Errorf("refusal = %+v, want the lift refused at the fetched head's hold", *refused)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused reconcile = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != heldHead {
		t.Fatalf("remote head after the refused reconcile = %s, want %s untouched", got, heldHead)
	}
}
