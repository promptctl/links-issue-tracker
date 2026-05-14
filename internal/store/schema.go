package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/rank"
)

// priorityCheckClause is the schema-level encoding of the priority range
// invariant. Derived from the canonical model.Priority* constants and shared
// by the fresh-table CREATE (createIssuesTableStmt) and the upgrade-path
// ALTER (resetPrioritiesToNormal) so the two writers cannot drift.
// [LAW:one-source-of-truth]
var priorityCheckClause = fmt.Sprintf("priority >= %d AND priority <= %d", model.PriorityNormal, model.PriorityUrgent)

// canonicalStatusCheckClause encodes the invariant that container rows store
// NULL status (state is derived from children) and leaf rows carry one of the
// known states. Single source of truth used by both the fresh-table CREATE
// (via createIssuesTableStmt) and the upgrade-path ALTER (ensureStatusConstraint)
// so they cannot diverge. The leaf branch carries an explicit `status IS NOT
// NULL`: `IN (...)` against NULL evaluates to NULL, and MySQL/Dolt CHECK
// treats NULL as not-violated, so without this clause a leaf row with NULL
// status would slip through the very constraint it is supposed to forbid.
const canonicalStatusCheckClause = `(issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))`

func createIssuesTableStmt() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS issues (
			id VARCHAR(191) PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			agent_prompt TEXT,
			status VARCHAR(32) NULL,
			priority INT NOT NULL,
			issue_type VARCHAR(32) NOT NULL,
			topic VARCHAR(191) NOT NULL,
			assignee TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL,
			closed_at VARCHAR(64) NULL,
			archived_at VARCHAR(64) NULL,
			deleted_at VARCHAR(64) NULL,
			item_rank TEXT NOT NULL DEFAULT '',
			CHECK(%s),
			CHECK(%s),
			CHECK(issue_type IN ('task','feature','bug','chore','epic'))
		);`, canonicalStatusCheckClause, priorityCheckClause)
}

// migrate is the per-Open schema entry point. It refuses out-of-window
// workspaces, dispatches to the goose-backed runner (which handles fresh /
// pre-goose / already-on-goose shapes), then writes the always-current
// workspace_id meta fixture.
//
// Commit topology across one Open:
//   - inside runMigrations: one Dolt commit per applied migration; one
//     additional commit isolating adoption when a pre-goose workspace is
//     adopted; on failure, a quarantine-row commit after the safety-branch
//     reset (see migration_runner.go).
//   - here, in migrate: one trailing "Migrate links schema" commit captures
//     the workspace_id and code_compat_floor meta updates — but only when
//     either changed, so an idempotent re-open writes nothing.
//
// [LAW:single-enforcer] Every workspace shape funnels through runMigrations
// for schema convergence; the per-Open meta fixture is the only thing that
// runs unconditionally outside that path. The compat-window check is the
// only thing that runs *before* the runner and can refuse the entire Open.
// [LAW:dataflow-not-control-flow] The same operations execute every Open;
// what varies is what the runner has to do (apply baseline / adopt / no-op)
// and whether the compat check returns a typed error or nil.
func (s *Store) migrate(ctx context.Context) error {
	if err := checkCompatWindow(ctx, s.db, effectiveCodeVersion()); err != nil {
		return err
	}
	migrated, err := s.runMigrations(ctx)
	if err != nil {
		return err
	}
	workspaceChanged, err := s.ensureMetaValue(ctx, "workspace_id", s.workspaceID)
	if err != nil {
		return err
	}
	if migrated || workspaceChanged {
		if err := s.commitWorkingSet(ctx, "Migrate links schema"); err != nil {
			return err
		}
	}
	// Final gate: every Open must produce a workspace that satisfies the
	// canonical smoke probes. If the converge in runMigrations could not
	// heal a divergence (e.g., a missing column that's not part of the
	// idempotent CREATE TABLE set), fail Open loudly here rather than
	// hand back a broken handle that 1146s on the next mutation.
	// [LAW:verifiable-goals] The "Open succeeded" claim is checkable.
	if probe, err := s.runSmokeTests(ctx); err != nil {
		return fmt.Errorf("workspace smoke failed after migrate (probe %q): %w", probe, err)
	}
	return nil
}

// infraSchemaStatements are the runner-infrastructure tables —
// migration_quarantine and migration_log — that the runner itself needs to
// be present before any safety branch is created. They are CREATEd
// idempotently and outside the goose batch precisely so the safety-branch
// revert cannot erase them: if v3 fails and revert undoes v1 and v2, the
// quarantine table the runner is about to write to would be gone too.
// [LAW:single-enforcer] Single bootstrap step for runner infra; the rest
// of the canonical schema waits until after goose has done its work.
func infraSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS migration_quarantine (
			version_id BIGINT PRIMARY KEY,
			reason TEXT NOT NULL,
			quarantined_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS migration_log (
			id      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			version BIGINT          NOT NULL,
			name    TEXT            NOT NULL,
			started_at  DATETIME(3)  NOT NULL,
			finished_at DATETIME(3),
			duration_ms BIGINT,
			status      VARCHAR(10)  NOT NULL,
			error_text  TEXT,
			rows_affected BIGINT,
			PRIMARY KEY (id)
		);`,
	}
}

