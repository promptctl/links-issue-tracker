package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
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

// The reconcile's lift is bound by the hold its caller hands it, the fetched
// head's for the head the spine adopts: a held head is lifted no further than
// its hold, whatever this lit's registry carries.
func TestTheReconcileLiftRefusesToRaisePastAHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	holdStore(t, ctx, st, max-1)

	hold, err := st.schemaHold(ctx)
	if err != nil {
		t.Fatalf("schemaHold() = %v", err)
	}
	err = st.liftWorkingSetToRegistry(ctx, hold)
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
		outcome, _, err = st.pushLeasedWithinLock(ctx, origin, "master", "master", heldHead, false)
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
	var refused *SchemaCeilingLiftError
	if !errors.As(err, &refused) {
		t.Fatalf("reconcile against a remote held at %d = %v, want a SchemaCeilingLiftError from the lift", max-1, err)
	}
	wantLift := SchemaCeilingError{Raise: RaiseLift, WorkspaceVersion: max - 1, TargetVersion: max, Ceiling: holdBy("origin/master's head", max-1)}
	if refused.Remote != "origin" || refused.Branch != "master" || refused.Lift != wantLift {
		t.Errorf("refusal = %+v, want the lift from %d to %d refused at the fetched head's hold, naming that head, for origin/master", *refused, max-1, max)
	}
	var lift *SchemaCeilingError
	if !errors.As(err, &lift) || *lift != refused.Lift {
		t.Errorf("refusal %v does not wrap the lift's own refusal", err)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused reconcile = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != heldHead {
		t.Fatalf("remote head after the refused reconcile = %s, want %s untouched", got, heldHead)
	}
}

// A remote downgrade's hold rides in the commit of the last reversed
// migration: the head before it carries no hold, the new head carries the
// hold at the target, and the reverse added exactly its reversal commits. Run
// again at the target, it reverses nothing and commits nothing.
func TestARemoteDowngradeHoldsTheStoreInItsLastCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	before := headCommit(t, ctx, st)
	logBefore := commitMessages(t, ctx, st)

	done, err := st.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	if done.From != max || done.Reversed != 1 || done.Hold != max-1 {
		t.Fatalf("done = %+v, want From %d, Reversed 1, Hold %d", *done, max, max-1)
	}
	if facts, _ := done.EventRecord(); facts["downgrade.schema_hold"] != max-1 {
		t.Errorf("downgrade.schema_hold = %v, want %d", facts["downgrade.schema_hold"], max-1)
	}
	if logAfter := commitMessages(t, ctx, st); len(logAfter) != len(logBefore)+1 {
		t.Fatalf("the reverse added %d commits, want 1:\n%q", len(logAfter)-len(logBefore), logAfter)
	}
	if hold, err := st.schemaHold(ctx); err != nil || hold != holdBy("this store", max-1) {
		t.Fatalf("schemaHold() after the reverse = %+v, %v; want %+v", hold, err, holdBy("this store", max-1))
	}
	if hold, err := st.schemaHoldAt(ctx, before, "the head before"); err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHoldAt(head before the reverse) = %+v, %v; want the unbounded ceiling", hold, err)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != max-1 {
		t.Fatalf("recorded version = %d, %v; want %d", got, err, max-1)
	}

	logHeld := commitMessages(t, ctx, st)
	again, err := st.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("second DowngradeAndHold(%d) = %v", max-1, err)
	}
	if again.From != max-1 || again.Reversed != 0 || again.Hold != max-1 {
		t.Fatalf("second done = %+v, want From %d, Reversed 0, Hold %d", *again, max-1, max-1)
	}
	if logAgain := commitMessages(t, ctx, st); !slices.Equal(logAgain, logHeld) {
		t.Fatalf("a repeated remote downgrade committed:\nbefore %q\nafter  %q", logHeld, logAgain)
	}
}

// A store already at the target has no reversal commit for the hold to ride
// in, so the hold lands in a commit of its own.
func TestARemoteDowngradeAtTheTargetHoldsInACommitOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	if hold, err := st.schemaHold(ctx); err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHold() after a plain downgrade = %+v, %v; want the unbounded ceiling", hold, err)
	}
	logBefore := commitMessages(t, ctx, st)

	done, err := st.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	if done.From != max-1 || done.Reversed != 0 || done.Hold != max-1 {
		t.Fatalf("done = %+v, want From %d, Reversed 0, Hold %d", *done, max-1, max-1)
	}
	logAfter := commitMessages(t, ctx, st)
	if want := append([]string{fmt.Sprintf("downgrade: hold schema at %d", max-1)}, logBefore...); !slices.Equal(logAfter, want) {
		t.Fatalf("commit log after the hold:\n%q\nwant\n%q", logAfter, want)
	}
	if hold, err := st.schemaHold(ctx); err != nil || hold != holdBy("this store", max-1) {
		t.Fatalf("schemaHold() = %+v, %v; want %+v", hold, err, holdBy("this store", max-1))
	}
}

