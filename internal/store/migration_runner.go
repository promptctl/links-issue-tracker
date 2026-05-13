package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// ErrDryRun is the sentinel returned by Open when LIT_MIGRATE_DRY_RUN=1 and
// all pending migrations validated cleanly. It is not an error in the usual
// sense — the binary should exit 0 when it sees this. Open refused to return
// a functional store so no workspace state is left in a partially-migrated
// condition.
var ErrDryRun = errors.New("dry-run validation complete")

// migrationEventWriter is where the runner emits structured event lines.
// Every line is one JSON object carrying an RFC3339 "ts" field, an "event"
// name, and any arbitrary fields the call site passes through
// emitMigrationEvent. Defaults to stderr; tests reroute through a bytes
// buffer via this hook to capture and assert on the rendered output.
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

// emitMigrationEvent writes one single-line JSON object to migrationEventWriter.
// Every field in `fields` is merged into the top-level object alongside the
// mandatory "ts" (RFC3339) and "event" keys. Numeric and boolean values should
// be passed as their native Go types (int64, bool) so the JSON representation
// is correct. [LAW:single-enforcer] One emission helper — every event in the
// runner routes through here.
//
// `fields` is `map[string]any`, which admits non-JSON-marshalable values
// (channels, functions, cyclic structures). On marshal failure, emit a
// minimal fallback JSON line carrying `ts`, the original `event` name, and
// an `_emit_error` field describing the failure — never produce a non-JSON
// line that would break downstream log parsing. [LAW:types-are-the-program]
// every line is one valid JSON object regardless of what callers pass.
func emitMigrationEvent(name string, fields map[string]any) {
	m := make(map[string]any, len(fields)+2)
	// Merge caller fields first, then write the mandatory ts/event keys so
	// they can never be overwritten by a stray caller. The runner itself
	// never passes ts/event in fields; this guard exists to keep the event
	// contract honored even if a future caller drifts.
	for k, v := range fields {
		if k == "ts" || k == "event" {
			continue
		}
		m[k] = v
	}
	m["ts"] = time.Now().UTC().Format(time.RFC3339)
	m["event"] = name
	b, err := json.Marshal(m)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{
			"ts":           m["ts"],
			"event":        name,
			"_emit_error":  err.Error(),
		})
		fmt.Fprintln(migrationEventWriter, string(fallback))
		return
	}
	fmt.Fprintln(migrationEventWriter, string(b))
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
	// [LAW:dataflow-not-control-flow] dryRun is a value that selects the
	// commit vs rollback path at the end; migration bodies run the same
	// code path regardless.
	dryRun := os.Getenv("LIT_MIGRATE_DRY_RUN") == "1"

	quarantined, err := readQuarantinedVersions(ctx, s.db)
	if err != nil {
		return false, &MigrationError{Phase: "quarantine_read", Cause: err}
	}
	for _, v := range quarantined {
		emitMigrationEvent("migrate.skipped_quarantined", map[string]any{
			"version": v,
		})
	}

	opts := []goose.ProviderOption{}
	if len(quarantined) > 0 {
		opts = append(opts, goose.WithExcludeVersions(quarantined))
	}
	if extraMigrationProviderOptions != nil {
		opts = append(opts, extraMigrationProviderOptions()...)
	}

	adoptionNeeded, err := needsAdoption(ctx, s.db)
	if err != nil {
		return false, &MigrationError{Phase: "adoption", Cause: fmt.Errorf("probe adoption: %w", err)}
	}

	// Skip the safety branch entirely when there's no work to do. Without
	// this short-circuit every Open would accumulate a `pre-migrate-<ns>`
	// branch at the current HEAD, which is wasted retention budget and —
	// more importantly — would let `lit doctor --reset-to-pre-migration`
	// pick a checkpoint that is "the state right before this no-op Open",
	// i.e. the same as current state. The reset is meaningful only when
	// the runner is about to mutate, so the checkpoint is created only
	// then. [LAW:dataflow-not-control-flow] every Open still runs the
	// same probe sequence; what varies is the data (whether adoption or
	// pending migrations exist) and therefore whether the work block runs.
	//
	// When adoption is needed we know there's work (adoption itself
	// qualifies) without inspecting pending — and crucially, we must NOT
	// build the goose Provider here because Provider construction creates
	// the `goose_db_version` table, which would flip the adoption probe to
	// "already on goose" and skip the reconciliation path that pre-goose
	// workspaces require.
	var provider *goose.Provider
	if !adoptionNeeded {
		provider, err = goose.NewProvider(gooseDialect, s.db, migrations.FS, opts...)
		if err != nil {
			return false, &MigrationError{Phase: "provider", Cause: fmt.Errorf("build goose provider: %w", err)}
		}
		pending, err := pendingMigrations(ctx, provider)
		if err != nil {
			return false, &MigrationError{Phase: "status", Cause: fmt.Errorf("get pending migrations: %w", err)}
		}
		if len(pending) == 0 {
			floorChanged, err := s.advanceCompatFloor(ctx, collectSettledVersions(false, nil))
			if err != nil {
				return false, &MigrationError{Phase: "advance_floor", Cause: fmt.Errorf("advance code_compat_floor: %w", err)}
			}
			if dryRun {
				emitMigrationEvent("dry_run.summary", map[string]any{
					"pending":   0,
					"validated": 0,
				})
				return false, ErrDryRun
			}
			return floorChanged, nil
		}
	}

	safety, err := s.CreateCheckpoint(ctx, preMigrateCheckpointPrefix, preMigrateCheckpointRetain)
	if err != nil {
		return false, &MigrationError{Phase: "checkpoint", Cause: fmt.Errorf("create pre-migrate safety branch: %w", err)}
	}
	emitMigrationEvent("safety_branch.created", map[string]any{
		"name":   safety.Name,
		"commit": safety.CommitSHA,
	})

	adopted, err := s.adoptPreGooseWorkspace(ctx)
	if err != nil {
		if dryRun {
			return false, s.revertDryRun(ctx, safety, "adoption", 0, fmt.Errorf("adopt pre-goose workspace: %w", err))
		}
		return false, s.revertWithQuarantine(ctx, safety, "adoption", 0, fmt.Errorf("adopt pre-goose workspace: %w", err))
	}
	if adopted {
		emitMigrationEvent("adopt.pre_goose", map[string]any{
			"stamped_to": int64(baselineVersion),
		})
		// Isolate adoption from the per-migration commits that follow.
		// Without this, the first `migrate: v<N>` commit would also carry
		// the goose_db_version table creation, baseline-stamp rows, and
		// the legacy meta.schema_version delete — making per-migration
		// commits non-isolated for forensic log inspection.
		// [LAW:single-enforcer] each migration commit must contain only
		// its own changes. Dry-run skips this because the safety branch
		// reset undoes everything regardless.
		if !dryRun {
			if cerr := s.commitWorkingSet(ctx, fmt.Sprintf("Adopt pre-goose workspace to baseline (v%d)", baselineVersion)); cerr != nil {
				return false, s.revertWithQuarantine(ctx, safety, "adoption_commit", 0, fmt.Errorf("commit adoption: %w", cerr))
			}
			emitMigrationEvent("adopt.commit", map[string]any{
				"stamped_to": int64(baselineVersion),
			})
		}
	}

	// Build (or rebuild) the provider after adoption: goose's Provider
	// snapshots state at construction time, so the post-stamp pending
	// list must come from a Provider built AFTER stampGooseBaseline.
	// When adoption was needed we deferred provider construction until
	// here; when no adoption fired, we rebuild to pick up any state goose
	// itself produced during its initial Status query above.
	if adopted || provider == nil {
		provider, err = goose.NewProvider(gooseDialect, s.db, migrations.FS, opts...)
		if err != nil {
			if dryRun {
				return false, s.revertDryRun(ctx, safety, "provider", 0, fmt.Errorf("build goose provider: %w", err))
			}
			return false, s.revertWithQuarantine(ctx, safety, "provider", 0, fmt.Errorf("build goose provider: %w", err))
		}
	}
	pending, err := pendingMigrations(ctx, provider)
	if err != nil {
		if dryRun {
			return false, s.revertDryRun(ctx, safety, "status", 0, fmt.Errorf("get pending migrations: %w", err))
		}
		return false, s.revertWithQuarantine(ctx, safety, "status", 0, fmt.Errorf("get pending migrations: %w", err))
	}

	var results []*goose.MigrationResult
	for _, m := range pending {
		emitMigrationEvent("migrate.start", map[string]any{
			"version": m.version,
			"name":    m.name,
		})
		result, err := provider.ApplyVersion(ctx, m.version, true)
		if err != nil {
			if dryRun {
				return false, s.revertDryRun(ctx, safety, "up", m.version, fmt.Errorf("apply migration v%d: %w", m.version, err))
			}
			return false, s.revertWithQuarantine(ctx, safety, "up", m.version, fmt.Errorf("apply migration v%d: %w", m.version, err))
		}
		if !dryRun {
			// Write success row before commitMigration so it lands in the
			// same Dolt commit as the migration's schema changes.
			s.writeMigrationLogSuccess(ctx, m.version, m.name, result.Duration.Milliseconds())
			if err := s.commitMigration(ctx, result); err != nil {
				// Phase is "commit", NOT "up", and version=0 so the migration
				// is NOT quarantined: the body applied cleanly and the
				// migration is logically correct. We still revert to the
				// safety branch so the workspace stays consistent, but on
				// the next Open the migration will be re-applied (retried)
				// rather than excluded. [LAW:single-enforcer] quarantine is
				// for migrations whose *body* is bad, not commits whose
				// *commit step* failed transiently.
				return false, s.revertWithQuarantine(ctx, safety, "commit", 0, fmt.Errorf("commit migration v%d: %w", m.version, err))
			}
		}
		results = append(results, result)
	}

	// Both paths ran all migration bodies. Now commit or rollback by mode.
	if dryRun {
		if err := s.ResetToCheckpoint(ctx, safety.Name); err != nil {
			return false, &MigrationError{Phase: "dry_run_reset", Cause: fmt.Errorf("dry-run reset to safety branch: %w", err)}
		}
		// Dry-run validations don't represent real migration work, so the
		// safety branch they created should not consume retention budget
		// or surface as a recovery target via `lit doctor --reset-to-pre-
		// migration`. Best-effort delete: a leak here is a noise problem,
		// not a correctness problem (the next Open's CreateCheckpoint
		// prune would eventually evict it), so we log the failure as an
		// event but don't fail the dry-run.
		s.deleteCheckpointBestEffort(ctx, safety.Name, "dry_run_cleanup")
		emitMigrationEvent("dry_run.summary", map[string]any{
			"pending":   len(results),
			"validated": len(results),
		})
		return false, ErrDryRun
	}

	settled := collectSettledVersions(adopted, results)
	floorChanged, err := s.advanceCompatFloor(ctx, settled)
	if err != nil {
		return false, s.revertWithQuarantine(ctx, safety, "advance_floor", 0, fmt.Errorf("advance code_compat_floor: %w", err))
	}
	return adopted || len(results) > 0 || floorChanged, nil
}