// bootstrapInfraSchema runs the infrastructure-tables reconcile. It is the
// FIRST thing runMigrations does — before reading the quarantine list,
// before the safety branch, before any goose work — so the runner can
// always count on those two tables existing regardless of what the
// workspace's goose stamp or disk truth says.
func (s *Store) bootstrapInfraSchema(ctx context.Context) (bool, error) {
	changed := false
	for _, stmt := range infraSchemaStatements() {
		c, err := execIgnoreAlreadyExists(ctx, s.db, stmt)
		if err != nil {
			return false, fmt.Errorf("bootstrap infra schema: %w", err)
		}
		changed = changed || c
	}
	return changed, nil
}

// canonicalSchemaStatements returns the ordered list of CREATE TABLE / CREATE
// INDEX statements that bring a workspace to the canonical application
// shape. Every statement is safe to re-execute — tables use CREATE TABLE IF
// NOT EXISTS, indexes are wrapped with the duplicate-key-name swallow in
// execIgnoreAlreadyExists.
//
// [LAW:one-source-of-truth] This list is the canonical application schema
// shared by ensureCanonicalSchema (post-Up unconditional reconcile) and
// the migration files (00001_baseline.sql for fresh workspaces). Adding
// a new table to the schema requires adding it here AND to smoke.go's
// smokeProbes — both have a coverage commitment.
func canonicalSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS meta (
			meta_key VARCHAR(191) PRIMARY KEY,
			meta_value TEXT NOT NULL
		);`,
		createIssuesTableStmt(),
		`CREATE TABLE IF NOT EXISTS relations (
			src_id VARCHAR(191) NOT NULL,
			dst_id VARCHAR(191) NOT NULL,
			type VARCHAR(32) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			PRIMARY KEY (src_id, dst_id, type),
			FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE,
			FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE,
			CHECK(type IN ('blocks','parent-child','related-to'))
		);`,
		`CREATE TABLE IF NOT EXISTS comments (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			body TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS labels (
			issue_id VARCHAR(191) NOT NULL,
			label VARCHAR(191) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			PRIMARY KEY (issue_id, label),
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		// issue_events + issue_event_changes are the canonical mutation log.
		`CREATE TABLE IF NOT EXISTS issue_events (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			action VARCHAR(64) NULL,
			reason TEXT NOT NULL,
			actor TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS issue_event_changes (
			event_id VARCHAR(191) NOT NULL,
			field VARCHAR(64) NOT NULL,
			from_value TEXT NULL,
			to_value TEXT NULL,
			PRIMARY KEY (event_id, field),
			FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE
		);`,
		// Indexes — execIgnoreAlreadyExists swallows the "duplicate key name"
		// error that fires when these already exist.
		//
		// idx_issues_rank is intentionally NOT here: it references the
		// item_rank column, which on legacy pre-goose workspaces is added
		// by reconcileLegacySchema's ALTER ADD COLUMN step AFTER this list
		// runs. Creating the index here would fail with "key column doesn't
		// exist" against those workspaces. reconcileLegacySchema and
		// baseline.sql remain the two places idx_issues_rank is created.
		`CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);`,
		`CREATE INDEX idx_relations_src_type ON relations(src_id, type);`,
		`CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);`,
		`CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);`,
		`CREATE INDEX idx_labels_issue ON labels(issue_id, label);`,
		`CREATE INDEX idx_labels_name ON labels(label, issue_id);`,
		`CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at);`,
	}
}

