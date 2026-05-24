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

// Pre-goose schema reconcile.
//
// This file is the resurrected forward-migration mechanism deleted in
// commit 254f86b. The deletion stranded every workspace at any pre-v1
// canonical shape — the verified-adoption gate that replaced it could
// detect "shape != current baseline" but not bring an earlier shape
// forward, so older workspaces hit "partial schema, restore or recreate"
// (i.e. destroy your data).
//
// The reconcile is a HISTORICAL ARTIFACT. The operations encoded here
// represent the schema evolution that happened BEFORE goose existed.
// No new operations should be added here — every future schema change
// is a numbered goose migration (00002+). Goose owns forward evolution
// from v1 onwards; this file owns "bring a pre-goose workspace to v1."
//
// [LAW:single-enforcer] Reconcile is the single boundary that handles
// pre-goose → v1 forward migration. Goose is the single boundary that
// handles v1 → vN forward evolution. The two are orthogonal; neither
// replaces the other.
//
// [LAW:dataflow-not-control-flow] Every reconcile step probes the
// workspace's current shape and decides skip-vs-execute from the probe
// result. The sequence runs the same on every Open; only the probe
// values vary. There is no "what kind of workspace is this" branch —
// each step independently knows whether it has work to do.
//
// [LAW:types-are-the-program] The ddlStep type carries the variability
// between steps in data (target, parent, stmt), not in code. The runner
// performs the same probe→snapshot→exec sequence for every step; the
// step's identity is its values, not its position in a branch.

// ddlStep names one declarative DDL operation so the schema-list runner
// can derive its existence probe without parsing the SQL. parent is
// empty for CREATE TABLE; for CREATE INDEX it carries the table the
// index lives on.
type ddlStep struct {
	target string
	parent string
	stmt   string
}

// existsProbe returns SQL that yields one row iff this step's target is
// already present in the schema (i.e. the DDL would be a no-op). When
// the probe yields no rows, the runner calls guard.ensure() and runs
// stmt.
func (d ddlStep) existsProbe() string {
	if d.parent == "" {
		return fmt.Sprintf(
			`SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '%s' LIMIT 1`,
			d.target,
		)
	}
	return fmt.Sprintf(
		`SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = '%s' AND index_name = '%s' LIMIT 1`,
		d.parent, d.target,
	)
}

// priorityCheckClause is the schema-level encoding of the priority range
// invariant. Derived from the canonical model.Priority* constants and
// shared by the fresh-table CREATE (createIssuesTableStmt) and the
// upgrade-path ALTER (resetPrioritiesToNormal) so the two writers
// cannot drift. [LAW:one-source-of-truth]
var priorityCheckClause = fmt.Sprintf("priority >= %d AND priority <= %d", model.PriorityNormal, model.PriorityUrgent)

// canonicalStatusCheckClause encodes the invariant that container rows
// store NULL status (state is derived from children) and leaf rows
// carry one of the known states. Shared by createIssuesTableStmt and
// ensureStatusConstraint so the fresh and upgrade paths cannot diverge.
// [LAW:one-source-of-truth]
const canonicalStatusCheckClause = `(issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))`

// createIssuesTableStmt is the v1 canonical shape of the issues table.
// Mirrors the issues table in 00001_baseline.sql exactly, including
// every column (item_rank in particular) and the deterministic
// constraint names. A reconcile-built issues table must be
// byte-equivalent to a baseline-applied one or the schema-drift canary
// (sxsk.4) breaks and downstream migrations that reference the
// constraint names fail.
//
// [LAW:one-source-of-truth] The two definitions (here and
// 00001_baseline.sql) are kept in sync; the drift canary catches
// divergence in CI.
func createIssuesTableStmt() string {
	return fmt.Sprintf(`CREATE TABLE issues (
			id VARCHAR(191) PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			agent_prompt TEXT NULL,
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
			CONSTRAINT issues_status_check CHECK (%s),
			CONSTRAINT issues_priority_check CHECK (%s),
			CONSTRAINT issues_type_check CHECK (issue_type IN ('task','feature','bug','chore','epic'))
		);`, canonicalStatusCheckClause, priorityCheckClause)
}

