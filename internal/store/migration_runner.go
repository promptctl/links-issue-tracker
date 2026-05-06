package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// runMigrations brings the workspace's schema to the latest registered goose
// version. Returns true if any state changed (so the caller can decide whether
// to commit the working set).
//
// Three workspace shapes converge through this function:
//   - fresh (no application tables, no goose_db_version) → goose runs baseline.
//   - pre-goose (application tables exist, no goose_db_version) → adoption
//     reconciles the legacy schema then stamps baseline as applied.
//   - already-on-goose → goose runs any pending migrations beyond baseline.
func (s *Store) runMigrations(ctx context.Context) (bool, error) {
	adopted, err := s.adoptPreGooseWorkspace(ctx)
	if err != nil {
		return false, fmt.Errorf("adopt pre-goose workspace: %w", err)
	}
	provider, err := goose.NewProvider(gooseDialect, s.db, migrations.FS)
	if err != nil {
		return false, fmt.Errorf("build goose provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return false, fmt.Errorf("apply pending migrations: %w", err)
	}
	return adopted || len(results) > 0, nil
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
