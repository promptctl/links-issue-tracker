package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/doltcli"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// openWorkspaceForDowngrade opens a fresh workspace at registry-max — every
// downgrade test starts from this state because Open is the only legitimate
// way to land a goose-managed workspace.
func openWorkspaceForDowngrade(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, doltRoot
}

// snapshotCount returns how many snapshots exist in the workspace's snapshots
// directory. Used to assert "no snapshot taken" for the refusal paths.
// Counts via dbsnapshot.List — the package's own definition of "a snapshot
// exists" — rather than raw dir entries, which would also count non-snapshot
// machinery like the producer beacon file. [LAW:one-source-of-truth]
//
// Because List refuses producer-artifact names by design, the count alone
// cannot notice a Take that strands its .tmp/.reserve; the explicit
// artifact scan keeps that cleanup invariant pinned.
func snapshotCount(t *testing.T, doltRoot string) int {
	t.Helper()
	dir := dbsnapshot.DirFor(doltRoot)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readdir snapshots: %v", err)
	}
	for _, e := range entries {
		if dbsnapshot.IsProducerArtifactName(e.Name()) {
			t.Fatalf("stranded producer artifact in snapshots dir: %s", e.Name())
		}
	}
	list, err := dbsnapshot.List(dir)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	return len(list)
}

// newSnapshotName is the one snapshot in after that before does not hold.
func newSnapshotName(before, after []dbsnapshot.Snapshot) string {
	held := map[string]bool{}
	for _, s := range before {
		held[s.Name] = true
	}
	for _, s := range after {
		if !held[s.Name] {
			return s.Name
		}
	}
	return ""
}

// TestAppliedSchemaVersionMatchesRecorded pins the exported reader `lit upgrade`
// consults for its backward-move refusal: on a goose-managed workspace it
// returns the same recorded version DowngradeTargetAheadError compares against.
func TestAppliedSchemaVersionMatchesRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)

	recorded, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	if recorded <= 0 {
		t.Fatalf("recordedMigrationVersion() = %d; want a managed workspace at a real version", recorded)
	}
	applied, err := st.AppliedSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("AppliedSchemaVersion() = %v", err)
	}
	if applied != recorded {
		t.Errorf("AppliedSchemaVersion() = %d; want %d (must equal the recorded/applied version)", applied, recorded)
	}
}

// TestAppliedSchemaVersionZeroForNonManaged pins the other half of the reader's
// contract: a workspace that is NOT goose-managed reports applied version 0. This
// is the fact the `lit upgrade` backward-move check leans on — if
// classifyMigrationState ever populated appliedVersion on a non-managed path, the
// guard would compare against a bogus version. Dropping goose_db_version after a
// normal open reclassifies the workspace as phaseAdopt (canonical tables present,
// no goose table, no issue_history), the realistic non-managed state; phaseFresh
// (no tables at all) is unreachable here because the reader's caller opens through
// OpenForRead, which requires an initialized database.
func TestAppliedSchemaVersionZeroForNonManaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)

	// Sanity: managed workspace reports a real version before we perturb it.
	managed, err := st.AppliedSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("AppliedSchemaVersion() (managed) = %v", err)
	}
	if managed <= 0 {
		t.Fatalf("AppliedSchemaVersion() (managed) = %d; want > 0", managed)
	}

	// Drop the goose bookkeeping so the next classify sees a pre-goose (adopt)
	// workspace: canonical tables remain, but there is no applied-version record.
	mustExec(t, ctx, st, `DROP TABLE goose_db_version`)

	adopt, err := st.AppliedSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("AppliedSchemaVersion() (adopt) = %v", err)
	}
	if adopt != 0 {
		t.Errorf("AppliedSchemaVersion() on a non-managed workspace = %d; want 0", adopt)
	}
}

