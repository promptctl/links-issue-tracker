package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/dbsnapshot"
	"github.com/bmf/links-issue-tracker/internal/store/migrations"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

const migrationCheckpointPrefix = "pre-migrate"
const migrationCheckpointRetention = 5

// migrationUpByOneForTest, if non-nil, replaces provider.UpByOne(ctx) in
// applyPendingMigrations. Tests use this to inject migration failures without
// needing real failing migrations in the embedded registry.
var migrationUpByOneForTest func(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error)

// CheckpointResetError is returned when a migration body failure triggers an
// automatic Dolt checkpoint reset. The error message names the checkpoint so
// the operator can use it as a lightweight forensics anchor alongside the
// dbsnapshot layer described in the wrapping MigrationRollbackError.
//
// [LAW:single-enforcer] The recovery-instruction format lives here so every
// caller sees the same operator-facing words.
type CheckpointResetError struct {
	Version    int64
	Name       string
	Checkpoint Checkpoint
	Cause      error
}

func (e *CheckpointResetError) Error() string {
	if e.Version == 0 {
		return fmt.Sprintf(
			"migration failed (version unknown): %v\n\n"+
				"the working set was automatically reset to Dolt checkpoint %q\n"+
				"restore from the pre-migration recovery snapshot",
			e.Cause, e.Checkpoint.Name,
		)
	}
	return fmt.Sprintf(
		"migration v%d %q failed: %v\n\n"+
			"the working set was automatically reset to Dolt checkpoint %q\n"+
			"to retry after fixing the migration, clear the quarantine:\n"+
			"  DELETE FROM migration_quarantine WHERE version = %d",
		e.Version, e.Name, e.Cause, e.Checkpoint.Name, e.Version,
	)
}

func (e *CheckpointResetError) Unwrap() error { return e.Cause }

// QuarantineBlockError is returned when Open finds a pending migration version
// that has a quarantine record from a previous failed attempt.
//
// [LAW:single-enforcer] The quarantine-error format lives here so operator
// tooling and tests have one place to parse the recovery instructions.
type QuarantineBlockError struct {
	Version   int64
	Name      string
	ErrorText string // original failure message recorded at quarantine time
}

func (e *QuarantineBlockError) Error() string {
	return fmt.Sprintf(
		"migration v%d %q is quarantined after a previous failure:\n  %s\n\n"+
			"to recover, either:\n"+
			"  (a) restore from a dbsnapshot: lit snapshots restore <name>\n"+
			"  (b) clear the quarantine row (if transient): DELETE FROM migration_quarantine WHERE version = %d",
		e.Version, e.Name, e.ErrorText, e.Version,
	)
}

// UnsupportedSchemaVersionError is returned when Open finds a workspace whose
// schema is genuinely incompatible with this binary — the live tables do not
// match the binary's baseline shape. A workspace whose goose bookkeeping is
// merely ahead of the registry but whose application tables are intact does
// NOT yield this error; that case is auto-reconciled (see recoverAheadOfRegistry).
//
// [LAW:one-source-of-truth] MaxSupported is registryMaxVersion() — the same
// value that bounds "pending". There is no second "max supported" constant to
// drift from the registry, so no startup assertion is needed to keep them
// coherent: they are the same number.
//
// [LAW:types-are-the-program] MissingBaseline carries the schema gaps that
// classify the workspace as genuinely incompatible (vs. recoverable). Its
// presence is the discriminator: empty means the application schema is fine
// (and the binary should not be returning this error), non-empty names the
// specific tables/columns the binary cannot operate against.
type UnsupportedSchemaVersionError struct {
	WorkspaceVersion int64
	MaxSupported     int64
	MissingBaseline  []string
}

func (e *UnsupportedSchemaVersionError) Error() string {
	if len(e.MissingBaseline) == 0 {
		return fmt.Sprintf(
			"please upgrade lit (your workspace is at schema version %d; this binary supports up to %d)",
			e.WorkspaceVersion, e.MaxSupported,
		)
	}
	return fmt.Sprintf(
		"please upgrade lit (your workspace is at schema version %d; this binary supports up to %d; "+
			"the live schema is also missing baseline shape: %s)",
		e.WorkspaceVersion, e.MaxSupported, strings.Join(e.MissingBaseline, ", "),
	)
}

// gooseVersionTable is goose's history table; its presence is the discriminator
// between a goose-managed workspace and a pre-goose / fresh one.
const gooseVersionTable = "goose_db_version"

// baselineVersion is the version 00001_baseline.sql stamps. A pre-goose
// workspace already at the baseline shape is adopted by recording this version
// without re-running the CREATE TABLEs.
const baselineVersion = 1

