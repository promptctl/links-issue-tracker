package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pressly/goose/v3"

	"github.com/bmf/links-issue-tracker/internal/store/migrations"
)

// gooseDialect is the SQL dialect goose uses against Dolt. Dolt speaks the
// MySQL wire protocol, so the MySQL querier produces the right DDL/DML.
const gooseDialect = goose.DialectMySQL

// gooseVersionTable is the table goose maintains its applied-migration history
// in. Spelled out here so adoptPreGooseWorkspace can reference the same name
// when stamping pre-goose workspaces.
const gooseVersionTable = "goose_db_version"

// baselineVersion is the version_id that 00001_baseline.sql registers as. Pre-
// goose workspaces are stamped at this version so goose treats the baseline as
// already applied and skips it. [LAW:one-source-of-truth] this constant and
// the migration's filename prefix are the two writers of "what version is the
// baseline"; both must move together if we ever renumber the file.
const baselineVersion = 1

// preMigrateCheckpointPrefix names the safety-branch family the runner
// creates before every Open's migration step. Listed here, not in the
// checkpoint primitive, so the primitive remains migration-agnostic.
const preMigrateCheckpointPrefix = "pre-migrate"

// preMigrateCheckpointRetain is the retention budget for safety branches.
// Five was the spec's choice; large enough to walk back across a small
// burst of bad migrations, small enough to keep the branch list scannable.
const preMigrateCheckpointRetain = 5

// MigrationError is the typed failure callers receive when the runner had to
// auto-revert a migration (or refused to start one). Phase identifies which
// step failed; Version is the migration version that was running (0 if the
// failure was not tied to a specific version, e.g., checkpoint or provider
// construction). Cause is the underlying error and is unwrappable.
type MigrationError struct {
	Phase   string
	Version int64
	Cause   error
}

func (e *MigrationError) Error() string {
	if e.Version > 0 {
		return fmt.Sprintf("migration phase %s (version %d): %v", e.Phase, e.Version, e.Cause)
	}
	return fmt.Sprintf("migration phase %s: %v", e.Phase, e.Cause)
}

func (e *MigrationError) Unwrap() error { return e.Cause }

// migrationEventWriter is where the runner emits structured-ish event lines.
// .7 (structured stderr events) will replace the current `name k=v` plain
// text with JSON; until then, this hook lets tests capture and assert on the
// rendered output.
var migrationEventWriter io.Writer = os.Stderr

// extraMigrationProviderOptions is a test-only seam: when non-nil, the
// runner appends the returned options to NewProvider. Production never sets
// this. Lives in production code (not _test.go) so multiple test files can
// share the hook; reset between tests via t.Cleanup.
//
// [LAW:no-shared-mutable-globals] Single owner (tests). Production code
// never assigns. Documented contract: nil except inside a test that opted
// in via this hook.
var extraMigrationProviderOptions func() []goose.ProviderOption

// emitMigrationEvent writes one line of `name k1=v1 k2=v2`, keys sorted so
// tests can match exact strings. [LAW:single-enforcer] One emission helper —
// every event in the runner routes through here.
func emitMigrationEvent(name string, fields map[string]string) {
	var b strings.Builder
	b.WriteString(name)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, fields[k])
	}
	fmt.Fprintln(migrationEventWriter, b.String())
}