// TestDowngradeTargetEqualIsNoOp pins the no-op branch: target == current
// reverses nothing and takes no snapshot, and says so.
func TestDowngradeTargetEqualIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	before := snapshotCount(t, doltRoot)
	done, err := st.Downgrade(ctx, current)
	if err != nil {
		t.Fatalf("Downgrade(current) = %v, want nil", err)
	}
	if want := (storage.Downgraded{From: current}); done != want {
		t.Fatalf("Downgrade(current) = %+v, want %+v", done, want)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("no-op downgrade took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeTargetAheadRefused pins the forward-direction refusal: a target
// above current yields a typed error and takes no snapshot.
func TestDowngradeTargetAheadRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	before := snapshotCount(t, doltRoot)
	_, err = st.Downgrade(ctx, current+5)
	var ahead *DowngradeTargetAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("err = %v, want *DowngradeTargetAheadError", err)
	}
	if ahead.Current != current || ahead.Target != current+5 {
		t.Fatalf("err fields = {Current:%d Target:%d}, want {%d %d}",
			ahead.Current, ahead.Target, current, current+5)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("ahead-refusal took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeBelowBaselineRefused pins the destructive-Down guard: target
// below baselineVersion (the only Down whose execution drops every table) is
// refused before invoking goose, so the snapshot is NOT taken.
func TestDowngradeBelowBaselineRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	before := snapshotCount(t, doltRoot)
	_, err = st.Downgrade(ctx, baselineVersion-1)
	var below *DowngradeBelowBaselineError
	if !errors.As(err, &below) {
		t.Fatalf("err = %v, want *DowngradeBelowBaselineError", err)
	}
	if below.Current != current || below.Target != baselineVersion-1 {
		t.Fatalf("err fields = {Current:%d Target:%d}, want {%d %d}",
			below.Current, below.Target, current, baselineVersion-1)
	}
	if !strings.Contains(err.Error(), "would destroy the workspace") {
		t.Fatalf("err message missing destructive warning: %v", err)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("below-baseline-refusal took %d snapshot(s); want 0", delta)
	}
}

// TestDowngradeRollbackOnFailure pins the rollback path: when a Down step
// fails after the snapshot is taken, the returned error is a typed
// DowngradeRollbackError naming the snapshot, and the snapshot exists on disk
// for `lit snapshots restore` to consume.
//
// The span (registry-max down to registry-max-1) is one this binary covers, so
// the refusals admit it into the loop; the injected Down hook then fails AFTER
// the snapshot guard fires, so DowngradeRollbackError must wrap the failure.
func TestDowngradeRollbackOnFailure(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationDownForTest
	// hook.
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)

	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}

	want := errors.New("synthetic down failure")
	migrationDownForTest = func(ctx context.Context, _ *goose.Provider) (*goose.MigrationResult, error) {
		return nil, want
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	_, err = st.Downgrade(ctx, registryMax-1)
	var rb *DowngradeRollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want *DowngradeRollbackError", err)
	}
	if rb.From != registryMax || rb.Reversed != 0 {
		t.Fatalf("rollback = {From:%d Reversed:%d}, want {%d 0}", rb.From, rb.Reversed, registryMax)
	}
	if !errors.Is(rb, want) {
		t.Fatalf("rollback error does not wrap synthetic cause: %v", rb)
	}
	if rb.Snapshot.Name == "" || rb.Snapshot.Path == "" {
		t.Fatalf("rollback snapshot incomplete: %+v", rb.Snapshot)
	}
	if _, statErr := os.Stat(rb.Snapshot.Path); statErr != nil {
		t.Fatalf("snapshot path not present on disk: %v", statErr)
	}
	if !strings.Contains(rb.Error(), "lit snapshots restore "+rb.Snapshot.Name) {
		t.Fatalf("rollback message missing literal restore command: %v", rb)
	}
}

// TestDowngradeHappyPathSteppedAndCommitted pins the multi-step happy path:
// applied above target, every Down runs, recorded version lands at target,
// one Dolt commit per reversed migration.
//
// It reverses the registry's top two migrations with their real Down
// sections — a span this binary covers — and asserts (a) the recorded version
// walks down to target, and (b) the dolt log carries one commit per reversed
// file, named by the registry's own file names.
func TestDowngradeHappyPathSteppedAndCommitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, doltRoot := openWorkspaceForDowngrade(t)

	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	target := registryMax - 2
	before, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	done, err := st.Downgrade(ctx, target)
	if err != nil {
		t.Fatalf("Downgrade(%d) = %v, want nil", target, err)
	}
	got, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	if got != target {
		t.Fatalf("recorded version after downgrade = %d, want %d", got, target)
	}
	after, err := dbsnapshot.List(dbsnapshot.DirFor(doltRoot))
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(after)-len(before) != 1 || snapshotCount(t, doltRoot) != len(after) {
		t.Fatalf("happy-path took %d snapshot(s); want exactly 1", len(after)-len(before))
	}
	taken := newSnapshotName(before, after)
	if want := (storage.Downgraded{From: registryMax, Reversed: 2, Snapshot: taken}); done != want {
		t.Fatalf("Downgrade(%d) = %+v, want %+v", target, done, want)
	}
	log, err := doltcli.Run(ctx, filepath.Join(doltRoot, "links"), "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log = %v", err)
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migration registry: %v", err)
	}
	reverted := 0
	for _, e := range entries {
		v, ok := migrations.ParseVersion(e.Name())
		if !ok || v <= target {
			continue
		}
		reverted++
		want := fmt.Sprintf("downgrade: revert v%d %s", v, e.Name())
		if !strings.Contains(log, want) {
			t.Fatalf("dolt log missing per-step commit %q:\n%s", want, log)
		}
	}
	if reverted != 2 {
		t.Fatalf("registry holds %d migration(s) above v%d, want 2 — the span under test", reverted, target)
	}
}