// migrationPhase is the workspace's position relative to the goose registry,
// derived once from side-effect-free probes. The runner acts on the phase; it
// never re-derives state from scattered checks.
// [LAW:types-are-the-program] The phase is the discriminator that decides
// stamping vs applying; illegal mixed states (a partial pre-goose schema) are
// not a phase — they are a classification error that fails loudly.
type migrationPhase int

const (
	// phaseFresh: no goose table and no canonical tables. goose applies the
	// baseline (and any later migrations) from scratch.
	phaseFresh migrationPhase = iota
	// phaseAdopt: no goose table but the full canonical schema is present.
	// Stamp the baseline version, then apply any later migrations.
	phaseAdopt
	// phaseManaged: goose table present. Apply whatever versions exceed the
	// recorded one (possibly none).
	phaseManaged
)

// migrationState is the classified migration position plus the recorded version
// (meaningful only for phaseManaged).
type migrationState struct {
	phase           migrationPhase
	appliedVersion  int64
	registryMaxVers int64
}

// willMutate reports whether this Open will write. Fresh and adopt always
// write (baseline apply / version stamp); a managed workspace writes only when
// it trails the registry. The snapshot guard fires exactly when this is true.
func (st migrationState) willMutate() bool {
	switch st.phase {
	case phaseManaged:
		return st.appliedVersion < st.registryMaxVers
	default:
		return true
	}
}

// migrate is the single startup migration boundary. It owns the snapshot
// guard: runMigration takes exactly one recovery snapshot before its first
// write, migrate wraps any post-snapshot failure with the operator restore
// command, and prunes migration snapshots at the tail of a mutating Open.
//
// [LAW:single-enforcer] Store-level Open routes all schema convergence through
// this one boundary; the snapshot/prune budget lives here, not at callsites.
func (s *Store) migrate(ctx context.Context) error {
	guard := newSnapshotGuard(
		s.doltRootDir,
		migrationSnapshotsDir(s.doltRootDir),
		formatMigrationSnapshotLabel(time.Now()),
	)
	if err := s.runMigration(ctx, guard); err != nil {
		if snap, ok := guard.took(); ok {
			return &MigrationRollbackError{Snapshot: snap, Cause: err}
		}
		return err
	}
	if _, ok := guard.took(); ok {
		// [LAW:one-source-of-truth] Migration retention bounds migration
		// snapshots only; user snapshots share the directory under an
		// independent budget. IsMigrationSnapshotName is the kind discriminator.
		if err := dbsnapshot.PruneMatching(guard.snapshotsDir, migrationSnapshotRetention, IsMigrationSnapshotName); err != nil {
			return fmt.Errorf("prune migration snapshots: %w", err)
		}
	}
	return nil
}