// pendingMigration pairs a migration version with its human-readable name so
// the caller can emit migrate.start events without re-querying provider.Status.
type pendingMigration struct {
	version int64
	name    string
}

// pendingMigrations returns the ordered list of migrations that are registered
// but not yet applied, paired with their human-readable names.
func pendingMigrations(ctx context.Context, provider *goose.Provider) ([]pendingMigration, error) {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("query migration status: %w", err)
	}
	var pending []pendingMigration
	for _, s := range statuses {
		if s.State == goose.StatePending && s.Source != nil {
			pending = append(pending, pendingMigration{
				version: s.Source.Version,
				name:    migrationSourceName(s.Source),
			})
		}
	}
	return pending, nil
}

// migrationCommitAuthor is the stable Dolt author identity used for per-
// migration commits so they are visually distinct from user-driven mutations
// in `dolt log`.
const migrationCommitAuthor = "lit-migrate <bot@local>"

// commitMigration writes a structured Dolt commit for a single applied
// migration. The commit message body carries machine-parseable key=value
// fields for forensic log inspection. Author is always migrationCommitAuthor.
//
// The actual DOLT_COMMIT runs inside retryTransientManifestReadOnly so a
// transient manifest-read-only error during a per-migration commit gets
// the same retry treatment as user-driven commits do via commitWorkingSet.
// [LAW:single-enforcer] retryTransientManifestReadOnly is the single
// transient-commit retry policy; bypassing it here would make migration
// commits more brittle than ordinary mutations.
func (s *Store) commitMigration(ctx context.Context, result *goose.MigrationResult) error {
	if result == nil || result.Source == nil {
		return nil
	}
	name := migrationSourceName(result.Source)
	msg := fmt.Sprintf("migrate: v%d %s\n\nduration_ms=%d\nsource=%s",
		result.Source.Version, name,
		result.Duration.Milliseconds(),
		result.Source.Path)
	var commitHash string
	var noop bool
	err := retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
		if cerr := s.db.QueryRowContext(ctx, `CALL DOLT_COMMIT('-Am', ?, '--author', ?)`, msg, migrationCommitAuthor).Scan(&commitHash); cerr != nil {
			if strings.Contains(strings.ToLower(cerr.Error()), "nothing to commit") {
				noop = true
				return nil
			}
			return cerr
		}
		return nil
	}, transientManifestRetryDelay, waitWithContext)
	if err != nil {
		return fmt.Errorf("commit migration v%d: %w", result.Source.Version, err)
	}
	if noop {
		emitMigrationEvent("migrate.commit", map[string]any{
			"version":     result.Source.Version,
			"duration_ms": result.Duration.Milliseconds(),
			"noop":        true,
		})
		return nil
	}
	emitMigrationEvent("migrate.commit", map[string]any{
		"version":     result.Source.Version,
		"duration_ms": result.Duration.Milliseconds(),
		"commit":      commitHash,
	})
	return nil
}