// reconcileToBaseline brings a pre-goose workspace at any historical
// canonical shape forward to the v1 baseline shape. It is idempotent:
// every step probes for "is this thing already present/canonical?" and
// skips if so. A fresh workspace (no canonical tables) sees every probe
// return absent and reconcile creates every table. A workspace already
// at v1 sees every probe return present and reconcile no-ops.
//
// Returns (changed, err). changed is true iff any step performed a
// write. The caller (runMigration) commits and the snapshotGuard
// fires before the first write.
//
// [LAW:dataflow-not-control-flow] The same step sequence runs every
// invocation; the changed bit is data computed from probe results.
// [LAW:single-enforcer] reconcileToBaseline is the one place that
// orders reconcile stages; callers do not partial-order them.
func (s *Store) reconcileToBaseline(ctx context.Context, guard *snapshotGuard) (bool, error) {
	// Precondition: verifyIssuesReconcileable was called by runMigration
	// before the snapshot guard fired, so this function assumes the
	// issues table (if present) carries the columns downstream steps
	// reference. Routing the check through the caller — not inside
	// reconcile itself — keeps the snapshot from firing on workspaces
	// reconcile cannot help with, so failed Opens don't accumulate
	// recovery snapshots without bound.
	schema := []ddlStep{
		{target: "meta", stmt: `CREATE TABLE meta (
			meta_key VARCHAR(191) PRIMARY KEY,
			meta_value TEXT NOT NULL
		);`},
		{target: "issues", stmt: createIssuesTableStmt()},
		{target: "relations", stmt: `CREATE TABLE relations (
			src_id VARCHAR(191) NOT NULL,
			dst_id VARCHAR(191) NOT NULL,
			type VARCHAR(32) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			PRIMARY KEY (src_id, dst_id, type),
			FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE,
			FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE,
			CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))
		);`},
		{target: "comments", stmt: `CREATE TABLE comments (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			body TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`},
		{target: "labels", stmt: `CREATE TABLE labels (
			issue_id VARCHAR(191) NOT NULL,
			label VARCHAR(191) NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			PRIMARY KEY (issue_id, label),
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`},
		{target: "idx_issues_status_priority", parent: "issues", stmt: `CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);`},
		{target: "idx_relations_src_type", parent: "relations", stmt: `CREATE INDEX idx_relations_src_type ON relations(src_id, type);`},
		{target: "idx_relations_dst_type", parent: "relations", stmt: `CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);`},
		{target: "idx_comments_issue_created", parent: "comments", stmt: `CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);`},
		{target: "idx_labels_issue", parent: "labels", stmt: `CREATE INDEX idx_labels_issue ON labels(issue_id, label);`},
		{target: "idx_labels_name", parent: "labels", stmt: `CREATE INDEX idx_labels_name ON labels(label, issue_id);`},
		// [LAW:one-source-of-truth] issue_events is the canonical mutation
		// log for every issue field. The legacy issue_history schema
		// (status-only, from/to columns that lied for archive/delete) is
		// dropped below.
		{target: "issue_events", stmt: `CREATE TABLE issue_events (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			action VARCHAR(64) NULL,
			reason TEXT NOT NULL,
			actor TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		);`},
		{target: "issue_event_changes", stmt: `CREATE TABLE issue_event_changes (
			event_id VARCHAR(191) NOT NULL,
			field VARCHAR(64) NOT NULL,
			from_value TEXT NULL,
			to_value TEXT NULL,
			PRIMARY KEY (event_id, field),
			FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE
		);`},
		{target: "idx_issue_events_issue_created", parent: "issue_events", stmt: `CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at);`},
	}
	changed := false
	for _, step := range schema {
		stmtChanged, err := s.runGatedCreate(ctx, guard, step)
		if err != nil {
			return changed, err
		}
		changed = changed || stmtChanged
	}
	// [LAW:one-source-of-truth] issue_history is superseded by
	// issue_events + issue_event_changes. Existing repos may still
	// have it; drop it (existing history rows are discarded — issues
	// are untouched).
	dropHistoryChanged, err := s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'issue_history' LIMIT 1`,
		`DROP TABLE IF EXISTS issue_history`,
		"drop legacy issue_history table",
	)
	if err != nil {
		return changed, err
	}
	changed = changed || dropHistoryChanged
	// issue_events.assignee was renamed to actor. Probe-gated rename
	// keeps the migration idempotent across fresh / migrated / pre-rename
	// workspace states.
	actorColumnChanged, err := s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issue_events' AND column_name = 'assignee' LIMIT 1`,
		`ALTER TABLE issue_events RENAME COLUMN assignee TO actor`,
		"rename issue_events.assignee to actor",
	)
	if err != nil {
		return changed, err
	}
	changed = changed || actorColumnChanged
	rankColumnChanged, err := s.execGatedColumnAdd(ctx, guard, "issues", "item_rank",
		`ALTER TABLE issues ADD COLUMN item_rank TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return changed, err
	}
	changed = changed || rankColumnChanged
	rankIndexChanged, err := s.runGatedCreate(ctx, guard, ddlStep{
		target: "idx_issues_rank",
		parent: "issues",
		stmt:   `CREATE INDEX idx_issues_rank ON issues(item_rank(191))`,
	})
	if err != nil {
		return changed, err
	}
	changed = changed || rankIndexChanged
	topicColumnChanged, err := s.execGatedColumnAdd(ctx, guard, "issues", "topic",
		`ALTER TABLE issues ADD COLUMN topic VARCHAR(191) NOT NULL DEFAULT 'misc' AFTER issue_type`)
	if err != nil {
		return changed, err
	}
	changed = changed || topicColumnChanged
	// Workspaces predating the rename still have the old `prompt`
	// column. Probe-gated rename keeps migration idempotent across
	// fresh / migrated / pre-rename workspace states. `prompt` is
	// reserved in Dolt's MySQL parser, so the source-side identifier
	// is backtick-quoted; `agent_prompt` is not reserved and needs no
	// quoting.
	promptRenamedChanged, err := s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'prompt' LIMIT 1`,
		"ALTER TABLE issues RENAME COLUMN `prompt` TO agent_prompt",
		"rename prompt column to agent_prompt",
	)
	if err != nil {
		return changed, err
	}
	changed = changed || promptRenamedChanged
	promptColumnChanged, err := s.execGatedColumnAdd(ctx, guard, "issues", "agent_prompt",
		"ALTER TABLE issues ADD COLUMN agent_prompt TEXT NULL AFTER `description`")
	if err != nil {
		return changed, err
	}
	changed = changed || promptColumnChanged
	// Workspaces where the column was added before the NULL declaration
	// took effect still have it as NOT NULL, which makes `lit new` fail
	// at the DB layer when no --prompt is supplied. Relax to NULL by
	// probing the is_nullable bit and modifying only when needed.
	promptRelaxedChanged, err := s.execGatedColumnRelax(ctx, guard, "issues", "agent_prompt",
		"ALTER TABLE issues MODIFY agent_prompt TEXT NULL")
	if err != nil {
		return changed, err
	}
	changed = changed || promptRelaxedChanged
	statusChanged, err := s.ensureUnifiedStatusSchema(ctx, guard)
	if err != nil {
		return changed, err
	}
	changed = changed || statusChanged
	topicChanged, err := s.ensureIssueTopics(ctx, guard)
	if err != nil {
		return changed, err
	}
	changed = changed || topicChanged
	rankChanged, err := s.ensureIssueRanks(ctx, guard)
	if err != nil {
		return changed, err
	}
	changed = changed || rankChanged
	priorityChanged, err := s.resetPrioritiesToNormal(ctx, guard)
	if err != nil {
		return changed, err
	}
	changed = changed || priorityChanged
	workspaceChanged, err := s.ensureMetaValue(ctx, guard, "workspace_id", s.workspaceID)
	if err != nil {
		return changed, err
	}
	changed = changed || workspaceChanged
	return changed, nil
}