// runMigration replaces the legacy scattered reconcile. It classifies the
// workspace once, snapshots before the first write, adopts a pre-goose
// workspace if needed, then applies pending migrations one Dolt commit each.
//
// [LAW:single-enforcer] One runner owns migration ordering and the snapshot/
// commit boundary; goose is its only changeset registry and no other code
// applies schema.
// [LAW:dataflow-not-control-flow] The same classify -> snapshot -> adopt ->
// apply sequence runs every Open; variability lives in the phase and the set
// of pending versions, not in whether stages execute.
func (s *Store) runMigration(ctx context.Context, guard *snapshotGuard) error {
	state, err := s.classifyMigrationState(ctx)
	if err != nil {
		return err
	}
	// [LAW:types-are-the-program] "Workspace stamped past the registry" is two
	// distinct states the binary must not collapse: (a) the live application
	// schema is intact and only goose's bookkeeping row is ahead — recoverable
	// without touching application data, and (b) the live schema itself is in
	// a shape this binary cannot operate against — genuinely incompatible.
	// recoverAheadOfRegistry distinguishes them and either reconciles
	// (case a) or refuses with MissingBaseline naming the gap (case b).
	if state.appliedVersion > state.registryMaxVers {
		return s.recoverAheadOfRegistry(ctx, guard, state)
	}
	if !state.willMutate() {
		return nil
	}
	// Fast-fail before the snapshot guard so a permanently-quarantined workspace
	// does not accumulate recovery snapshots on every Open. The check is a no-op
	// when the table does not exist (phaseFresh — the table is created later).
	//
	// [LAW:single-enforcer] The quarantine gate lives here and nowhere else; this
	// is the only site that calls checkPendingQuarantine.
	if err := s.quarantineFastFail(ctx, state); err != nil {
		return err
	}
	// [LAW:no-silent-fallbacks] Verify reconcile prerequisites BEFORE
	// the snapshot guard fires, so a workspace whose shape reconcile
	// cannot recover from does not accumulate a recovery snapshot per
	// Open. The check is gated to phaseAdopt because that is the only
	// phase where reconcile actually runs — phaseFresh has no issues
	// table to verify, and phaseManaged has already passed v1
	// adoption so its issues table shape is governed by the goose
	// registry, not by reconcile preconditions. Running the probe in
	// non-adopt phases would either no-op pointlessly or produce a
	// misleadingly-wrapped error.
	if state.phase == phaseAdopt {
		if err := s.verifyIssuesReconcilable(ctx); err != nil {
			return fmt.Errorf("reconcile pre-goose workspace: %w", err)
		}
	}
	if _, err := guard.ensure(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if hook := migrationPostSnapshotHookForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if state.phase == phaseAdopt {
		// [LAW:single-enforcer] Pre-goose workspaces at any historical
		// canonical shape forward-migrate through reconcileToBaseline
		// BEFORE the adoption stamp lands. The reconcile is idempotent:
		// a workspace already at v1 sees every probe return "present"
		// and reconcile no-ops; a workspace at an earlier shape (e.g.
		// missing issue_events or agent_prompt) gets its gaps filled.
		//
		// This is the recovery from commit 254f86b, which deleted the
		// reconcile and left pre-v1 workspaces bricked. The reconcile
		// is a HISTORICAL ARTIFACT — no new operations get added here.
		// Goose owns v1 → vN going forward.
		if _, err := s.reconcileToBaseline(ctx, guard); err != nil {
			return fmt.Errorf("reconcile pre-goose workspace: %w", err)
		}
		// [LAW:no-silent-fallbacks] After reconcile, verify the
		// workspace shape actually matches the baseline before
		// stamping. The reconcile's CREATE TABLE steps are gated on
		// table presence, NOT column presence — so a workspace that
		// has a malformed non-issues table (e.g. relations exists but
		// is missing the type column) would have reconcile skip the
		// CREATE and the malformed table would persist. Stamping v1
		// on that workspace would be a lie, recreating the PR #119
		// failure shape that adoption was supposed to prevent.
		// verifyBaselineShape compares against the baseline file and
		// names every remaining gap; if any gaps survive, refuse with
		// a structural error before the stamp lands.
		_, missing, err := s.verifyBaselineShape(ctx)
		if err != nil {
			return fmt.Errorf("verify post-reconcile baseline shape: %w", err)
		}
		if len(missing) > 0 {
			return fmt.Errorf(
				"post-reconcile workspace shape still differs from baseline "+
					"(remaining gaps: %s); reconcile cannot bring this workspace "+
					"to v1 — the shape is structurally beyond what pre-goose "+
					"reconcile can recover",
				strings.Join(missing, ", "),
			)
		}
		if err := s.adoptPreGooseWorkspace(ctx); err != nil {
			return err
		}
		// commitWorkingSet (not ...Once) so the adoption stamp gets the
		// transient-manifest retry wrapper. migrate() already holds the commit
		// lock, so the nested withCommitLock short-circuits acquisition.
		if err := s.commitWorkingSet(ctx, fmt.Sprintf("migrate: adopt pre-goose workspace at v%d", baselineVersion)); err != nil {
			return fmt.Errorf("commit adoption stamp: %w", err)
		}
	}
	// Ensure the quarantine table exists and is committed before the Dolt
	// checkpoint is taken inside applyPendingMigrations. This ordering
	// guarantees the table survives a checkpoint reset: after reset the
	// working set reverts to the checkpoint state, which already includes
	// the committed quarantine table.
	//
	// [LAW:single-enforcer] Quarantine table creation is decoupled from goose
	// migrations so a goose rollback cannot erase the table it depends on.
	if err := s.ensureQuarantineTable(ctx); err != nil {
		return err
	}
	if err := s.commitWorkingSet(ctx, "migrate: ensure migration_quarantine table"); err != nil {
		return fmt.Errorf("commit quarantine table: %w", err)
	}
	return s.applyPendingMigrations(ctx)
}

// applyPendingMigrations runs each pending migration through goose and records
// one Dolt commit per applied migration. Before the first migration runs it
// creates a Dolt checkpoint so a failure can reset the working set. On
// failure it quarantines the failed version (persisted after the reset) and
// returns a CheckpointResetError naming both recovery layers.
//
// [LAW:single-enforcer] The checkpoint and per-migration commit boundary live
// here; no other code drives goose.Up or touches Dolt branches for migration
// purposes. The quarantine check lives in quarantineFastFail (called before
// the snapshot guard in runMigration) so the gate fires without a snapshot.
// [LAW:dataflow-not-control-flow] The same sequence (checkpoint → goose loop
// → prune) runs on every call; variability lives in the applied-vs-registry
// set, not in whether stages execute.
func (s *Store) applyPendingMigrations(ctx context.Context) error {
	// Construct the provider before creating the checkpoint so a provider
	// construction failure leaves no orphaned branch behind.
	provider, err := newGooseProvider(s.db)
	if err != nil {
		return fmt.Errorf("construct migration provider: %w", err)
	}

	// Create Dolt checkpoint before any mutation. The quarantine table is
	// already committed (ensureQuarantineTable ran before this call), so
	// the checkpoint state includes it — the table survives a hard reset.
	checkpoint, err := s.CreateCheckpoint(ctx, migrationCheckpointPrefix)
	if err != nil {
		return fmt.Errorf("create migration checkpoint: %w", err)
	}
	upByOne := func(ctx context.Context) (*goose.MigrationResult, error) {
		if hook := migrationUpByOneForTest; hook != nil {
			return hook(ctx, provider)
		}
		return provider.UpByOne(ctx)
	}

	for {
		result, gooseErr := upByOne(ctx)
		if errors.Is(gooseErr, goose.ErrNoNextVersion) {
			// Success: prune old checkpoints to the retention count.
			if err := s.PruneCheckpoints(ctx, migrationCheckpointPrefix, migrationCheckpointRetention); err != nil {
				return fmt.Errorf("prune migration checkpoints: %w", err)
			}
			return nil
		}
		if gooseErr != nil {
			cpErr := s.handleMigrationFailure(ctx, result, gooseErr, checkpoint)
			// Prune on the failure path too: normal failures insert a quarantine row
			// so future Opens are blocked before creating a new checkpoint, but
			// nil-result failures skip quarantine insertion, leaving the door open
			// for repeated manual retries to accumulate branches. Error ignored —
			// the migration failure is the primary concern.
			_ = s.PruneCheckpoints(ctx, migrationCheckpointPrefix, migrationCheckpointRetention)
			return cpErr
		}
		// commitWorkingSet (not ...Once) so each migration commit gets the
		// transient-manifest retry — startup migration is a critical path and a
		// recoverable Dolt manifest blip must not brick Open. The commit lock is
		// already held, so re-entering withCommitLock short-circuits.
		if err := s.commitWorkingSet(ctx, migrationCommitMessage(result)); err != nil {
			return fmt.Errorf("commit migration v%d: %w", result.Source.Version, err)
		}
	}
}

// handleMigrationFailure resets the working set to the pre-migrate checkpoint,
// records the quarantine row in the post-reset database, and returns a
// CheckpointResetError naming both recovery surfaces.
//
// Ordering: reset first, quarantine second — the reset discards all working-set
// changes since the checkpoint, but the quarantine table itself was committed
// before the checkpoint, so it survives and the post-reset INSERT lands cleanly.
func (s *Store) handleMigrationFailure(ctx context.Context, result *goose.MigrationResult, cause error, checkpoint Checkpoint) error {
	var version int64
	var name string
	if result != nil && result.Source != nil {
		version = result.Source.Version
		name = filepath.Base(result.Source.Path)
	}

	if resetErr := s.ResetToCheckpoint(ctx, checkpoint.Name); resetErr != nil {
		return fmt.Errorf(
			"migration v%d failed and Dolt reset to %q failed (%v); restore from dbsnapshot. Root cause: %w",
			version, checkpoint.Name, resetErr, cause,
		)
	}

	if version > 0 {
		if recordErr := s.recordQuarantine(ctx, version, name, cause.Error()); recordErr != nil {
			return fmt.Errorf(
				"migration v%d failed (reset to %q); quarantine insert failed (%v); restore from dbsnapshot. Root cause: %w",
				version, checkpoint.Name, recordErr, cause,
			)
		}
		if commitErr := s.commitWorkingSet(ctx, fmt.Sprintf("migrate: quarantine v%d %s", version, name)); commitErr != nil {
			return fmt.Errorf(
				"migration v%d failed (reset to %q); quarantine commit failed (%v); restore from dbsnapshot. Root cause: %w",
				version, checkpoint.Name, commitErr, cause,
			)
		}
	}

	return &CheckpointResetError{
		Version:    version,
		Name:       name,
		Checkpoint: checkpoint,
		Cause:      cause,
	}
}

// ensureQuarantineTable creates migration_quarantine if it does not already
// exist. The table is created outside the goose migration batch so a goose
// rollback cannot erase it.
//
// [LAW:one-source-of-truth] migration_quarantine is the sole authority on
// "failed and not to be retried"; it is owned by workspace bootstrap, not
// by the goose migration log.
func (s *Store) ensureQuarantineTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migration_quarantine (
		version    BIGINT NOT NULL,
		name       TEXT NOT NULL,
		error_text TEXT NOT NULL,
		created_at VARCHAR(64) NOT NULL,
		PRIMARY KEY (version)
	)`)
	if err != nil {
		return fmt.Errorf("ensure migration_quarantine table: %w", err)
	}
	return nil
}

// quarantineFastFail checks for blocking quarantine rows before the snapshot
// guard fires. It is a no-op when migration_quarantine does not yet exist
// (phaseFresh — the table is created later inside runMigration).
//
// For phaseAdopt, adoptPreGooseWorkspace will stamp baselineVersion before any
// migration runs, so the effective applied version is baselineVersion — a
// quarantine row for the baseline itself must not block after adoption confirms
// the schema is present.
//
// [LAW:single-enforcer] This is the only call site for checkPendingQuarantine.
// [LAW:dataflow-not-control-flow] The table-exists result is the data that
// decides behavior; the check always runs when the table is present.
func (s *Store) quarantineFastFail(ctx context.Context, state migrationState) error {
	exists, err := s.tableExists(ctx, "migration_quarantine")
	if err != nil {
		return fmt.Errorf("migrate: probe quarantine table: %w", err)
	}
	if !exists {
		return nil
	}
	effectiveApplied := state.appliedVersion
	if state.phase == phaseAdopt {
		effectiveApplied = baselineVersion
	}
	return s.checkPendingQuarantine(ctx, effectiveApplied)
}

// checkPendingQuarantine returns a QuarantineBlockError if any migration
// version greater than appliedVersion has a quarantine record.
func (s *Store) checkPendingQuarantine(ctx context.Context, appliedVersion int64) error {
	var version int64
	var name, errorText string
	err := s.db.QueryRowContext(ctx,
		`SELECT version, name, error_text FROM migration_quarantine WHERE version > ? ORDER BY version LIMIT 1`,
		appliedVersion,
	).Scan(&version, &name, &errorText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check pending quarantine: %w", err)
	}
	return &QuarantineBlockError{Version: version, Name: name, ErrorText: errorText}
}

// recordQuarantine upserts a quarantine row for the given migration version.
// ON DUPLICATE KEY UPDATE handles the case where a previous run already
// recorded a quarantine row for this version (e.g., operator cleared and
// retried, failed again).
func (s *Store) recordQuarantine(ctx context.Context, version int64, name, errorText string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE name = VALUES(name), error_text = VALUES(error_text), created_at = VALUES(created_at)`,
		version, name, errorText, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("record quarantine for v%d: %w", version, err)
	}
	return nil
}