// writeMigrationLogSuccess writes a success row to migration_log. It is
// called BEFORE commitMigration so the row is included in the same Dolt
// commit as the migration's schema changes. On a fresh workspace
// migrations 1 and 2 run before migration 3 creates the table — we probe
// for its existence first and silently skip when absent, so the happy
// path stays clean. Real write failures (table exists but the INSERT
// errors) still surface as `migration_log.write_failed` events.
func (s *Store) writeMigrationLogSuccess(ctx context.Context, version int64, name string, durationMs int64) {
	exists, err := tableExists(ctx, s.db, "migration_log")
	if err != nil || !exists {
		return
	}
	finishedAt := time.Now().UTC()
	startedAt := finishedAt.Add(-time.Duration(durationMs) * time.Millisecond)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO migration_log
			(version, name, started_at, finished_at, duration_ms, status, error_text, rows_affected)
		 VALUES (?, ?, ?, ?, ?, 'success', NULL, 0)`,
		version, name, startedAt, finishedAt, durationMs); err != nil {
		emitMigrationEvent("migration_log.write_failed", map[string]any{
			"version": version,
			"error":   err.Error(),
		})
	}
}

// writeMigrationLogFailure writes a failure row to migration_log. It is
// called AFTER ResetToCheckpoint in revertWithQuarantine so the row survives
// the reset and is committed alongside the quarantine row. Same probe-then-
// write shape as writeMigrationLogSuccess so happy paths stay clean.
func (s *Store) writeMigrationLogFailure(ctx context.Context, version int64, errText string) {
	exists, err := tableExists(ctx, s.db, "migration_log")
	if err != nil || !exists {
		return
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO migration_log
			(version, name, started_at, finished_at, duration_ms, status, error_text, rows_affected)
		 VALUES (?, '', ?, ?, 0, 'failure', ?, 0)`,
		version, now, now, errText); err != nil {
		emitMigrationEvent("migration_log.write_failed", map[string]any{
			"version": version,
			"error":   err.Error(),
		})
	}
}