// A down step that fails after earlier steps committed reports how many did,
// so the event of a half-reversed workspace says how far the reverse got.
func TestDowngradeRollbackCountsTheStepsBeforeTheFailure(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationDownForTest
	// hook.
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	calls := 0
	migrationDownForTest = func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error) {
		calls++
		if calls == 1 {
			return provider.Down(ctx)
		}
		return nil, errors.New("synthetic second-step failure")
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	_, err = st.Downgrade(ctx, registryMax-2)
	var rb *DowngradeRollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want *DowngradeRollbackError", err)
	}
	if rb.From != registryMax || rb.Reversed != 1 {
		t.Fatalf("rollback = {From:%d Reversed:%d}, want {%d 1}", rb.From, rb.Reversed, registryMax)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != registryMax-1 {
		t.Fatalf("recorded version = %d, %v; want %d, one step reversed", got, err, registryMax-1)
	}
}

// Every error Downgrade raises after reading the applied version records that
// version on the failed command's event, and how far the reverse got, so a
// refusal's event says what it was decided from.
func TestDowngradeErrorsRecordTheVersionTheyWereDecidedFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)
	current, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() = %v", err)
	}
	for _, target := range []int64{current + 1, baselineVersion - 1} {
		_, err := st.Downgrade(ctx, target)
		var recorded model.RecordedError
		if !errors.As(err, &recorded) {
			t.Fatalf("Downgrade(%d) err = %v, want a model.RecordedError", target, err)
		}
		facts, counts := recorded.EventRecord()
		wantFacts := map[string]any{"downgrade.workspace_schema": current, "downgrade.snapshot": ""}
		if fmt.Sprint(facts) != fmt.Sprint(wantFacts) || counts["downgrade.reversed"] != 0 || len(counts) != 1 {
			t.Errorf("Downgrade(%d) record = %v %v; want %v reversed=0", target, facts, counts, wantFacts)
		}
	}
	rollback := &DowngradeRollbackError{From: 9, Reversed: 2, Snapshot: dbsnapshot.Snapshot{Name: "snap"}}
	facts, counts := rollback.EventRecord()
	if facts["downgrade.workspace_schema"] != int64(9) || facts["downgrade.snapshot"] != "snap" || counts["downgrade.reversed"] != 2 {
		t.Errorf("rollback record = %v %v; want workspace_schema=9 snapshot=snap reversed=2", facts, counts)
	}
}

// TestDowngradeIncompleteWhenGooseExhausted pins the no-silent-fallback
// guarantee for the registry-vs-target mismatch case: if goose returns
// ErrNoNextVersion while the recorded version is still above target, the
// loop must NOT report success — it raises DowngradeIncompleteError so the
// operator sees the gap.
func TestDowngradeIncompleteWhenGooseExhausted(t *testing.T) {
	// serial: no t.Parallel — installs the package-level migrationDownForTest
	// hook.
	ctx := context.Background()
	st, _ := openWorkspaceForDowngrade(t)
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}
	migrationDownForTest = func(ctx context.Context, _ *goose.Provider) (*goose.MigrationResult, error) {
		return nil, goose.ErrNoNextVersion
	}
	t.Cleanup(func() { migrationDownForTest = nil })

	_, err = st.Downgrade(ctx, registryMax-1)
	var rb *DowngradeRollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v, want *DowngradeRollbackError wrapping DowngradeIncompleteError", err)
	}
	var incomplete *DowngradeIncompleteError
	if !errors.As(rb.Cause, &incomplete) {
		t.Fatalf("rb.Cause = %v, want *DowngradeIncompleteError", rb.Cause)
	}
	if incomplete.Current != registryMax || incomplete.Target != registryMax-1 {
		t.Fatalf("incomplete = {Current:%d Target:%d}, want {%d %d}",
			incomplete.Current, incomplete.Target, registryMax, registryMax-1)
	}
}

// TestDowngradeSpanUnsupportedRefused pins the refusal for a workspace a newer
// lit migrated: this binary opens it (its baseline is intact) but does not
// carry the down-migrations above its own registry, so Downgrade refuses with
// the span and this binary's support, before any snapshot is taken.
func TestDowngradeSpanUnsupportedRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	ahead := stampGooseVersionAhead(t, ctx, doltRoot)
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() on a workspace past the registry = %v, want it to open", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() = %v", err)
	}

	before := snapshotCount(t, doltRoot)
	_, err = st.Downgrade(ctx, registryMax)
	var unsupported *DowngradeSpanUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *DowngradeSpanUnsupportedError", err)
	}
	want := DowngradeSpanUnsupportedError{Current: ahead, Target: registryMax, Support: version.SchemaSupport{Min: baselineVersion, Max: registryMax}}
	if *unsupported != want {
		t.Fatalf("err fields = %+v, want %+v", *unsupported, want)
	}
	if facts, _ := unsupported.EventRecord(); facts["downgrade.workspace_schema"] != ahead {
		t.Fatalf("record workspace_schema = %v, want %d", facts["downgrade.workspace_schema"], ahead)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("span-unsupported refusal took %d snapshot(s); want 0", delta)
	}
	if got, err := st.recordedMigrationVersion(ctx); err != nil || got != ahead {
		t.Fatalf("recorded version after refusal = %d, %v; want %d untouched", got, err, ahead)
	}
}