// ensureCanonicalSchema brings the workspace to the canonical schema shape
// regardless of what goose_db_version claims. Every statement is idempotent
// (CREATE TABLE IF NOT EXISTS, plus execIgnoreAlreadyExists swallowing
// duplicate-key-name on CREATE INDEX), so calling this on a healthy workspace
// is a no-op.
//
// It exists to break the false-equivalence between
// "goose stamped version N applied" and "the tables version N created exist
// on disk." Dolt's safety-branch revert can leave that pair inconsistent
// — orphan tables after a revert, missing tables after a partial undo —
// and the runner trusts goose by default. This function is the trust-but-
// verify step.
//
// Runs as the FIRST thing in runMigrations, before any safety branch is
// created, so that critical infra tables (migration_quarantine, migration_log)
// are present in the pre-migration state and survive a revert.
//
// [LAW:single-enforcer] Single function defining the canonical schema; both
// the migration files and reconcileLegacySchema delegate here.
// [LAW:types-are-the-program] Workspace shape divergence (goose claim vs
// disk truth) is converged into a single legal state by construction.
func (s *Store) ensureCanonicalSchema(ctx context.Context) (bool, error) {
	changed := false
	for _, stmt := range canonicalSchemaStatements() {
		c, err := execIgnoreAlreadyExists(ctx, s.db, stmt)
		if err != nil {
			return false, fmt.Errorf("ensure canonical schema: %w", err)
		}
		changed = changed || c
	}
	return changed, nil
}