// The lift removes the hold in one commit and reports it; a store with no
// hold lifts nothing and commits nothing; and once lifted, the store rises
// to the registry again.
func TestLiftSchemaHoldRemovesTheHoldInOneCommitAndTheStoreRisesAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)
	if _, err := st.DowngradeAndHold(ctx, max-1); err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	logHeld := commitMessages(t, ctx, st)

	lifted, err := st.LiftSchemaHold(ctx)
	if err != nil {
		t.Fatalf("LiftSchemaHold() = %v", err)
	}
	if lifted.Lifted != max-1 {
		t.Fatalf("Lifted = %d, want %d", lifted.Lifted, max-1)
	}
	if facts, _ := lifted.EventRecord(); facts["schema_hold.lifted"] != max-1 {
		t.Errorf("schema_hold.lifted = %v, want %d", facts["schema_hold.lifted"], max-1)
	}
	logLifted := commitMessages(t, ctx, st)
	if want := append([]string{fmt.Sprintf("upgrade: lift the schema hold at %d", max-1)}, logHeld...); !slices.Equal(logLifted, want) {
		t.Fatalf("commit log after the lift:\n%q\nwant\n%q", logLifted, want)
	}
	if hold, err := st.schemaHold(ctx); err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHold() after the lift = %+v, %v; want the unbounded ceiling", hold, err)
	}

	none, err := st.LiftSchemaHold(ctx)
	if err != nil || none.Lifted != 0 {
		t.Fatalf("LiftSchemaHold() of an unheld store = %+v, %v; want Lifted 0", none, err)
	}
	if logAgain := commitMessages(t, ctx, st); !slices.Equal(logAgain, logLifted) {
		t.Fatalf("a lift of an unheld store committed:\nbefore %q\nafter  %q", logLifted, logAgain)
	}

	if err := st.liftWorkingSetToRegistry(ctx, version.UnboundedSchemaCeiling()); err != nil {
		t.Fatalf("lift of the working set after the hold was lifted = %v", err)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != max {
		t.Fatalf("recorded version after the rise = %d, %v; want %d", got, err, max)
	}
}

// A push that would lower a remote with a head carrying no schema hold is
// refused, naming both schemas, and the remote keeps its head; the same head
// held at its schema lands, and the remote reads as held there. The hold is
// what makes a lowering stick, so it is what a lowering must carry.
func TestAnUnheldLoweringPushSendsNothingAndAHeldOneLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	producer, consumer := migratedDoltDir(t), migratedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	origin := remoteName(t, "origin")
	max := registryMax(t)

	seed := openSyncOrFatal(t, ctx, producer)
	if err := seed.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed) = %v", err)
	}
	if _, err := seed.SyncPush(ctx, origin, "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed) = %v", err)
	}
	remoteHead := headCommit(t, ctx, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("Close(seed) = %v", err)
	}

	st := openSyncOrFatal(t, ctx, consumer)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, origin, remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(consumer) = %v", err)
	}
	if err := st.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(consumer) = %v", err)
	}
	if _, err := st.Downgrade(ctx, max-1); err != nil {
		t.Fatalf("Downgrade(%d) = %v", max-1, err)
	}
	_, err := st.SyncPush(ctx, origin, "master", false, false)
	var refused *SchemaLoweringRequiresHoldError
	if !errors.As(err, &refused) {
		t.Fatalf("SyncPush of an unheld head at %d over a remote at %d = %v, want a SchemaLoweringRequiresHoldError", max-1, max, err)
	}
	if want := (SchemaLoweringRequiresHoldError{Remote: "origin", Branch: "master", RemoteSchema: max, CommitSchema: max - 1}); *refused != want {
		t.Errorf("refusal = %+v, want %+v", *refused, want)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the refused push = %v", err)
	}
	if got := headAt(t, ctx, filepath.Join(base, "clone")); got != remoteHead {
		t.Fatalf("remote head after the refused push = %s, want %s untouched", got, remoteHead)
	}

	done, err := st.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	if done.Reversed != 0 || done.Hold != max-1 {
		t.Fatalf("done = %+v, want Reversed 0, Hold %d", *done, max-1)
	}
	if _, err := st.SyncPush(ctx, origin, "master", false, false); err != nil {
		t.Fatalf("SyncPush of the held head = %v", err)
	}
	if err := AdoptRemoteByClone(ctx, testStderr(), filepath.Join(base, "clone-held"), "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the held push = %v", err)
	}
	if got, want := headAt(t, ctx, filepath.Join(base, "clone-held")), headCommit(t, ctx, st); got != want {
		t.Fatalf("remote head after the held push = %s, want %s", got, want)
	}
	if head := trackingSchema(t, ctx, st); head.version != max-1 || head.hold != holdBy("origin/master's head", max-1) {
		t.Fatalf("remote after the held push = %+v, want schema %d held there", head, max-1)
	}
}