// verifyIssuesReconcileable fast-fails when the issues table exists but
// is missing columns the reconcile's downstream steps depend on. An
// issues table that absent altogether is fine — reconcile will CREATE
// it via createIssuesTableStmt. The failure mode this guards is the
// synthetic-corruption shape (an issues table with only `id`) — no
// real pre-goose workspace shape had issues without status / priority /
// updated_at.
//
// [LAW:single-enforcer] One structural-precondition probe at the
// reconcile boundary; downstream steps trust that their preconditions
// hold.
// [LAW:no-silent-fallbacks] A specific, named structural error is
// emitted before any mutation; the operator sees the actual anomaly.
func (s *Store) verifyIssuesReconcileable(ctx context.Context) error {
	cols, err := s.tableColumns(ctx, "issues")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		// issues table doesn't exist at all — reconcile's CREATE TABLE
		// step will produce the canonical shape from scratch.
		return nil
	}
	required := []string{"status", "priority", "updated_at"}
	var missing []string
	for _, c := range required {
		if !cols[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"workspace's issues table is missing reconcile prerequisites (%s); "+
				"the shape is structurally beyond what pre-goose reconcile can recover — "+
				"this is not a known historical shape",
			strings.Join(missing, ", "),
		)
	}
	return nil
}

// runGatedCreate is the schema-list runner — probes existence, snapshots
// before first mutation, then runs the CREATE. The "already exists"
// swallow is a belt-and-suspenders against probe/exec races; under
// normal operation the probe alone determines the no-op case.
func (s *Store) runGatedCreate(ctx context.Context, guard *snapshotGuard, step ddlStep) (bool, error) {
	return s.execGatedCreate(ctx, guard, step.existsProbe(), step.stmt,
		fmt.Sprintf("create %s", step.target))
}