// runMigrations brings the workspace's schema to the latest registered goose
// version under the protection of a pre-migrate safety branch. Returns true
// if any state changed (so the caller can decide whether to commit the
// working set).
//
// Three workspace shapes converge through this function:
//   - fresh (no application tables, no goose_db_version) → goose runs baseline.
//   - pre-goose (application tables exist, no goose_db_version) → adoption
//     reconciles the legacy schema then stamps baseline as applied.
//   - already-on-goose → goose runs any pending migrations beyond baseline.
//
// [LAW:dataflow-not-control-flow] Same operations every Open: create
// checkpoint, read quarantine, adopt, build provider, Up, advance floor.
// Variability is in the data — what's pending, what's quarantined, whether
// adoption fires — never in whether each step executes.
// [LAW:single-enforcer] Auto-revert is the only writer of "undo a partially
// applied migration"; manual recovery (`lit doctor --reset-to-pre-migration`)
// reuses the same primitives but as a separate code path on a separate
// trigger.
func (s *Store) runMigrations(ctx context.Context) (bool, error) {
	safety, err := s.CreateCheckpoint(ctx, preMigrateCheckpointPrefix, preMigrateCheckpointRetain)
	if err != nil {
		return false, &MigrationError{Phase: "checkpoint", Cause: fmt.Errorf("create pre-migrate safety branch: %w", err)}
	}
	emitMigrationEvent("safety_branch.created", map[string]string{
		"branch": safety.Name,
		"commit": safety.CommitSHA,
	})

	quarantined, err := readQuarantinedVersions(ctx, s.db)
	if err != nil {
		return false, &MigrationError{Phase: "quarantine_read", Cause: err}
	}
	for _, v := range quarantined {
		emitMigrationEvent("migrate.skipped_quarantined", map[string]string{
			"version": fmt.Sprintf("%d", v),
		})
	}

	adopted, err := s.adoptPreGooseWorkspace(ctx)
	if err != nil {
		return false, s.revertWithQuarantine(ctx, safety, "adoption", 0, fmt.Errorf("adopt pre-goose workspace: %w", err))
	}

	opts := []goose.ProviderOption{}
	if len(quarantined) > 0 {
		opts = append(opts, goose.WithExcludeVersions(quarantined))
	}
	if extraMigrationProviderOptions != nil {
		opts = append(opts, extraMigrationProviderOptions()...)
	}
	provider, err := goose.NewProvider(gooseDialect, s.db, migrations.FS, opts...)
	if err != nil {
		return false, s.revertWithQuarantine(ctx, safety, "provider", 0, fmt.Errorf("build goose provider: %w", err))
	}
	results, err := provider.Up(ctx)
	if err != nil {
		version := versionFromGooseError(err)
		return false, s.revertWithQuarantine(ctx, safety, "up", version, fmt.Errorf("apply pending migrations: %w", err))
	}
	settled := collectSettledVersions(adopted, results)
	floorChanged, err := s.advanceCompatFloor(ctx, settled)
	if err != nil {
		return false, s.revertWithQuarantine(ctx, safety, "advance_floor", 0, fmt.Errorf("advance code_compat_floor: %w", err))
	}
	return adopted || len(results) > 0 || floorChanged, nil
}

// revertWithQuarantine resets to the safety branch and quarantines the
// failed version (if known) so subsequent Opens skip it. Returns the typed
// MigrationError describing the original failure. If the reset itself fails,
// the returned error wraps both failures so the surface keeps full context.
//
// Ordering note: reset happens FIRST, then the quarantine row is inserted on
// top of the now-pre-migration state and committed. Inserting before reset
// would write the row to a commit that the reset then discards. Reading the
// failed version is independent of database state — it comes from the goose
// error — so no read is lost by reverting first.
func (s *Store) revertWithQuarantine(ctx context.Context, safety Checkpoint, phase string, version int64, cause error) *MigrationError {
	me := &MigrationError{Phase: phase, Version: version, Cause: cause}
	emitMigrationEvent("migrate.failed", map[string]string{
		"phase":   phase,
		"version": fmt.Sprintf("%d", version),
		"error":   cause.Error(),
	})
	if err := s.ResetToCheckpoint(ctx, safety.Name); err != nil {
		emitMigrationEvent("safety_branch.revert_failed", map[string]string{
			"branch": safety.Name,
			"error":  err.Error(),
		})
		me.Cause = fmt.Errorf("%w; revert to safety branch %s also failed: %v", cause, safety.Name, err)
		return me
	}
	emitMigrationEvent("safety_branch.reverted", map[string]string{
		"branch":  safety.Name,
		"phase":   phase,
		"version": fmt.Sprintf("%d", version),
	})
	if version <= 0 {
		return me
	}
	if qerr := s.recordQuarantine(ctx, version, fmt.Sprintf("auto-reverted by migration runner: %v", cause)); qerr != nil {
		emitMigrationEvent("quarantine.write_failed", map[string]string{
			"version": fmt.Sprintf("%d", version),
			"error":   qerr.Error(),
		})
		return me
	}
	if cerr := s.commitWorkingSet(ctx, fmt.Sprintf("Quarantine migration version %d", version)); cerr != nil {
		emitMigrationEvent("quarantine.commit_failed", map[string]string{
			"version": fmt.Sprintf("%d", version),
			"error":   cerr.Error(),
		})
	}
	return me
}