// convergeLegacyAlterations runs the probe-gated ALTER / RENAME / data-backfill
// steps that bring legacy columns into the canonical shape. Every operation
// is idempotent (probe-and-execute or execIgnoreAlreadyExists), so running
// this on a fully-converged workspace is a no-op.
//
// Why this runs every Open (not just during adoption): a workspace adopted
// at an earlier date carried whatever the legacy reconciliation looked like
// THEN. New legacy renames added later (e.g., issue_events.assignee →
// actor when field-history landed) would never apply to already-adopted
// workspaces because the adoption path skipped on re-Open.
// [LAW:single-enforcer] One reconcile pass, one set of probe-gated ALTERs,
// runs on every Open. The "is this a legacy workspace" question is answered
// per-probe, not by a global adoption flag.
func (s *Store) convergeLegacyAlterations(ctx context.Context) (bool, error) {
	changed := false
	// Workspaces predating field-history's column rename still have
	// issue_events.assignee. The probe-gated rename keeps adoption
	// idempotent across pre-rename and post-rename shapes.
	renamed, err := s.execReconciliationUpdate(
		ctx,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issue_events' AND column_name = 'assignee' LIMIT 1`,
		`ALTER TABLE issue_events RENAME COLUMN assignee TO actor`,
		"rename issue_events.assignee to actor",
	)
	if err != nil {
		return false, err
	}
	changed = changed || renamed

	// item_rank was added to issues post-baseline. ADD COLUMN IF NOT EXISTS
	// isn't portable, so probe-and-execute via execIgnoreAlreadyExists.
	addedRank, err := execIgnoreAlreadyExists(ctx, s.db, `ALTER TABLE issues ADD COLUMN item_rank TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return false, err
	}
	changed = changed || addedRank

	addedRankIdx, err := execIgnoreAlreadyExists(ctx, s.db, `CREATE INDEX idx_issues_rank ON issues(item_rank(191))`)
	if err != nil {
		return false, err
	}
	changed = changed || addedRankIdx

	addedTopic, err := execIgnoreAlreadyExists(ctx, s.db, `ALTER TABLE issues ADD COLUMN topic VARCHAR(191) NOT NULL DEFAULT 'misc' AFTER issue_type`)
	if err != nil {
		return false, err
	}
	changed = changed || addedTopic

	// Workspaces predating the prompt rename still have the old `prompt`
	// column. `prompt` is reserved in Dolt's MySQL parser; backtick-quote
	// the source identifier.
	renamedPrompt, err := s.execReconciliationUpdate(
		ctx,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'prompt' LIMIT 1`,
		"ALTER TABLE issues RENAME COLUMN `prompt` TO agent_prompt",
		"rename prompt column to agent_prompt",
	)
	if err != nil {
		return false, err
	}
	changed = changed || renamedPrompt

	addedPrompt, err := execIgnoreAlreadyExists(ctx, s.db, "ALTER TABLE issues ADD COLUMN agent_prompt TEXT NULL AFTER `description`")
	if err != nil {
		return false, err
	}
	changed = changed || addedPrompt

	// Earlier add-column ran before the NULL declaration took effect on
	// some workspaces. Re-relax idempotently.
	relaxedPrompt, err := execIgnoreAlreadyExists(ctx, s.db, "ALTER TABLE issues MODIFY agent_prompt TEXT NULL")
	if err != nil {
		return false, err
	}
	changed = changed || relaxedPrompt

	statusChanged, err := s.ensureUnifiedStatusSchema(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || statusChanged

	topicsChanged, err := s.ensureIssueTopics(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || topicsChanged

	ranksChanged, err := s.ensureIssueRanks(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || ranksChanged

	priorityChanged, err := s.resetPrioritiesToNormal(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || priorityChanged

	// Workspaces created on the pre-canonical issue_history schema may
	// still have that legacy table. Drop is safe and idempotent.
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS issue_history`); err != nil {
		return false, fmt.Errorf("drop legacy issue_history table: %w", err)
	}

	return changed, nil
}

// reconcileLegacySchema is the probe-gated reconciliation that brings a
// pre-goose workspace to the converged shape encoded in 00001_baseline.sql.
// It runs only during adoption (adoptPreGooseWorkspace); fresh workspaces
// reach the converged shape directly via baseline.sql, and already-on-goose
// workspaces evolve through registered goose migrations.
//
// Now a thin wrapper around ensureCanonicalSchema + convergeLegacyAlterations
// — the two unconditional passes that runMigrations also calls every Open.
// Kept as a named entry point because the adoption path is the documented
// place this convergence happens, and tests reference it.
func (s *Store) reconcileLegacySchema(ctx context.Context) error {
	if _, err := s.ensureCanonicalSchema(ctx); err != nil {
		return err
	}
	if _, err := s.convergeLegacyAlterations(ctx); err != nil {
		return err
	}
	return nil
}

type issueCheckConstraint struct {
	name   string
	clause string
}

func (s *Store) ensureUnifiedStatusSchema(ctx context.Context) (bool, error) {
	// [LAW:one-source-of-truth] `status` is the canonical workflow state for
	// non-container issues. Containers derive state from children and store NULL.
	changed := false
	// Existing workspaces created before status was nullable still have the
	// column declared NOT NULL. Relax it before any backfill that needs to
	// write NULL. Dolt rejects MODIFY on a column that already matches, so
	// the helper swallows "already" errors via execIgnoreAlreadyExists.
	relaxedChanged, err := execIgnoreAlreadyExists(ctx, s.db, `ALTER TABLE issues MODIFY status VARCHAR(32) NULL`)
	if err != nil {
		return false, err
	}
	changed = changed || relaxedChanged
	legacyStatusUpdates := []struct {
		probe   string
		stmt    string
		context string
	}{
		{
			probe:   `SELECT 1 FROM issues WHERE status = 'in-progress' LIMIT 1`,
			stmt:    `UPDATE issues SET status = 'in_progress' WHERE status = 'in-progress'`,
			context: "normalize legacy in-progress status",
		},
		{
			probe:   `SELECT 1 FROM issues WHERE status = 'todo' LIMIT 1`,
			stmt:    `UPDATE issues SET status = 'open' WHERE status = 'todo'`,
			context: "normalize legacy todo status",
		},
		{
			probe:   `SELECT 1 FROM issues WHERE status = 'done' LIMIT 1`,
			stmt:    `UPDATE issues SET status = 'closed' WHERE status = 'done'`,
			context: "normalize legacy done status",
		},
		{
			probe:   `SELECT 1 FROM issues WHERE status NOT IN ('open','in_progress','closed') LIMIT 1`,
			stmt:    `UPDATE issues SET status = 'open' WHERE status NOT IN ('open','in_progress','closed')`,
			context: "normalize invalid status",
		},
		{
			probe:   `SELECT 1 FROM issues WHERE closed_at IS NOT NULL AND status <> 'closed' LIMIT 1`,
			stmt:    `UPDATE issues SET status = 'closed' WHERE closed_at IS NOT NULL AND status <> 'closed'`,
			context: "normalize closed_at status",
		},
		{
			probe:   `SELECT 1 FROM issues WHERE status <> 'closed' AND closed_at IS NOT NULL LIMIT 1`,
			stmt:    `UPDATE issues SET closed_at = NULL WHERE status <> 'closed'`,
			context: "normalize non-closed closed_at",
		},
		{
			// [LAW:one-source-of-truth] Containers derive state from children;
			// any persisted status on an epic row is dead data left over from
			// the pre-derivation schema. NULL it so the column stops lying and
			// future readers that touch i.status on an epic fail loudly.
			probe:   `SELECT 1 FROM issues WHERE issue_type IN ('epic') AND status IS NOT NULL LIMIT 1`,
			stmt:    `UPDATE issues SET status = NULL WHERE issue_type IN ('epic')`,
			context: "null out container status",
		},
	}
	for _, update := range legacyStatusUpdates {
		updateChanged, err := s.execReconciliationUpdate(ctx, update.probe, update.stmt, update.context)
		if err != nil {
			return false, err
		}
		changed = changed || updateChanged
	}
	constraintChanged, err := s.ensureStatusConstraint(ctx)
	if err != nil {
		return false, err
	}
	changed = changed || constraintChanged
	return changed, nil
}

func (s *Store) ensureIssueTopics(ctx context.Context) (bool, error) {
	// [LAW:single-enforcer] Legacy topic repair happens in one SQL reconciliation stage instead of a second Go defaulting path.
	return s.execReconciliationUpdate(
		ctx,
		`SELECT 1 FROM issues WHERE TRIM(COALESCE(topic, '')) = '' LIMIT 1`,
		`UPDATE issues SET topic = 'misc' WHERE TRIM(COALESCE(topic, '')) = ''`,
		"backfill legacy issue topics",
	)
}

func (s *Store) ensureIssueRanks(ctx context.Context) (bool, error) {
	// Assign ranks to any issues that don't have one yet, preserving the
	// previous default ordering (status, priority, updated_at, id) as the
	// initial rank sequence.
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM issues WHERE item_rank = '' ORDER BY status ASC, priority ASC, updated_at DESC, id ASC")
	if err != nil {
		return false, fmt.Errorf("ensureIssueRanks: query unranked: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, fmt.Errorf("ensureIssueRanks: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("ensureIssueRanks: rows: %w", err)
	}
	if len(ids) == 0 {
		return false, nil
	}
	current := rank.Initial()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?", current, id); err != nil {
			return false, fmt.Errorf("ensureIssueRanks: update %s: %w", id, err)
		}
		current = rank.After(current)
	}
	return true, nil
}

// resetPrioritiesToNormal performs the one-shot data migration described in
// links-priority-2r6: collapse the legacy 0..4 priority range to {normal=0,
// urgent=1} by resetting all existing priorities to normal, then install the
// canonical CHECK constraint. Gated by the CHECK constraint shape itself: a
// table whose only priority constraint is `priority >= 0 AND priority <= 1`
// is already on the canonical schema (fresh-create or post-migration), so
// the function returns without writing. Otherwise it resets all rows to 0
// before replacing the CHECK so the new constraint can never reject the
// existing data. [LAW:dataflow-not-control-flow] [LAW:single-enforcer]
func (s *Store) resetPrioritiesToNormal(ctx context.Context) (bool, error) {
	constraints, err := s.listIssuePriorityCheckConstraints(ctx)
	if err != nil {
		return false, err
	}
	if hasCanonicalPriorityConstraint(constraints) {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("UPDATE issues SET priority = %d", model.PriorityNormal)); err != nil {
		return false, fmt.Errorf("reset priorities to normal: %w", err)
	}
	for _, c := range constraints {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE issues DROP CHECK `"+strings.ReplaceAll(c.name, "`", "``")+"`"); err != nil {
			return false, fmt.Errorf("drop priority check %s: %w", c.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE issues ADD CONSTRAINT issues_priority_check CHECK (%s)", priorityCheckClause)); err != nil {
		return false, fmt.Errorf("add priority check: %w", err)
	}
	return true, nil
}

func (s *Store) listIssuePriorityCheckConstraints(ctx context.Context) ([]issueCheckConstraint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tc.constraint_name, cc.check_clause
		FROM information_schema.table_constraints tc
		JOIN information_schema.check_constraints cc
		  ON tc.constraint_schema = cc.constraint_schema
		 AND tc.constraint_name = cc.constraint_name
		WHERE tc.table_schema = DATABASE()
		  AND tc.table_name = 'issues'
		  AND tc.constraint_type = 'CHECK'`)
	if err != nil {
		return nil, fmt.Errorf("query issue check constraints: %w", err)
	}
	defer rows.Close()
	out := []issueCheckConstraint{}
	for rows.Next() {
		var c issueCheckConstraint
		if err := rows.Scan(&c.name, &c.clause); err != nil {
			return nil, fmt.Errorf("scan issue check constraint: %w", err)
		}
		if strings.Contains(normalizeConstraintClause(c.clause), "priority") {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue check constraints: %w", err)
	}
	return out, nil
}

func hasCanonicalPriorityConstraint(constraints []issueCheckConstraint) bool {
	if len(constraints) != 1 {
		return false
	}
	normalized := normalizeConstraintClause(constraints[0].clause)
	// [LAW:one-source-of-truth] Discriminator derived from PriorityUrgent — the
	// upper bound is what differs between the legacy (<=4) and canonical (<=1)
	// shapes.
	return strings.Contains(normalized, fmt.Sprintf("priority<=%d", model.PriorityUrgent))
}

func (s *Store) ensureStatusConstraint(ctx context.Context) (bool, error) {
	checks, err := s.listIssueStatusCheckConstraints(ctx)
	if err != nil {
		return false, err
	}
	if hasCanonicalStatusConstraint(checks) {
		return false, nil
	}
	for _, constraint := range checks {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE issues DROP CHECK `"+strings.ReplaceAll(constraint.name, "`", "``")+"`"); err != nil {
			return false, fmt.Errorf("drop status check %s: %w", constraint.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE issues ADD CONSTRAINT issues_status_check CHECK (`+canonicalStatusCheckClause+`)`); err != nil {
		return false, fmt.Errorf("add canonical status check: %w", err)
	}
	return true, nil
}

func (s *Store) listIssueStatusCheckConstraints(ctx context.Context) ([]issueCheckConstraint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tc.constraint_name, cc.check_clause
		FROM information_schema.table_constraints tc
		JOIN information_schema.check_constraints cc
		  ON tc.constraint_schema = cc.constraint_schema
		 AND tc.constraint_name = cc.constraint_name
		WHERE tc.table_schema = DATABASE()
		  AND tc.table_name = 'issues'
		  AND tc.constraint_type = 'CHECK'`)
	if err != nil {
		return nil, fmt.Errorf("query issue check constraints: %w", err)
	}
	defer rows.Close()
	out := []issueCheckConstraint{}
	for rows.Next() {
		var constraint issueCheckConstraint
		if err := rows.Scan(&constraint.name, &constraint.clause); err != nil {
			return nil, fmt.Errorf("scan issue check constraint: %w", err)
		}
		normalized := normalizeConstraintClause(constraint.clause)
		if strings.Contains(normalized, "statusin(") {
			out = append(out, constraint)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue check constraints: %w", err)
	}
	return out, nil
}

func hasCanonicalStatusConstraint(constraints []issueCheckConstraint) bool {
	if len(constraints) != 1 {
		return false
	}
	// Dolt's information_schema.check_clauses may report the clause with or
	// without an outer wrapping pair of parentheses depending on how the
	// constraint was added. Tolerate either side wrapping the other so the
	// migration stays idempotent across normalization differences. Drift past
	// this is caught by TestMigrationIsIdempotentOnSecondOpen.
	actual := normalizeConstraintClause(constraints[0].clause)
	expected := normalizeConstraintClause(canonicalStatusCheckClause)
	return strings.Contains(actual, expected) || strings.Contains(expected, actual)
}

func normalizeConstraintClause(clause string) string {
	replacer := strings.NewReplacer(" ", "", "\t", "", "\n", "", "`", "")
	return strings.ToLower(replacer.Replace(clause))
}

func (s *Store) execReconciliationUpdate(ctx context.Context, probe string, stmt string, contextLabel string) (bool, error) {
	var matched int
	if err := s.db.QueryRowContext(ctx, probe).Scan(&matched); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("%s: probe rows: %w", contextLabel, err)
	}
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return false, fmt.Errorf("%s: %w", contextLabel, err)
	}
	return true, nil
}

func execIgnoreAlreadyExists(ctx context.Context, db *sql.DB, stmt string) (bool, error) {
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		normalized := strings.ToLower(err.Error())
		if strings.Contains(normalized, "already exists") || strings.Contains(normalized, "duplicate column") || strings.Contains(normalized, "duplicate key name") {
			return false, nil
		}
		return false, fmt.Errorf("migrate schema: %w", err)
	}
	return true, nil
}