// execGatedColumnAdd probes for an existing column and runs the ADD
// only when it is missing. Swallow handles benign races where the
// column appeared between probe and exec.
func (s *Store) execGatedColumnAdd(ctx context.Context, guard *snapshotGuard, table, column, stmt string) (bool, error) {
	probe := fmt.Sprintf(
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '%s' AND column_name = '%s' LIMIT 1`,
		table, column,
	)
	return s.execGatedCreate(ctx, guard, probe, stmt,
		fmt.Sprintf("add column %s.%s", table, column))
}

// execGatedColumnRelax probes for a NOT NULL column and runs MODIFY
// only when the column is still declared NOT NULL. is_nullable = 'NO'
// is the canonical-source discriminator. A MODIFY that errors signals
// a structural problem (column missing, type mismatch) — propagate it
// instead of swallowing.
func (s *Store) execGatedColumnRelax(ctx context.Context, guard *snapshotGuard, table, column, stmt string) (bool, error) {
	probe := fmt.Sprintf(
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '%s' AND column_name = '%s' AND is_nullable = 'NO' LIMIT 1`,
		table, column,
	)
	return s.execGatedMutation(ctx, guard, probe, stmt,
		fmt.Sprintf("relax column %s.%s to nullable", table, column))
}

// execGatedCreate gates a CREATE/ADD-style statement on an existence
// probe. If the probe matches (target already present), the statement
// is skipped. Otherwise the snapshot guard is armed and the statement
// runs; an "already exists" / "duplicate column" / "duplicate key
// name" error after the probe is treated as a benign race (another
// writer landed the same shape between probe and exec) and silently
// succeeds.
//
// [LAW:single-enforcer] All CREATE-style DDL flows through this
// driver. The swallow set lives here, in one place, instead of being
// scattered across the schema-list and ADD-COLUMN callsites.
func (s *Store) execGatedCreate(ctx context.Context, guard *snapshotGuard, probe, stmt, label string) (bool, error) {
	exists, err := s.probeYields(ctx, probe, label)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, snapErr := guard.ensure(); snapErr != nil {
		return false, fmt.Errorf("%s: %w", label, snapErr)
	}
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		normalized := strings.ToLower(err.Error())
		if strings.Contains(normalized, "already exists") || strings.Contains(normalized, "duplicate column") || strings.Contains(normalized, "duplicate key name") {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", label, err)
	}
	return true, nil
}