// recoverAheadOfRegistry handles the case where goose_db_version records a
// schema version higher than this binary's registry can produce. It owns the
// fork between recoverable (live application schema is intact — trim the
// bookkeeping log to the registry max so this binary can operate) and
// genuinely incompatible (live schema is missing baseline shape this binary
// requires — refuse).
//
// The "stamped ahead by a non-master binary that the workspace then needs to
// be opened by master" failure shape is exactly case (a): the unmerged binary
// added migrations that recorded their version in goose_db_version, but the
// live tables it created are either also created programmatically by the
// current binary (migration_quarantine) or are orphans the current binary
// does not reference. Application data — the rows in issues / comments / etc.
// — is never touched. The only write is DELETE on the goose bookkeeping log.
//
// [LAW:types-are-the-program] The discriminator is verifyBaselineShape, the
// same predicate phaseAdopt uses to decide "is this workspace really at
// baseline." Reusing it makes the boundary unambiguous: either every baseline
// table/column the binary needs is present (recover), or some is missing
// (refuse with MissingBaseline named).
//
// [LAW:single-enforcer] All schema convergence flows through runMigration;
// this function is the sole recovery path for the ahead-of-registry case
// and is called only from runMigration.
//
// [LAW:dataflow-not-control-flow] The verify result (present count and
// missing list) is the data that drives the branch; the function does not
// take a "mode" parameter — variability lives in the workspace's actual
// shape, not in flags.
func (s *Store) recoverAheadOfRegistry(ctx context.Context, guard *snapshotGuard, state migrationState) error {
	present, missing, err := s.verifyBaselineShape(ctx)
	if err != nil {
		return err
	}
	if present == 0 || len(missing) > 0 {
		return &UnsupportedSchemaVersionError{
			WorkspaceVersion: state.appliedVersion,
			MaxSupported:     state.registryMaxVers,
			MissingBaseline:  missing,
		}
	}
	// Snapshot-before-mutate: this IS a mutation (goose_db_version DELETE), so
	// the same guarantee that protects forward migrations protects this
	// reconciliation. The guard is idempotent — migrate() owns retention.
	if _, err := guard.ensure(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM `+gooseVersionTable+` WHERE version_id > ?`,
		state.registryMaxVers,
	); err != nil {
		return fmt.Errorf("reconcile ahead goose log: %w", err)
	}
	// [LAW:types-are-the-program] "Reconciled" means recordedMigrationVersion()
	// equals registryMaxVers, not "rows above registryMaxVers were deleted." A
	// corrupted goose history (e.g., only an ahead row, no rows for baseline)
	// would leave the table empty after the DELETE — the next Open would see
	// applied=0 and try to re-baseline against an already-initialized schema.
	// Restamp registryMaxVers if it is not the post-DELETE max, so the type
	// of the post-reconcile state is "managed at registryMaxVers" — the only
	// state runMigration's other branches handle correctly.
	postDelete, err := s.recordedMigrationVersion(ctx)
	if err != nil {
		return fmt.Errorf("read goose version after reconcile: %w", err)
	}
	if postDelete < state.registryMaxVers {
		gooseStore, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
		if err != nil {
			return fmt.Errorf("reconcile: construct goose store: %w", err)
		}
		if err := gooseStore.Insert(ctx, s.db, database.InsertRequest{Version: state.registryMaxVers}); err != nil {
			return fmt.Errorf("reconcile: restamp registry max v%d: %w", state.registryMaxVers, err)
		}
		// Final verification: the invariant must hold or the next Open lands in
		// an inconsistent state. Fail loudly here while the operator still has
		// context, instead of mutating the workspace into a worse position.
		verified, err := s.recordedMigrationVersion(ctx)
		if err != nil {
			return fmt.Errorf("verify goose version after reconcile: %w", err)
		}
		if verified != state.registryMaxVers {
			return fmt.Errorf("reconcile: post-restamp goose version is v%d, want v%d", verified, state.registryMaxVers)
		}
	}
	if err := s.commitWorkingSet(ctx,
		fmt.Sprintf("migrate: reconcile ahead-of-registry goose log to v%d (was v%d)",
			state.registryMaxVers, state.appliedVersion),
	); err != nil {
		return fmt.Errorf("commit ahead-goose reconciliation: %w", err)
	}
	return nil
}

// classifyMigrationState derives the workspace phase using only reads,
// so a no-op Open performs no writes (it must take no snapshot).
//
// Three phases:
//   - phaseManaged: goose_db_version table present; goose owns the workspace.
//   - phaseFresh:   no goose table AND no canonical tables; brand new.
//   - phaseAdopt:   no goose table BUT at least one canonical table present.
//                   The workspace is pre-goose at SOME historical canonical
//                   shape (current or earlier). reconcileToBaseline (a
//                   resurrected, idempotent, probe-driven forward migrator)
//                   brings any earlier shape forward to v1 before adoption
//                   stamps. There is no "partial-and-illegal" refusal —
//                   any presence of canonical tables means "pre-goose
//                   workspace, reconcile-then-adopt."
//
// [LAW:types-are-the-program] Three phases, each with a forward path. No
// refusal branch. The "partial schema, restore or recreate" failure mode
// the prior implementation had — which destroyed real user data with old
// canonical shapes — does not exist by construction here.
//
// [LAW:dataflow-not-control-flow] The classify function reads facts about
// the workspace; the runner reacts to them. No flags, no modes, no
// "what kind of corruption is this" guessing.
func (s *Store) classifyMigrationState(ctx context.Context) (migrationState, error) {
	registryMax, err := registryMaxVersion()
	if err != nil {
		return migrationState{}, err
	}
	// [LAW:types-are-the-program] Disk shape outranks goose bookkeeping
	// when the two disagree. issue_history is a pre-goose-only table —
	// the canonical baseline does not create it; reconcileToBaseline
	// drops it as part of the legacy→v1 bridge. Its presence is
	// conclusive evidence the workspace last reached a pre-goose
	// canonical shape, regardless of what goose_db_version claims.
	//
	// This probe re-routes "buggy older binary fabricated goose rows on
	// a pre-goose workspace" — which used to trap in phaseManaged with
	// recoverAheadOfRegistry refusal — back into phaseAdopt where the
	// bridge can run. The fabricated rows are wiped by reconcile and
	// adoption restamps cleanly.
	//
	// [LAW:one-source-of-truth] The live schema is canonical; the
	// goose log is derived state that should reflect it. When they
	// disagree, the schema wins and the log is reconciled to match.
	legacyMarker, err := s.tableExists(ctx, "issue_history")
	if err != nil {
		return migrationState{}, err
	}
	if legacyMarker {
		return migrationState{phase: phaseAdopt, registryMaxVers: registryMax}, nil
	}
	gooseManaged, err := s.tableExists(ctx, gooseVersionTable)
	if err != nil {
		return migrationState{}, err
	}
	if gooseManaged {
		applied, err := s.recordedMigrationVersion(ctx)
		if err != nil {
			return migrationState{}, err
		}
		return migrationState{phase: phaseManaged, appliedVersion: applied, registryMaxVers: registryMax}, nil
	}
	present, _, err := s.verifyBaselineShape(ctx)
	if err != nil {
		return migrationState{}, err
	}
	if present == 0 {
		return migrationState{phase: phaseFresh, registryMaxVers: registryMax}, nil
	}
	return migrationState{phase: phaseAdopt, registryMaxVers: registryMax}, nil
}

// adoptPreGooseWorkspace records the baseline version for a workspace already
// at the canonical shape, then removes the superseded legacy schema_version
// key so goose_db_version is the sole authority on "what's applied".
//
// [LAW:one-source-of-truth] After adoption, goose_db_version owns applied-state;
// the legacy meta.schema_version key is deleted so two authorities cannot
// coexist and drift.
func (s *Store) adoptPreGooseWorkspace(ctx context.Context) error {
	store, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
	if err != nil {
		return fmt.Errorf("adopt: construct goose store: %w", err)
	}
	if err := store.CreateVersionTable(ctx, s.db); err != nil {
		return fmt.Errorf("adopt: create version table: %w", err)
	}
	if err := store.Insert(ctx, s.db, database.InsertRequest{Version: baselineVersion}); err != nil {
		return fmt.Errorf("adopt: stamp baseline version: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE meta_key = 'schema_version'`); err != nil {
		return fmt.Errorf("adopt: drop legacy schema_version key: %w", err)
	}
	return nil
}