// One migrated workspace carries every rule of the hold's writer and remover,
// in sequence, since each starts from the state the one before it leaves and a
// workspace costs seconds to build. A hold below the head (a binary from
// before the row raised the store past it) is the operator's hold: a release
// downgrade to a schema above it leaves it where it is. A release downgrade
// below it moves the hold with the head at every step, so a reverse that
// stops part-way leaves no head holding above itself. A hold row or its
// removal a crash left uncommitted is committed by the next remote downgrade
// or lift before it judges the hold, so the head it then pushes carries what
// the working set said.
func TestTheHoldMovesWithTheHeadAndNeverRises(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	max := registryMax(t)
	st, _ := openWorkspaceForDowngrade(t)

	// A hold below the head stays where it is.
	holdStore(t, ctx, st, max-2)
	done, err := st.Downgrade(ctx, max-1)
	if err != nil {
		t.Fatalf("Downgrade(%d) of a store held at %d = %v", max-1, max-2, err)
	}
	if done.From != max || done.Reversed != 1 || done.Hold != max-2 {
		t.Fatalf("done = %+v, want From %d, Reversed 1, Hold %d left where it was", *done, max, max-2)
	}
	if hold, err := st.schemaHold(ctx); err != nil || hold != holdBy("this store", max-2) {
		t.Fatalf("schemaHold() = %+v, %v; want %+v", hold, err, holdBy("this store", max-2))
	}

	// Below it, the hold moves with the head, at every step.
	done, err = st.Downgrade(ctx, max-4)
	if err != nil {
		t.Fatalf("Downgrade(%d) of a store held at %d = %v", max-4, max-2, err)
	}
	if done.From != max-1 || done.Reversed != 3 || done.Hold != max-4 {
		t.Fatalf("done = %+v, want From %d, Reversed 3, Hold %d", *done, max-1, max-4)
	}
	if hold, err := st.schemaHoldAt(ctx, headCommit(t, ctx, st), "the head"); err != nil || hold != holdBy("the head", max-4) {
		t.Fatalf("schemaHoldAt(head) = %+v, %v; want the hold in the lowered commit", hold, err)
	}
	if hold, err := st.schemaHoldAt(ctx, parentCommit(t, ctx, st), "the step before"); err != nil || hold != holdBy("the step before", max-3) {
		t.Fatalf("schemaHoldAt(parent) = %+v, %v; want the hold at %d", hold, err, max-3)
	}

	// A removal a crash left uncommitted is committed by the next lift.
	held := headCommit(t, ctx, st)
	if _, err := st.db.ExecContext(ctx, `DELETE FROM meta WHERE meta_key = ?`, schemaHoldKey); err != nil {
		t.Fatalf("leave the removal uncommitted: %v", err)
	}
	if _, err := st.LiftSchemaHold(ctx); err != nil {
		t.Fatalf("LiftSchemaHold() over the stranded removal = %v", err)
	}
	unheld := headCommit(t, ctx, st)
	if unheld == held {
		t.Fatalf("the stranded removal was never committed: HEAD is still %s", held)
	}
	if hold, err := st.schemaHoldAt(ctx, unheld, "HEAD"); err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHoldAt(HEAD) after the lift = %+v, %v; want no hold", hold, err)
	}

	// A hold row a crash left uncommitted is committed by the next remote
	// downgrade.
	if err := st.setMeta(ctx, nil, schemaHoldKey, strconv.FormatInt(max-4, 10)); err != nil {
		t.Fatalf("leave the hold row uncommitted: %v", err)
	}
	done, err = st.DowngradeAndHold(ctx, max-4)
	if err != nil {
		t.Fatalf("DowngradeAndHold(%d) over the stranded row = %v", max-4, err)
	}
	if done.From != max-4 || done.Reversed != 0 || done.Hold != max-4 {
		t.Fatalf("done = %+v, want From %d, Reversed 0, Hold %d", *done, max-4, max-4)
	}
	head := headCommit(t, ctx, st)
	if head == unheld {
		t.Fatalf("the stranded hold row was never committed: HEAD is still %s", unheld)
	}
	if hold, err := st.schemaHoldAt(ctx, head, "HEAD"); err != nil || hold != holdBy("HEAD", max-4) {
		t.Fatalf("schemaHoldAt(HEAD) = %+v, %v; want the hold committed", hold, err)
	}
}