// execGatedMutation gates an UPDATE / RENAME / DROP / MODIFY statement
// on a "needs work" probe. If the probe matches (work-to-do detected),
// the snapshot guard is armed and the statement runs. If the statement
// errors, the error propagates verbatim — these statement classes have
// no benign "already exists" failure mode, and silencing one would let
// Open succeed against a drifted schema.
//
// [LAW:single-enforcer] All probe-gated mutation flows through this
// driver; callsites do not implement probe/exec sequences inline.
// [LAW:types-are-the-program] The split between this helper and
// execGatedCreate puts the swallow-vs-propagate semantics in the
// function name, so the next callsite cannot accidentally get the
// wrong behavior.
func (s *Store) execGatedMutation(ctx context.Context, guard *snapshotGuard, probe, stmt, label string) (bool, error) {
	needed, err := s.probeYields(ctx, probe, label)
	if err != nil {
		return false, err
	}
	if !needed {
		return false, nil
	}
	if _, snapErr := guard.ensure(); snapErr != nil {
		return false, fmt.Errorf("%s: %w", label, snapErr)
	}
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	return true, nil
}

// probeYields runs probe and reports whether it returned at least one
// row. A driver-level error other than sql.ErrNoRows propagates wrapped
// with label for callsite context.
func (s *Store) probeYields(ctx context.Context, probe, label string) (bool, error) {
	var marker int
	err := s.db.QueryRowContext(ctx, probe).Scan(&marker)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("%s: probe: %w", label, err)
}

type issueCheckConstraint struct {
	name   string
	clause string
}

func (s *Store) ensureUnifiedStatusSchema(ctx context.Context, guard *snapshotGuard) (bool, error) {
	// [LAW:one-source-of-truth] `status` is the canonical workflow state
	// for non-container issues. Containers derive state from children
	// and store NULL.
	changed := false
	// Existing workspaces created before status was nullable still have
	// the column declared NOT NULL. Relax it before any backfill that
	// needs to write NULL. The probe checks is_nullable='NO' so the
	// MODIFY only fires on workspaces whose schema still carries the
	// non-canonical declaration.
	relaxedChanged, err := s.execGatedColumnRelax(ctx, guard, "issues", "status",
		`ALTER TABLE issues MODIFY status VARCHAR(32) NULL`)
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
			// [LAW:single-enforcer] The UPDATE predicate matches the
			// probe exactly so the UPDATE touches only inconsistent
			// rows. The old shape (UPDATE filtered only on
			// `status <> 'closed'`) was a full-table write on every
			// run.
			probe:   `SELECT 1 FROM issues WHERE status <> 'closed' AND closed_at IS NOT NULL LIMIT 1`,
			stmt:    `UPDATE issues SET closed_at = NULL WHERE status <> 'closed' AND closed_at IS NOT NULL`,
			context: "normalize non-closed closed_at",
		},
		{
			// [LAW:one-source-of-truth] Containers derive state from
			// children; any persisted status on an epic row is dead
			// data left over from the pre-derivation schema. NULL it
			// so the column stops lying and future readers that touch
			// i.status on an epic fail loudly.
			// [LAW:single-enforcer] The UPDATE predicate matches the
			// probe exactly (also gating on `status IS NOT NULL`) so
			// the UPDATE writes only inconsistent rows. The probe-only
			// shape would full-scan the epic set on every reconcile.
			probe:   `SELECT 1 FROM issues WHERE issue_type IN ('epic') AND status IS NOT NULL LIMIT 1`,
			stmt:    `UPDATE issues SET status = NULL WHERE issue_type IN ('epic') AND status IS NOT NULL`,
			context: "null out container status",
		},
	}
	for _, update := range legacyStatusUpdates {
		updateChanged, err := s.execGatedMutation(ctx, guard, update.probe, update.stmt, update.context)
		if err != nil {
			return false, err
		}
		changed = changed || updateChanged
	}
	constraintChanged, err := s.ensureStatusConstraint(ctx, guard)
	if err != nil {
		return false, err
	}
	changed = changed || constraintChanged
	return changed, nil
}