// versionFromGooseError extracts the failing version from goose's
// PartialError when present. Returns 0 if the error is not a PartialError —
// callers treat 0 as "no specific version to quarantine."
func versionFromGooseError(err error) int64 {
	var partial *goose.PartialError
	if !errors.As(err, &partial) {
		return 0
	}
	if partial.Failed == nil || partial.Failed.Source == nil {
		return 0
	}
	return partial.Failed.Source.Version
}

// collectSettledVersions returns the set of migration versions that ended up
// applied in this Open: every version goose just ran via Up plus, when an
// adoption stamped them, baselineVersion. Used by advanceCompatFloor to
// determine whether the workspace's code_compat_floor needs to advance.
func collectSettledVersions(adopted bool, results []*goose.MigrationResult) []int64 {
	versions := make([]int64, 0, len(results)+1)
	if adopted {
		versions = append(versions, baselineVersion)
	}
	for _, r := range results {
		if r.Source != nil {
			versions = append(versions, r.Source.Version)
		}
	}
	return versions
}

// adoptPreGooseWorkspace detects workspaces that predate goose (application
// tables present, goose_db_version absent) and stamps them at baselineVersion
// after running the legacy probe-gated reconciliation that brings their schema
// to the converged shape baseline.sql encodes. No-op for fresh workspaces (no
// app tables) and for already-adopted workspaces (goose_db_version present).
func (s *Store) adoptPreGooseWorkspace(ctx context.Context) (bool, error) {
	gooseExists, err := tableExists(ctx, s.db, gooseVersionTable)
	if err != nil {
		return false, err
	}
	if gooseExists {
		return false, nil
	}
	appExists, err := tableExists(ctx, s.db, "issues")
	if err != nil {
		return false, err
	}
	if !appExists {
		return false, nil
	}
	if err := s.reconcileLegacySchema(ctx); err != nil {
		return false, err
	}
	if err := s.stampGooseBaseline(ctx); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM meta WHERE meta_key = ?", "schema_version"); err != nil {
		return false, fmt.Errorf("delete legacy meta.schema_version: %w", err)
	}
	return true, nil
}

// stampGooseBaseline creates goose_db_version and seeds it so goose treats
// baselineVersion as already applied. Mirrors goose's own initialization
// (the DDL and seed-row goose runs internally on first contact) so a pre-goose
// workspace ends up indistinguishable from one that ran baseline.sql via
// goose itself.
func (s *Store) stampGooseBaseline(ctx context.Context) error {
	createStmt := fmt.Sprintf(`CREATE TABLE %s (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		version_id BIGINT NOT NULL,
		is_applied BOOLEAN NOT NULL,
		tstamp TIMESTAMP NULL DEFAULT NOW(),
		PRIMARY KEY(id)
	)`, gooseVersionTable)
	if _, err := s.db.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("create %s: %w", gooseVersionTable, err)
	}
	insertStmt := fmt.Sprintf("INSERT INTO %s (version_id, is_applied) VALUES (?, ?)", gooseVersionTable)
	// Goose's own initialization inserts version 0 to mark "table created"; it
	// is not a real migration. We mirror that, then stamp the baseline.
	if _, err := s.db.ExecContext(ctx, insertStmt, 0, true); err != nil {
		return fmt.Errorf("seed %s with version 0: %w", gooseVersionTable, err)
	}
	if _, err := s.db.ExecContext(ctx, insertStmt, baselineVersion, true); err != nil {
		return fmt.Errorf("stamp baseline in %s: %w", gooseVersionTable, err)
	}
	return nil
}

// tableExists reports whether the named table is present in the current
// database. Used by adoption to discriminate fresh / pre-goose / on-goose
// workspaces. Restricted to the active database via DATABASE() so a stray
// table in another schema does not skew detection.
func tableExists(ctx context.Context, db *sql.DB, tableName string) (bool, error) {
	const probe = `SELECT 1 FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1`
	var present int
	err := db.QueryRowContext(ctx, probe, tableName).Scan(&present)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("probe table %s: %w", tableName, err)
	}
	return true, nil
}