// migrationSourceName extracts a human-readable migration name from a goose
// Source. SQL files follow the "NNNNN_name.sql" convention; Go migrations
// registered via WithGoMigrations have an empty path and fall back to a
// version-based placeholder.
func migrationSourceName(source *goose.Source) string {
	if source.Path == "" {
		return fmt.Sprintf("v%d", source.Version)
	}
	base := strings.TrimSuffix(filepath.Base(source.Path), filepath.Ext(source.Path))
	if idx := strings.Index(base, "_"); idx != -1 {
		return base[idx+1:]
	}
	return base
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
	emitMigrationEvent("migrate.error", map[string]any{
		"phase":   phase,
		"version": version,
		"error":   cause.Error(),
	})
	if err := s.ResetToCheckpoint(ctx, safety.Name); err != nil {
		emitMigrationEvent("safety_branch.revert_failed", map[string]any{
			"name":  safety.Name,
			"error": err.Error(),
		})
		me.Cause = fmt.Errorf("%w; revert to safety branch %s also failed: %v", cause, safety.Name, err)
		return me
	}
	emitMigrationEvent("safety_branch.reverted", map[string]any{
		"name":    safety.Name,
		"phase":   phase,
		"version": version,
	})
	if version <= 0 {
		return me
	}
	if qerr := s.recordQuarantine(ctx, version, fmt.Sprintf("auto-reverted by migration runner: %v", cause)); qerr != nil {
		emitMigrationEvent("quarantine.write_failed", map[string]any{
			"version": version,
			"error":   qerr.Error(),
		})
		// Sister case to the quarantine-commit failure path below: a
		// failed write leaves the workspace reverted with no durable
		// quarantine record, so the same bad migration would be retried
		// on the next Open. Wrap into me.Cause so operators see the full
		// failure story — symmetric with the commit-failure handling.
		// [LAW:single-enforcer] operator-facing error surface owns both
		// write- and commit-stage failures.
		me.Cause = fmt.Errorf("%w; quarantine write for v%d also failed: %v", cause, version, qerr)
		return me
	}
	// Write failure row after reset so it survives alongside the quarantine
	// commit. Best-effort: if migration_log doesn't exist yet, silently drops.
	s.writeMigrationLogFailure(ctx, version, cause.Error())
	if cerr := s.commitWorkingSet(ctx, fmt.Sprintf("Quarantine migration version %d", version)); cerr != nil {
		emitMigrationEvent("quarantine.commit_failed", map[string]any{
			"version": version,
			"error":   cerr.Error(),
		})
		// Surface the commit failure to the caller so operators see that
		// the quarantine record did not persist. Without this, the same
		// bad migration would be retried on the next Open with no log of
		// why the previous run failed. Mirrors the reset-failed pattern
		// above. [LAW:single-enforcer] operator-facing error surface owns
		// the full failure story.
		me.Cause = fmt.Errorf("%w; quarantine commit for v%d also failed: %v", cause, version, cerr)
	}
	return me
}

