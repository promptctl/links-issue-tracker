package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// quarantineTableName names the goose-owned table that records "do not
// apply" versions. The table is created by 00002_migration_quarantine.sql,
// so workspaces still on baseline (or earlier, during adoption) see no
// table; the readers below treat absence as "no quarantines."
const quarantineTableName = "migration_quarantine"

// readQuarantinedVersions returns the versions the runner must exclude on
// this Open. Returns an empty slice when the table is absent (workspace
// predates 00002) or empty. [LAW:dataflow-not-control-flow] absent and empty
// produce the same downstream behavior — only the data differs.
func readQuarantinedVersions(ctx context.Context, db *sql.DB) ([]int64, error) {
	exists, err := tableExists(ctx, db, quarantineTableName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		fmt.Sprintf("SELECT version_id FROM %s ORDER BY version_id", quarantineTableName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", quarantineTableName, err)
	}
	defer rows.Close()
	versions := make([]int64, 0)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", quarantineTableName, err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s rows: %w", quarantineTableName, err)
	}
	return versions, nil
}

// recordQuarantine inserts (or updates) the quarantine row for `version`.
// Caller is responsible for committing the working set — the helper writes
// the row only, so it can be batched with other writes in the recovery
// flow. Returns an error if the table is absent — the caller (auto-revert
// or recovery) handles that case by surfacing it rather than silently
// dropping the quarantine.
//
// [LAW:single-enforcer] All writes to migration_quarantine route through
// this function; the schema (version_id PK, reason TEXT, quarantined_at
// DATETIME) is encoded once.
func (s *Store) recordQuarantine(ctx context.Context, version int64, reason string) error {
	if version <= 0 {
		return fmt.Errorf("quarantine version must be positive, got %d", version)
	}
	exists, err := tableExists(ctx, s.db, quarantineTableName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s table absent; cannot quarantine version %d", quarantineTableName, version)
	}
	_, err = s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (version_id, reason, quarantined_at)
			VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE reason = VALUES(reason), quarantined_at = VALUES(quarantined_at)`,
			quarantineTableName),
		version, reason, time.Now().UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return fmt.Errorf("insert quarantine for version %d: %w", version, err)
	}
	return nil
}

// ResetToPreMigrationResult describes the outcome of a manual recovery
// triggered by `lit doctor --reset-to-pre-migration`. Surfaced through the
// CLI so the agent can see what happened and what was quarantined.
type ResetToPreMigrationResult struct {
	Checkpoint          string  `json:"checkpoint"`
	CheckpointTimestamp string  `json:"checkpoint_timestamp"`
	QuarantinedVersions []int64 `json:"quarantined_versions"`
}

// ErrNoPreMigrationCheckpoint is the typed sentinel for "there is no
// safety branch to reset to." The CLI surfaces this as an actionable
// message rather than a generic failure.
var ErrNoPreMigrationCheckpoint = errors.New("no pre-migrate safety branch found")

// ResetToPreMigration is the manual recovery surface for agents whose
// schema is broken in a way the runner did not catch (e.g., smoke test
// failed after a successful migration). Reads the most recent pre-migrate
// safety branch, reverts to it, and quarantines every version applied
// since so subsequent Opens skip them.
//
// Order matters here:
//   - Read goose_db_version BEFORE reset (the rows for the offending
//     versions are about to vanish).
//   - Reset master to the safety branch.
//   - Insert quarantine rows on the now-pre-migration master.
//   - Commit.
//
// Inserting before reset would write the quarantine row to a commit that
// the reset then discards. The current ordering keeps the quarantine on
// the live master after reset.
//
// [LAW:single-enforcer] This function is the only writer of the
// "manual reset + quarantine" flow; the auto-revert path owns the
// "runner-detected failure" flow. They share primitives (ResetToCheckpoint,
// recordQuarantine) but not control.
func (s *Store) ResetToPreMigration(ctx context.Context) (ResetToPreMigrationResult, error) {
	var result ResetToPreMigrationResult
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		inner, runErr := s.resetToPreMigrationLocked(ctx)
		result = inner
		return runErr
	})
	return result, err
}

// resetToPreMigrationLocked performs the recovery without acquiring the
// commit lock so callers that are already inside a locked region (e.g., the
// CLI's withMutation-style wrappers) compose cleanly. Public callers use
// ResetToPreMigration which delegates here under the lock.
func (s *Store) resetToPreMigrationLocked(ctx context.Context) (ResetToPreMigrationResult, error) {
	checkpoints, err := s.ListCheckpoints(ctx, preMigrateCheckpointPrefix)
	if err != nil {
		return ResetToPreMigrationResult{}, err
	}
	if len(checkpoints) == 0 {
		return ResetToPreMigrationResult{}, ErrNoPreMigrationCheckpoint
	}
	cp := checkpoints[0]

	versions, err := versionsAppliedSinceCheckpoint(ctx, s.db, cp.Name)
	if err != nil {
		return ResetToPreMigrationResult{}, fmt.Errorf("read versions applied since %s: %w", cp.Name, err)
	}

	if err := s.ResetToCheckpoint(ctx, cp.Name); err != nil {
		return ResetToPreMigrationResult{}, err
	}

	reason := fmt.Sprintf("reset by lit doctor at %s", time.Now().UTC().Format(time.RFC3339))
	for _, v := range versions {
		if err := s.recordQuarantine(ctx, v, reason); err != nil {
			return ResetToPreMigrationResult{}, err
		}
	}
	if len(versions) > 0 {
		if err := s.commitWorkingSet(ctx, fmt.Sprintf("Quarantine %d version(s) reset by lit doctor", len(versions))); err != nil {
			return ResetToPreMigrationResult{}, err
		}
	}

	return ResetToPreMigrationResult{
		Checkpoint:          cp.Name,
		CheckpointTimestamp: cp.CreatedAt.Format(time.RFC3339),
		QuarantinedVersions: versions,
	}, nil
}

// versionsAppliedSinceCheckpoint returns goose versions present at HEAD but
// absent at the safety branch — exactly the migrations that landed since the
// branch was forked, excluding goose's seed row 0.
//
// We diff against the branch's view of goose_db_version (via Dolt's AS OF
// branch syntax) instead of comparing tstamps because the embedded Dolt
// server's clock and Go's time.Now() do not share a timezone, and the
// goose_db_version.tstamp column is second-precision — both make tstamp
// comparison unreliable. Set difference against the branch snapshot is the
// authoritative answer: it asks the same data question Dolt was forked for
// in the first place.
//
// safetyBranch is interpolated literally because Dolt's AS OF target is not
// a parameterizable position in the SQL grammar; the regex in
// checkpointPrefixPattern + the timestamp suffix the runner appends keep
// safetyBranch in a safe character class (lowercase, digits, hyphen) so
// interpolation cannot smuggle SQL.
func versionsAppliedSinceCheckpoint(ctx context.Context, db *sql.DB, safetyBranch string) ([]int64, error) {
	if !isSafeCheckpointBranchName(safetyBranch) {
		return nil, fmt.Errorf("checkpoint branch name %q failed safety check; refusing to interpolate", safetyBranch)
	}
	exists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	headVersions, err := readAppliedGooseVersions(ctx, db,
		fmt.Sprintf("SELECT version_id FROM %s WHERE is_applied = TRUE AND version_id > 0",
			gooseVersionTable))
	if err != nil {
		return nil, fmt.Errorf("read HEAD goose versions: %w", err)
	}
	branchVersions, err := readAppliedGooseVersions(ctx, db,
		fmt.Sprintf("SELECT version_id FROM %s AS OF '%s' WHERE is_applied = TRUE AND version_id > 0",
			gooseVersionTable, safetyBranch))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "table not found") {
			// Safety branch predates the goose registry (it was taken on
			// a workspace's very first Open, before baseline applied).
			// Treat the branch as having no applied versions — every
			// current applied version is "new" relative to it.
			branchVersions = nil
		} else {
			return nil, fmt.Errorf("read safety-branch goose versions: %w", err)
		}
	}
	branchSet := make(map[int64]struct{}, len(branchVersions))
	for _, v := range branchVersions {
		branchSet[v] = struct{}{}
	}
	diff := make([]int64, 0)
	for _, v := range headVersions {
		if _, present := branchSet[v]; !present {
			diff = append(diff, v)
		}
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i] < diff[j] })
	return diff, nil
}

// readAppliedGooseVersions runs the given query (which must select
// version_id alone) and collects the result. Caller composes the WHERE /
// AS OF clause; this helper owns the row-iteration boilerplate.
func readAppliedGooseVersions(ctx context.Context, db *sql.DB, query string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// isSafeCheckpointBranchName mirrors the character class formatCheckpointName
// produces (`<prefix>-<unix-nanos>` where prefix matches checkpointPrefixPattern).
// Used as a defensive guard before interpolating a branch name into SQL.
func isSafeCheckpointBranchName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return true
}

// formatRecoveryHint composes the agent-facing message that accompanies a
// failed smoke test (or any other "schema is broken at HEAD" surface). The
// hint names the next command to run rather than describing the failure in
// the abstract — agents act on commands, not advice.
func formatRecoveryHint(failedProbe string, lastVersion int64, lastTimestamp string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "smoke test %q failed", failedProbe)
	if lastVersion > 0 {
		fmt.Fprintf(&b, "; the most recent migration was version %d", lastVersion)
		if lastTimestamp != "" {
			fmt.Fprintf(&b, " at %s", lastTimestamp)
		}
	}
	b.WriteString("; run `lit doctor --reset-to-pre-migration` to recover")
	return b.String()
}