func (s *Store) ensureIssueTopics(ctx context.Context, guard *snapshotGuard) (bool, error) {
	// [LAW:single-enforcer] Legacy topic repair happens in one SQL
	// reconciliation stage instead of a second Go defaulting path.
	return s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM issues WHERE TRIM(COALESCE(topic, '')) = '' LIMIT 1`,
		`UPDATE issues SET topic = 'misc' WHERE TRIM(COALESCE(topic, '')) = ''`,
		"backfill legacy issue topics",
	)
}

func (s *Store) ensureIssueRanks(ctx context.Context, guard *snapshotGuard) (bool, error) {
	// Assign ranks to any issues that don't have one yet, preserving the
	// previous default ordering (status, priority, updated_at, id) as
	// the initial rank sequence.
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
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("ensureIssueRanks: %w", err)
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

// resetPrioritiesToNormal performs the one-shot data migration described
// in links-priority-2r6: collapse the legacy 0..4 priority range to
// {normal=0, urgent=1} by resetting all existing priorities to normal,
// then install the canonical CHECK constraint. Gated by the CHECK
// constraint shape itself: a table whose only priority constraint is
// `priority >= 0 AND priority <= 1` is already on the canonical schema
// (fresh-create or post-migration), so the function returns without
// writing. Otherwise it resets all rows to 0 before replacing the
// CHECK so the new constraint can never reject the existing data.
// [LAW:dataflow-not-control-flow] [LAW:single-enforcer]
func (s *Store) resetPrioritiesToNormal(ctx context.Context, guard *snapshotGuard) (bool, error) {
	constraints, err := s.listIssuePriorityCheckConstraints(ctx)
	if err != nil {
		return false, err
	}
	if hasCanonicalPriorityConstraint(constraints) {
		return false, nil
	}
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("reset priorities to normal: %w", err)
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
	// [LAW:one-source-of-truth] Discriminator derived from
	// PriorityUrgent — the upper bound is what differs between the
	// legacy (<=4) and canonical (<=1) shapes.
	return strings.Contains(normalized, fmt.Sprintf("priority<=%d", model.PriorityUrgent))
}

func (s *Store) ensureStatusConstraint(ctx context.Context, guard *snapshotGuard) (bool, error) {
	checks, err := s.listIssueStatusCheckConstraints(ctx)
	if err != nil {
		return false, err
	}
	if hasCanonicalStatusConstraint(checks) {
		return false, nil
	}
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("ensure status constraint: %w", err)
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
	// Dolt rewrites the canonical clause into a structurally equivalent
	// but textually different form on round-trip: NOT IN becomes
	// NOT(.. IN ..), IS NOT NULL becomes NOT(.. IS NULL), and column
	// refs get backticks. A full-text comparison fails on the rewritten
	// form, which would force the migration to drop+re-add the
	// constraint every Open. Fingerprint the structural tokens that
	// uniquely identify the canonical clause and accept either
	// rendering of NOT-IN / IS-NOT-NULL.
	//
	// [LAW:one-source-of-truth] These tokens are derived from
	// canonicalStatusCheckClause; if that constant changes, this
	// matcher changes alongside.
	normalized := normalizeConstraintClause(constraints[0].clause)
	if !strings.Contains(normalized, "issue_typein('epic')") {
		return false
	}
	if !strings.Contains(normalized, "statusin('open','in_progress','closed')") {
		return false
	}
	if !strings.Contains(normalized, "statusisnotnull") && !strings.Contains(normalized, "not(statusisnull)") {
		return false
	}
	if !strings.Contains(normalized, "andstatusisnull") {
		return false
	}
	if !hasNegatedEpicGuard(normalized) {
		return false
	}
	return true
}

// hasNegatedEpicGuard reports whether the already-normalized clause
// carries a leaf-arm filter excluding epic rows. Accepts both the
// canonical "issue_typenotin('epic')" form and Dolt's
// "not(...issue_typein('epic')...)" rewrites with arbitrary
// inner-paren depth.
func hasNegatedEpicGuard(normalized string) bool {
	if strings.Contains(normalized, "issue_typenotin('epic')") {
		return true
	}
	const negation = "not("
	const positive = "issue_typein('epic')"
	cursor := 0
	for {
		offset := strings.Index(normalized[cursor:], negation)
		if offset < 0 {
			return false
		}
		pos := cursor + offset + len(negation)
		for pos < len(normalized) && normalized[pos] == '(' {
			pos++
		}
		if strings.HasPrefix(normalized[pos:], positive) {
			return true
		}
		cursor = cursor + offset + 1
	}
}

func normalizeConstraintClause(clause string) string {
	replacer := strings.NewReplacer(" ", "", "\t", "", "\n", "", "`", "")
	return strings.ToLower(replacer.Replace(clause))
}