// recordedMigrationVersion returns goose's recorded version, or 0 when none has
// been applied yet.
func (s *Store) recordedMigrationVersion(ctx context.Context) (int64, error) {
	store, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
	if err != nil {
		return 0, fmt.Errorf("construct goose store: %w", err)
	}
	version, err := store.GetLatestVersion(ctx, s.db)
	if err != nil {
		if errors.Is(err, database.ErrVersionNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("read recorded migration version: %w", err)
	}
	return version, nil
}

// verifyBaselineShape compares the live workspace against the baseline schema
// parsed from 00001_baseline.sql. It returns how many baseline tables are
// present and a list of every shape gap: a fully-absent table is reported as
// "<table>", a present table missing a column as "<table>.<column>".
//
// Checking column presence (not just table presence) is what makes "adoptable"
// mean "actually at baseline": a pre-goose workspace can carry every table yet
// still be pre-converged (e.g. issue_events.assignee never renamed to actor, or
// issues missing topic), and stamping such a workspace at v1 would permanently
// mark an incompatible schema as baseline — the PR #119 failure shape.
//
// [LAW:one-source-of-truth] The expected shape is parsed from the same baseline
// file goose applies; there is no hand-maintained table/column list to drift.
// Column NAMES are compared (not types/constraints): identifiers survive Dolt's
// DDL round-trip verbatim, so name presence is a robust discriminator without
// the rewrite brittleness that full-text constraint matching suffers. Exact
// shape (types, constraints, indexes) is the drift canary's job (sxsk.4).
func (s *Store) verifyBaselineShape(ctx context.Context) (present int, missing []string, err error) {
	schema, err := baselineSchema()
	if err != nil {
		return 0, nil, err
	}
	for _, table := range sortedKeys(schema) {
		actual, err := s.tableColumns(ctx, table)
		if err != nil {
			return 0, nil, err
		}
		if len(actual) == 0 {
			missing = append(missing, table)
			continue
		}
		present++
		for _, column := range schema[table] {
			if !actual[column] {
				missing = append(missing, table+"."+column)
			}
		}
	}
	return present, missing, nil
}

// tableColumns returns the set of column names a table has in the active
// database. An absent table yields an empty set.
func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table,
	)
	if err != nil {
		return nil, fmt.Errorf("probe columns of %q: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column of %q: %w", table, err)
		}
		columns[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns of %q: %w", table, err)
	}
	return columns, nil
}

