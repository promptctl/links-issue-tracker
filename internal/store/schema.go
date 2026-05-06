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
	return fmt.Sprintf(`CREATE TABLE issues (
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
			CHECK(%s),
			CHECK(%s),
			CHECK(issue_type IN ('task','feature','bug','chore','epic'))
		);`, canonicalStatusCheckClause, priorityCheckClause)
}

// migrate is the per-Open schema entry point. It refuses out-of-window
// workspaces, dispatches to the goose-backed runner (which handles fresh /
// pre-goose / already-on-goose shapes), then writes the always-current
// workspace_id meta fixture. Commits the working set exactly once if
// anything changed.
//
// [LAW:single-enforcer] Every workspace shape funnels through runMigrations
// for schema convergence; the per-Open meta fixture is the only thing that
// runs unconditionally outside that path. The compat-window check is the
// only thing that runs *before* the runner and can refuse the entire Open.
// [LAW:dataflow-not-control-flow] The same operations execute every Open;
// what varies is what the runner has to do (apply baseline / adopt / no-op)
// and whether the compat check returns a typed error or nil.
func (s *Store) migrate(ctx context.Context) error {
	if err := checkCompatWindow(ctx, s.db, codeVersion); err != nil {
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
	if !migrated && !workspaceChanged {
		return nil
	}
	return s.commitWorkingSet(ctx, "Migrate links schema")
}

// reconcileLegacySchema is the probe-gated reconciliation that brings a
// pre-goose workspace to the converged shape encoded in 00001_baseline.sql.
// It runs only during adoption (adoptPreGooseWorkspace); fresh workspaces
// reach the converged shape directly via baseline.sql, and already-on-goose
// workspaces evolve through registered goose migrations.
//
// The CREATE TABLE / ALTER ADD COLUMN statements are idempotent via
// execIgnoreAlreadyExists, so even a pre-goose workspace already at the
// converged shape (e.g., one running a binary at the master tip just before
// goose landed) traverses this safely without rewrites.
func (s *Store) reconcileLegacySchema(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE meta (
			meta_key VARCHAR(191) PRIMARY KEY,
			meta_value TEXT NOT NULL
		);`,
		createIssuesTableStmt(),
		`CREATE TABLE relations (
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
		`CREATE TABLE comments (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			body TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE labels (
			issue_id VARCHAR(191) NOT NULL,
			label VARCHAR(191) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			PRIMARY KEY (issue_id, label),
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);`,
		`CREATE INDEX idx_relations_src_type ON relations(src_id, type);`,
		`CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);`,
		`CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);`,
		`CREATE INDEX idx_labels_issue ON labels(issue_id, label);`,
		`CREATE INDEX idx_labels_name ON labels(label, issue_id);`,
		`CREATE TABLE issue_history (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			action TEXT NOT NULL,
			reason TEXT NOT NULL,
			from_status TEXT NOT NULL,
			to_status TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX idx_issue_history_issue_created ON issue_history(issue_id, created_at);`,
	}
	for _, stmt := range schema {
		if _, err := execIgnoreAlreadyExists(ctx, s.db, stmt); err != nil {
			return err
		}
	}
	if _, err := execIgnoreAlreadyExists(ctx, s.db, `ALTER TABLE issues ADD COLUMN item_rank TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if _, err := execIgnoreAlreadyExists(ctx, s.db, `CREATE INDEX idx_issues_rank ON issues(item_rank(191))`); err != nil {
		return err
	}
	if _, err := execIgnoreAlreadyExists(ctx, s.db, `ALTER TABLE issues ADD COLUMN topic VARCHAR(191) NOT NULL DEFAULT 'misc' AFTER issue_type`); err != nil {
		return err
	}
	// Workspaces predating the rename still have the old `prompt` column.
	// Probe-gated rename keeps adoption idempotent across pre-rename and
	// post-rename pre-goose shapes. `prompt` is reserved in Dolt's MySQL
	// parser, so the source-side identifier is backtick-quoted; `agent_prompt`
	// is not reserved and needs no quoting.
	if _, err := s.execReconciliationUpdate(
		ctx,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'prompt' LIMIT 1`,
		"ALTER TABLE issues RENAME COLUMN `prompt` TO agent_prompt",
		"rename prompt column to agent_prompt",
	); err != nil {
		return err
	}
	if _, err := execIgnoreAlreadyExists(ctx, s.db, "ALTER TABLE issues ADD COLUMN agent_prompt TEXT NULL AFTER `description`"); err != nil {
		return err
	}
	// Workspaces where the column was added before the NULL declaration took
	// effect still have it as NOT NULL, which makes `lit new` fail at the DB
	// layer when no --prompt is supplied. Relax to NULL the same way
	// ensureUnifiedStatusSchema relaxes status; the helper swallows the no-op
	// error when the column is already nullable.
	if _, err := execIgnoreAlreadyExists(ctx, s.db, "ALTER TABLE issues MODIFY agent_prompt TEXT NULL"); err != nil {
		return err
	}
	if _, err := s.ensureUnifiedStatusSchema(ctx); err != nil {
		return err
	}
	if _, err := s.ensureIssueTopics(ctx); err != nil {
		return err
	}
	if _, err := s.ensureIssueRanks(ctx); err != nil {
		return err
	}
	if _, err := s.resetPrioritiesToNormal(ctx); err != nil {
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