// A remote downgrade whose push is refused as diverged is completed by a pull
// and a second run. Two clones of one remote: A fetches, B pushes a ticket, A
// lowers and holds, and A's push is refused as diverged. A's pull reads A's own
// held head as a projection, which no hold binds, so the replay lands A on B's
// head at this lit's schema with no hold and the reversed migration back (A's
// reversal and hold commits change no ticket and fold to nothing). The second
// remote downgrade reverses once more, holds, and its push fast-forwards: the
// remote's head is held at vN-1 and carries B's ticket.
func TestADivergedRemoteLoweringPulledAndRunAgainLandsAHeldHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	max := registryMax(t)
	origin := remoteName(t, "origin")
	remoteURL := "file://" + filepath.Join(base, "remote")
	rootB := migratedDoltDir(t)
	epicID := seedSyncEpic(t, ctx, rootB, "ws")
	pushSyncFrom(t, ctx, rootB, remoteURL, "seed")
	rootA := cloneStoreTree(t, ctx, rootB, base)

	a := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = a.Close() }()
	if err := a.SyncFetch(ctx, origin); err != nil {
		t.Fatalf("SyncFetch(A) = %v", err)
	}
	commitTitled(t, ctx, rootB, epicID, "B's ticket")
	pushSyncFrom(t, ctx, rootB, "", "B's push")

	done, err := a.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("DowngradeAndHold(%d) = %v", max-1, err)
	}
	if done.From != max || done.Reversed != 1 || done.Hold != max-1 {
		t.Fatalf("done = %+v, want From %d, Reversed 1, Hold %d", *done, max, max-1)
	}
	if _, err := a.SyncPush(ctx, origin, "master", false, false); !errors.Is(err, storage.ErrPushDiverged) {
		t.Fatalf("SyncPush of the lowering over B's ticket = %v, want storage.ErrPushDiverged", err)
	}

	pulled, err := a.SyncPull(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncPull after the refused lowering = %v", err)
	}
	if pulled.State != storage.SyncPullLinearized {
		t.Fatalf("SyncPull state = %q, want %q", pulled.State, storage.SyncPullLinearized)
	}
	if got, err := a.recordedMigrationVersion(ctx); err != nil || got != max {
		t.Fatalf("recorded version after the pull = %d, %v; want %d, the remote head's schema the replay landed on", got, err, max)
	}
	if hold, err := a.schemaHold(ctx); err != nil || hold != version.UnboundedSchemaCeiling() {
		t.Fatalf("schemaHold() after the pull = %+v, %v; want none, since the hold that never landed folds to nothing", hold, err)
	}
	if n := issuesTitled(t, ctx, a, "B's ticket"); n != 1 {
		t.Fatalf("A holds %d rows of B's ticket after the pull, want 1", n)
	}

	again, err := a.DowngradeAndHold(ctx, max-1)
	if err != nil {
		t.Fatalf("second DowngradeAndHold(%d) = %v", max-1, err)
	}
	if again.From != max || again.Reversed != 1 || again.Hold != max-1 {
		t.Fatalf("second done = %+v, want From %d, Reversed 1, Hold %d", *again, max, max-1)
	}
	if _, err := a.SyncPush(ctx, origin, "master", false, false); err != nil {
		t.Fatalf("SyncPush of the held head after the pull = %v", err)
	}

	verify := filepath.Join(base, "verify")
	if err := AdoptRemoteByClone(ctx, testStderr(), verify, "ws", origin, remoteURL, "master"); err != nil {
		t.Fatalf("clone after the held push = %v", err)
	}
	if got, want := headAt(t, ctx, verify), headCommit(t, ctx, a); got != want {
		t.Fatalf("remote head after the held push = %s, want A's head %s", got, want)
	}
	if head := trackingSchema(t, ctx, a); head.version != max-1 || head.hold != holdBy("origin/master's head", max-1) {
		t.Fatalf("remote after the held push = %+v, want schema %d held there", head, max-1)
	}
	remote := openSyncOrFatal(t, ctx, verify)
	defer func() { _ = remote.Close() }()
	if n := issuesTitled(t, ctx, remote, "B's ticket"); n != 1 {
		t.Fatalf("the remote's held head holds %d rows of B's ticket, want 1", n)
	}
}

// issuesTitled is how many issues st holds titled title. It reads the rows
// rather than exporting: a held store sits below this lit's schema, where the
// export names a column the reversed migration removed.
func issuesTitled(t *testing.T, ctx context.Context, st *Store, title string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE title = ?`, title).Scan(&n); err != nil {
		t.Fatalf("count issues titled %q: %v", title, err)
	}
	return n
}

// parentCommit is the hash of the commit before HEAD on st's checked-out branch.
func parentCommit(t *testing.T, ctx context.Context, st *Store) string {
	t.Helper()
	var hash string
	if err := st.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1 OFFSET 1`).Scan(&hash); err != nil {
		t.Fatalf("read the parent commit: %v", err)
	}
	return hash
}
