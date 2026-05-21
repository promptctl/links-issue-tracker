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

// gooseVersionTable is goose's history table; its presence is the discriminator
// between a goose-managed workspace and a pre-goose / fresh one.
const gooseVersionTable = "goose_db_version"

// baselineVersion is the version 00001_baseline.sql stamps. A pre-goose
// workspace already at the baseline shape is adopted by recording this version
// without re-running the CREATE TABLEs.
const baselineVersion = 1

// baselineTables is the canonical table set 00001_baseline.sql creates. It is
// the expected-shape oracle for adoption: a pre-goose workspace must have all
// of these to be stamped at baseline.
//
// [LAW:one-source-of-truth] baseline.sql defines the schema; this list mirrors
// the table set it produces. The two are kept coherent by convention today;
// the planned drift canary (links-migrate-frame-sxsk.4) will enforce it
// mechanically once it lands.
var baselineTables = []string{
	"meta", "issues", "relations", "comments", "labels", "issue_events", "issue_event_changes",
}

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
	if !state.willMutate() {
		return nil
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
		if err := s.adoptPreGooseWorkspace(ctx); err != nil {
			return err
		}
		if err := s.commitWorkingSetOnce(ctx, fmt.Sprintf("migrate: adopt pre-goose workspace at v%d", baselineVersion)); err != nil {
			return fmt.Errorf("commit adoption stamp: %w", err)
		}
	}
	return s.applyPendingMigrations(ctx)
}

// applyPendingMigrations runs each pending migration through goose and records
// one Dolt commit per applied migration, so `dolt log` carries one entry per
// schema version.
func (s *Store) applyPendingMigrations(ctx context.Context) error {
	provider, err := newGooseProvider(s.db)
	if err != nil {
		return fmt.Errorf("construct migration provider: %w", err)
	}
	for {
		result, err := provider.UpByOne(ctx)
		if errors.Is(err, goose.ErrNoNextVersion) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply migration: %w", err)
		}
		if err := s.commitWorkingSetOnce(ctx, migrationCommitMessage(result)); err != nil {
			return fmt.Errorf("commit migration v%d: %w", result.Source.Version, err)
		}
	}
}

// classifyMigrationState derives the workspace phase using only reads, so a
// no-op Open performs no writes (it must take no snapshot). It refuses a
// partial pre-goose schema — the failure shape PR #119's adoption swallowed.
func (s *Store) classifyMigrationState(ctx context.Context) (migrationState, error) {
	registryMax, err := registryMaxVersion()
	if err != nil {
		return migrationState{}, err
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
	present, missing, err := s.partitionBaselineTables(ctx)
	if err != nil {
		return migrationState{}, err
	}
	if present == 0 {
		return migrationState{phase: phaseFresh, registryMaxVers: registryMax}, nil
	}
	if len(missing) > 0 {
		return migrationState{}, fmt.Errorf(
			"workspace has a partial schema (missing tables: %s) and no goose history; "+
				"this workspace is not safely adoptable — restore it from a snapshot or recreate it",
			strings.Join(missing, ", "),
		)
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

// partitionBaselineTables returns how many baseline tables are present and the
// names of any that are missing.
func (s *Store) partitionBaselineTables(ctx context.Context) (present int, missing []string, err error) {
	for _, table := range baselineTables {
		exists, err := s.tableExists(ctx, table)
		if err != nil {
			return 0, nil, err
		}
		if exists {
			present++
			continue
		}
		missing = append(missing, table)
	}
	return present, missing, nil
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