// revertDryRun resets to the safety branch without quarantining the failed
// version. Used by dry-run mode: the migration ran as a validation exercise
// so no permanent quarantine record should be written.
func (s *Store) revertDryRun(ctx context.Context, safety Checkpoint, phase string, version int64, cause error) *MigrationError {
	me := &MigrationError{Phase: phase, Version: version, Cause: cause}
	emitMigrationEvent("migrate.error", map[string]any{
		"phase":   phase,
		"version": version,
		"error":   cause.Error(),
	})
	if err := s.ResetToCheckpoint(ctx, safety.Name); err != nil {
		emitMigrationEvent("safety_branch.revert_failed", map[string]any{
			"name":  safety.Name,
			"error": err.Error(),
		})
		me.Cause = fmt.Errorf("%w; dry-run revert to safety branch %s also failed: %v", cause, safety.Name, err)
		return me
	}
	emitMigrationEvent("safety_branch.reverted", map[string]any{
		"name":    safety.Name,
		"phase":   phase,
		"version": version,
	})
	// Failed dry-runs leave nothing of value on the safety branch — the
	// validation aborted before any real work landed. Delete the branch
	// best-effort so it doesn't consume retention budget or pollute the
	// recovery surface.
	s.deleteCheckpointBestEffort(ctx, safety.Name, "dry_run_cleanup")
	return me
}

// deleteCheckpointBestEffort removes a checkpoint branch via DOLT_BRANCH -D
// and emits a tagged event if the deletion fails. Used by dry-run paths
// where the branch is known not to represent a recovery target; a leak is
// noise (next CreateCheckpoint's prune would eventually evict it) rather
// than a correctness issue, so failure is logged but not propagated.
func (s *Store) deleteCheckpointBestEffort(ctx context.Context, name, reason string) {
	if _, err := s.db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", name); err != nil {
		emitMigrationEvent("safety_branch.cleanup_failed", map[string]any{
			"name":   name,
			"reason": reason,
			"error":  err.Error(),
		})
	}
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

// needsAdoption returns true iff the workspace requires the legacy
// pre-goose reconciliation path: application tables are present but
// goose_db_version is not. Read-only probe; runMigrations uses it to
// decide whether to create a safety branch before any state mutates.
func needsAdoption(ctx context.Context, db *sql.DB) (bool, error) {
	gooseExists, err := tableExists(ctx, db, gooseVersionTable)
	if err != nil {
		return false, err
	}
	if gooseExists {
		return false, nil
	}
	appExists, err := tableExists(ctx, db, "issues")
	if err != nil {
		return false, err
	}
	return appExists, nil
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