// tableExists reports whether a table is present in the active database.
func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var marker int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1`,
		table,
	).Scan(&marker)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("probe table %q: %w", table, err)
}

// newGooseProvider builds a goose provider over the embedded registry. mysql
// dialect: Dolt speaks the MySQL protocol.
func newGooseProvider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
}

// registryMaxVersion is the highest version in the embedded registry. It bounds
// "pending" without touching the database.
func registryMaxVersion() (int64, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("read migration registry: %w", err)
	}
	var versions []int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		v, ok := parseMigrationVersion(entry.Name())
		if !ok {
			return 0, fmt.Errorf("migration file %q does not begin with a numeric version", entry.Name())
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		return 0, errors.New("migration registry is empty")
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions[len(versions)-1], nil
}

// parseMigrationVersion extracts the leading numeric version from a goose
// migration filename (e.g. "00002_add_foo.sql" -> 2).
func parseMigrationVersion(name string) (int64, bool) {
	base := filepath.Base(name)
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, false
	}
	var version int64
	if _, err := fmt.Sscanf(base[:idx], "%d", &version); err != nil {
		return 0, false
	}
	return version, true
}

// migrationCommitMessage is the one-line Dolt commit message for an applied
// migration: `migrate: v<N> <file>`.
func migrationCommitMessage(result *goose.MigrationResult) string {
	return fmt.Sprintf("migrate: v%d %s", result.Source.Version, filepath.Base(result.Source.Path))
}

// baselineSchema parses the embedded baseline migration into the table->columns
// shape it creates — the single oracle for what "at baseline" means. The same
// file goose applies on a fresh workspace defines what adoption must verify on
// a pre-goose one.
func baselineSchema() (map[string][]string, error) {
	name, err := baselineFileName()
	if err != nil {
		return nil, err
	}
	data, err := migrations.FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read baseline migration %q: %w", name, err)
	}
	schema := parseCreateTableColumns(gooseUpSection(string(data)))
	if len(schema) == 0 {
		return nil, fmt.Errorf("baseline migration %q defines no tables", name)
	}
	return schema, nil
}

// baselineFileName is the registry file whose version is baselineVersion.
func baselineFileName() (string, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("read migration registry: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if v, ok := parseMigrationVersion(entry.Name()); ok && v == baselineVersion {
			return entry.Name(), nil
		}
	}
	return "", fmt.Errorf("no baseline migration (v%d) found in registry", baselineVersion)
}

// gooseUpSection returns the SQL between the goose Up and Down markers, so the
// parser never reads the Down (DROP TABLE) statements as table definitions.
func gooseUpSection(sql string) string {
	lower := strings.ToLower(sql)
	up := strings.Index(lower, "-- +goose up")
	if up < 0 {
		return sql
	}
	body := sql[up:]
	if down := strings.Index(strings.ToLower(body), "-- +goose down"); down >= 0 {
		return body[:down]
	}
	return body
}

// parseCreateTableColumns extracts table -> column-names from CREATE TABLE
// statements. It reads only column identifiers (the first token of each
// top-level item that is not a table-level constraint keyword); CREATE INDEX
// and everything else is ignored. ASCII-lowercasing preserves byte indices, so
// the case-insensitive keyword scan and the original-text slicing stay aligned.
func parseCreateTableColumns(sql string) map[string][]string {
	out := map[string][]string{}
	lower := strings.ToLower(sql)
	const kw = "create table"
	for pos := 0; ; {
		i := strings.Index(lower[pos:], kw)
		if i < 0 {
			break
		}
		cursor := pos + i + len(kw)
		name, afterName := firstIdentifier(sql[cursor:])
		open := strings.IndexByte(afterName, '(')
		if name == "" || open < 0 {
			pos = cursor
			continue
		}
		consumedToName := len(sql[cursor:]) - len(afterName)
		body, blockLen := parenBlock(afterName[open:])
		out[strings.ToLower(name)] = columnNames(body)
		pos = cursor + consumedToName + open + blockLen
	}
	return out
}

// columnNames returns the column identifiers in a CREATE TABLE body, skipping
// table-level constraint clauses.
func columnNames(body string) []string {
	var cols []string
	for _, item := range splitTopLevel(body) {
		name, _ := firstIdentifier(item)
		if name == "" || isConstraintKeyword(name) {
			continue
		}
		cols = append(cols, strings.ToLower(name))
	}
	return cols
}

// splitTopLevel splits a CREATE TABLE body at depth-0, unquoted commas, so a
// CHECK clause's internal commas (inside parens or string literals) do not
// fragment a single item.
func splitTopLevel(body string) []string {
	var parts []string
	depth, inQuote, start := 0, false, 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, body[start:])
}

// parenBlock takes a string beginning with '(' and returns the content between
// it and its matching ')', plus the total bytes consumed (including both
// parens). Quote- and depth-aware. An unbalanced input yields an empty body.
func parenBlock(s string) (string, int) {
	depth, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '\'':
			inQuote = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], i + 1
			}
		}
	}
	return "", len(s)
}

// firstIdentifier returns the leading SQL identifier (backticks stripped) and
// the remainder after it, skipping leading whitespace.
func firstIdentifier(s string) (string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	start := i
	for i < len(s) && (isIdentByte(s[i]) || s[i] == '`') {
		i++
	}
	return strings.Trim(s[start:i], "`"), s[i:]
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// isConstraintKeyword reports whether a CREATE TABLE item's leading token names
// a table-level constraint clause rather than a column.
func isConstraintKeyword(token string) bool {
	switch strings.ToUpper(token) {
	case "CONSTRAINT", "PRIMARY", "FOREIGN", "KEY", "CHECK", "UNIQUE", "INDEX":
		return true
	default:
		return false
	}
}

// sortedKeys returns the map keys in deterministic order so adoption probing and
// error messages are stable across runs.
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