// TestIsDowngradeSnapshotNameSymmetry pins the disjointness of the two
// system-kind predicates: every migration snapshot name fails the downgrade
// predicate and vice versa. The CLI's user-snapshot classifier depends on
// these two predicates being non-overlapping — overlap would mean a
// downgrade snapshot is also classified as a migration snapshot, breaking
// the "each producer owns its own retention" invariant.
func TestIsDowngradeSnapshotNameSymmetry(t *testing.T) {
	t.Parallel()
	migName := "1700000000000000000-pre-migrate-1700000000000000001"
	dgName := "1700000000000000000-lit-downgrade-1700000000000000001"
	if !IsMigrationSnapshotName(migName) {
		t.Fatalf("IsMigrationSnapshotName(%q) = false, want true", migName)
	}
	if IsDowngradeSnapshotName(migName) {
		t.Fatalf("IsDowngradeSnapshotName(%q) = true, want false (overlap with migration kind)", migName)
	}
	if !IsDowngradeSnapshotName(dgName) {
		t.Fatalf("IsDowngradeSnapshotName(%q) = false, want true", dgName)
	}
	if IsMigrationSnapshotName(dgName) {
		t.Fatalf("IsMigrationSnapshotName(%q) = true, want false (overlap with downgrade kind)", dgName)
	}
}

// TestDowngradeUntouchedOpen pins the isolation acceptance: a workspace that
// rode Downgrade back to baseline re-opens cleanly — Open's migrate() sees no
// version-ahead condition and performs no work beyond the no-op classify path.
// This is the simulation of "downgraded workspace re-opens on the prior
// binary" called out in the ticket.
func TestDowngradeUntouchedOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) = %v", err)
	}
	current, err := first.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion(first) = %v", err)
	}
	// No-op downgrade (target == current) — proves the contract end-to-end
	// against the live registry without needing a Down section we don't have
	// in the embedded registry yet.
	if _, err := first.Downgrade(ctx, current); err != nil {
		t.Fatalf("Downgrade(no-op) = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) = %v", err)
	}

	second, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(second) after downgrade = %v", err)
	}
	defer second.Close()
	again, err := second.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion(second) = %v", err)
	}
	if again != current {
		t.Fatalf("recorded version after re-Open = %d, want %d (untouched)", again, current)
	}
}

// TestDowngradeRequiresGooseManaged pins the phase guard: a workspace whose
// classify step does not yield phaseManaged refuses without taking a
// snapshot. Open always converges to phaseManaged on success, so we exercise
// the guard by opening normally and then dropping goose_db_version — that
// reverts the workspace to a phaseAdopt-shaped classification (canonical
// tables present, no goose history), which Downgrade has nothing to act on.
func TestDowngradeRequiresGooseManaged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Drop goose_db_version so classify sees phaseAdopt (canonical shape
	// present, no goose table). Downgrade has nothing to reverse here.
	if _, err := st.db.ExecContext(ctx, `DROP TABLE `+gooseVersionTable); err != nil {
		t.Fatalf("drop goose table = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "test: drop goose table"); err != nil {
		t.Fatalf("commit drop = %v", err)
	}

	before := snapshotCount(t, doltRoot)
	_, err = st.Downgrade(ctx, baselineVersion)
	if err == nil {
		t.Fatal("Downgrade on non-managed workspace returned nil")
	}
	if !strings.Contains(err.Error(), "not goose-managed") {
		t.Fatalf("err = %v, want phase-guard message", err)
	}
	if delta := snapshotCount(t, doltRoot) - before; delta != 0 {
		t.Fatalf("phase-refusal took %d snapshot(s); want 0", delta)
	}
	// Sanity: ensure no DowngradeRollbackError is produced for refusals.
	var rb *DowngradeRollbackError
	if errors.As(err, &rb) {
		t.Fatalf("phase refusal must not produce DowngradeRollbackError: %v", rb)
	}
}

// Compile-time guards: the typed errors satisfy `error` and unwrap cleanly.
var (
	_ error = (*DowngradeTargetAheadError)(nil)
	_ error = (*DowngradeBelowBaselineError)(nil)
	_ error = (*DowngradeRollbackError)(nil)
	_ dbsnapshot.Snapshot
)
