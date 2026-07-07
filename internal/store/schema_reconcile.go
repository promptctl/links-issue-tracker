package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
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

// quotedIssueTypeList renders a type set as the SQL literal list the CHECK
// clauses embed: 'a','b','c' in canonical order.
func quotedIssueTypeList(types []model.IssueType) string {
	quoted := make([]string, len(types))
	for i, t := range types {
		quoted[i] = "'" + string(t) + "'"
	}
	return strings.Join(quoted, ",")
}

// issueTypeCheckClause and containerTypeMembership are the schema-level
// encodings of the issue-type vocabulary and its container subset. Derived
// from the canonical model.IssueTypes list and the IsContainer predicate so
// the schema cannot drift from the sealed type; byte-identical to the
// literals reconcile has always installed, so existing workspaces see no
// churn and the normalized-clause probes keep matching. [LAW:one-source-of-truth]
var (
	issueTypeCheckClause    = fmt.Sprintf("issue_type IN (%s)", quotedIssueTypeList(model.IssueTypes))
	containerTypeMembership = fmt.Sprintf("issue_type IN (%s)", quotedIssueTypeList(model.ContainerTypes()))
)

// canonicalStatusCheckClause encodes the invariant that container rows
// store NULL status (state is derived from children) and leaf rows
// carry one of the known states. Shared by createIssuesTableStmt and
// ensureStatusConstraint so the fresh and upgrade paths cannot diverge.
// [LAW:one-source-of-truth]
var canonicalStatusCheckClause = fmt.Sprintf(
	`(issue_type IN (%[1]s) AND status IS NULL) OR (issue_type NOT IN (%[1]s) AND status IS NOT NULL AND status IN ('open','in_progress','closed'))`,
	quotedIssueTypeList(model.ContainerTypes()))

// createIssuesTableStmt is the v1 canonical shape of the issues table.
// Mirrors the issues table in 00001_baseline.sql exactly, including
// every column (item_rank in particular) and the deterministic
// constraint names. A reconcile-built issues table must be
// byte-equivalent to a baseline-applied one or downstream migrations
// that reference the constraint names fail and the post-reconcile
// verifyBaselineShape gate rejects the stamp.
//
// [LAW:one-source-of-truth] The two definitions (here and
// 00001_baseline.sql) are kept in sync by convention; the project's
// existing drift canary (sxsk.4) compares a fresh goose-applied
// workspace to schema_snapshot.sql but does NOT directly compare
// reconcile-built tables, so the synchronization is enforced by
// (a) human review of any change to either definition and (b) the
// TestReconcileCreatedTablesMatchBaselineConstraintNames test which
// asserts the named constraints reconcile installs match the
// baseline names.
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
			CONSTRAINT issues_type_check CHECK (%s)
		);`, canonicalStatusCheckClause, priorityCheckClause, issueTypeCheckClause)
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
	// Precondition: verifyIssuesReconcilable was called by runMigration
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
		// translated row-by-row into issue_events (+ issue_event_changes
		// for status transitions) by translateIssueHistoryToEvents below,
		// then the table itself is dropped. [LAW:no-silent-failure] —
		// the legacy→v1 bridge no longer destroys audit history.
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
	// [LAW:single-enforcer] Reconcile is the legacy→v1 bridge and owns
	// BOTH schema translation AND bookkeeping cleanup. A workspace that
	// reaches this function was classified phaseAdopt — by definition
	// NOT phaseManaged — so any rows present in goose_db_version are
	// fabricated: an older buggy binary inserted them without the
	// migrations actually running (field signature: every row carries
	// the same tstamp, evidence of a single INSERT loop). Drop the
	// table; adoptPreGooseWorkspace recreates it and stamps the
	// baseline cleanly.
	//
	// [LAW:types-are-the-program] The pre-condition for adoption is
	// "no goose log, or a goose log we know is empty"; this gate
	// enforces that pre-condition by construction so adoption's
	// CreateVersionTable + Insert(baseline) cannot collide with lying
	// rows the workspace carried in.
	dropFabricatedGooseLog, err := s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'goose_db_version' LIMIT 1`,
		`DROP TABLE goose_db_version`,
		"drop fabricated goose_db_version (legacy workspace carried lying bookkeeping)",
	)
	if err != nil {
		return changed, err
	}
	changed = changed || dropFabricatedGooseLog
	// issue_events.assignee was renamed to actor. Probe-gated rename
	// keeps the migration idempotent across fresh / migrated / pre-rename
	// workspace states. MUST run BEFORE translateIssueHistoryToEvents so
	// the translation's INSERT targets the canonical column name on every
	// shape, including workspaces that still carry the pre-rename column.
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
	// [LAW:no-silent-failure] Before issue_history goes away, lift every
	// row that maps cleanly onto the canonical event log forward — the
	// previous drop-without-translate path silently discarded audit data
	// on every legacy→v1 bridge. MUST run AFTER the assignee→actor rename
	// above; the translation writes to issue_events.actor, which exists
	// only after the rename has landed on workspaces that pre-date it.
	translatedHistoryChanged, err := s.translateIssueHistoryToEvents(ctx, guard)
	if err != nil {
		return changed, err
	}
	changed = changed || translatedHistoryChanged
	// [LAW:one-source-of-truth] issue_history is superseded by
	// issue_events + issue_event_changes. Translation above preserves
	// every row whose schema participates in the canonical mapping;
	// the drop here removes the legacy table after the rows have
	// reached their canonical home (or after the translate step
	// no-oped on a partial/synthetic shape that carries no rows the
	// canonical mapping could express).
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
	// The ADD COLUMN above needed `DEFAULT 'misc'` so existing rows
	// could be inserted without violating NOT NULL. Baseline.sql
	// declares topic as `VARCHAR(191) NOT NULL` with no default, so
	// reconcile-built columns end up structurally different from
	// baseline-built ones unless the default is dropped here.
	//
	// [LAW:one-source-of-truth] Post-reconcile schema must equal the
	// baseline schema byte-for-byte; otherwise the drift canary breaks
	// and future migrations against the canonical shape get surprised.
	topicDefaultDropped, err := s.execGatedMutation(
		ctx,
		guard,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'topic' AND column_default IS NOT NULL LIMIT 1`,
		`ALTER TABLE issues MODIFY topic VARCHAR(191) NOT NULL`,
		"drop topic default to match baseline shape",
	)
	if err != nil {
		return changed, err
	}
	changed = changed || topicDefaultDropped
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

// verifyIssuesReconcilable fast-fails when the issues table exists but
// is missing columns the reconcile's downstream steps depend on. An
// issues table absent altogether is fine — reconcile will CREATE it
// via createIssuesTableStmt. The failure mode this guards is the
// synthetic-corruption shape (an issues table with only `id`) — no
// real pre-goose workspace shape had issues without these columns.
//
// The prerequisite list is the full set of columns reconcile reads
// from or writes against the existing issues table — anything later
// reconcile steps assume is present:
//
//   - status       — idx_issues_status_priority, ensureUnifiedStatusSchema
//     backfills, status CHECK clause
//   - priority     — idx_issues_status_priority, resetPrioritiesToNormal,
//     priority CHECK clause
//   - updated_at   — idx_issues_status_priority, ensureIssueRanks ordering
//   - issue_type   — ensureUnifiedStatusSchema (epic carve-out),
//     topic ADD COLUMN AFTER issue_type, type CHECK clause
//   - closed_at    — ensureUnifiedStatusSchema (closed_at consistency)
//
// Reconcile adds the columns it knows are not part of every historical
// shape (item_rank, topic, agent_prompt) — they are NOT prerequisites.
//
// [LAW:single-enforcer] One structural-precondition probe at the
// reconcile boundary; downstream steps trust that their preconditions
// hold.
// [LAW:no-silent-failure] A specific, named structural error is
// emitted before any mutation; the operator sees the actual anomaly.
// reconcileRequiredIssueColumns is the column set reconcile's downstream steps
// read from the existing issues table: an issues table missing any of them is
// the synthetic-corruption shape reconcile cannot forward-migrate. (The data
// lifeboat enforces its own analogous "recognizable shape" notion via required
// targets in the shapemap registry, derived from the domain model rather than
// from reconcile's step prerequisites.)
var reconcileRequiredIssueColumns = []string{"status", "priority", "updated_at", "issue_type", "closed_at", "description"}

func (s *Store) verifyIssuesReconcilable(ctx context.Context) error {
	cols, err := s.tableColumns(ctx, "issues")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		// issues table doesn't exist at all — reconcile's CREATE TABLE
		// step will produce the canonical shape from scratch.
		return nil
	}
	// description appears in the ALTER TABLE issues ADD COLUMN
	// agent_prompt ... AFTER `description` step, so reconcile fails
	// mid-flight if it is missing.
	var missing []string
	for _, c := range reconcileRequiredIssueColumns {
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

// legacyIssueHistoryColumns is the minimum column set the deleted
// issue_history table must include for the canonical mapping to
// apply (every column the translation reads). The check is presence-
// only: a workspace whose issue_history is a strict superset (extra
// columns from an even-older shape) still translates correctly,
// because the translation only reads the columns named here. A
// table missing any of these columns (synthetic test fixture, half-
// migrated legacy state) has nothing the canonical mapping can
// express and is left for the drop step.
//
// [LAW:one-source-of-truth] Derived from the historical schema; if
// the legacy shape were ever extended retroactively (it will not be —
// the table is removed), this list would extend alongside.
var legacyIssueHistoryColumns = []string{
	"id", "issue_id", "action", "reason", "from_status", "to_status", "created_at", "created_by",
}

// translateIssueHistoryToEvents lifts legacy issue_history rows into
// the canonical issue_events (+ issue_event_changes for status
// transitions) before the table is dropped. Each issue_history row
// becomes one issue_events row carrying the same id, issue_id,
// action, reason, actor (mapped from created_by), and created_at.
// Status transitions (from_status != to_status, with NULL on either
// side counting as transition) emit one issue_event_changes row
// with field='status'.
//
// Translation is gated on three preconditions:
//
//  1. issue_history must exist (no-op otherwise).
//  2. issue_history must carry the full canonical column set
//     (partial/synthetic shapes have nothing to translate; the drop
//     step still removes the table).
//  3. At least one issue_history row must reference an existing
//     issue AND not already be present in issue_events (FK + idempotency).
//
// The per-row INSERT pair runs inside one transaction so the next Open
// observes either every translatable row in issue_events (+ paired
// change rows) or none of them — a half-translated state would make
// the drop step lose any rows that had not yet copied.
//
// [LAW:no-silent-failure] The previous drop-only bridge silently
// destroyed audit history. Translation makes the bridge lossless for
// every row whose shape the canonical mapping accepts.
// [LAW:dataflow-not-control-flow] The per-row loop runs the same
// sequence (INSERT event, conditionally INSERT change) for every row
// that satisfied the SELECT — variability lives in the rows the
// SELECT returns and in each row's status-transition values, not in
// whether the loop runs.
// [LAW:single-enforcer] All issue_events writes flow through one of
// recordEvent (live mutations), the JSONL importer, or this legacy
// translation. No other writer touches the events table.
// [LAW:types-are-the-program] The change-row's event_id by construction
// points at the event row inserted on the same loop iteration — pairing
// is per-row, not via a JOIN that could match a pre-existing unrelated
// event with a colliding id. Idempotency at the event level is encoded
// in the SELECT's NOT EXISTS clause: re-runs do not re-select rows
// whose id is already in issue_events.
func (s *Store) translateIssueHistoryToEvents(ctx context.Context, guard *snapshotGuard) (bool, error) {
	historyExists, err := s.tableExists(ctx, "issue_history")
	if err != nil {
		return false, fmt.Errorf("translate issue_history: probe table: %w", err)
	}
	if !historyExists {
		return false, nil
	}
	cols, err := s.tableColumns(ctx, "issue_history")
	if err != nil {
		return false, fmt.Errorf("translate issue_history: probe columns: %w", err)
	}
	for _, c := range legacyIssueHistoryColumns {
		if !cols[c] {
			// Partial/synthetic shape — no canonical mapping exists for
			// this table's rows. Leave the drop step to remove it.
			return false, nil
		}
	}
	// Cheap pre-check (no snapshot fired) so workspaces whose canonical-
	// shape issue_history has no translatable rows (e.g. all orphans, all
	// already-in-events) skip the snapshot guard and the tx entirely.
	// Existence probe (SELECT 1 ... LIMIT 1 via probeYields) rather than
	// COUNT(*) — only the boolean matters, and the LIMIT keeps the
	// optimizer from scanning every issue_history row on large workspaces.
	// The authoritative read happens inside the tx below; this probe
	// only exists to avoid an unnecessary snapshot.
	hasPending, err := s.probeYields(ctx, `
		SELECT 1
		FROM issue_history h
		WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = h.issue_id)
		  AND NOT EXISTS (SELECT 1 FROM issue_events e WHERE e.id = h.id)
		LIMIT 1
	`, "translate issue_history: pending probe")
	if err != nil {
		return false, err
	}
	if !hasPending {
		return false, nil
	}
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("translate issue_history: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("translate issue_history: begin tx: %w", err)
	}
	// Belt-and-suspenders Rollback so any error path (existing or
	// future-added) cannot leak the transaction. Rollback after Commit
	// returns sql.ErrTxDone and is a no-op per the database/sql contract,
	// so the success path is unaffected. [LAW:types-are-the-program] —
	// the transaction's finalization is encoded once at the boundary
	// instead of N times at each return site that might drift.
	defer func() { _ = tx.Rollback() }()
	// SELECT raw column values inside the tx so the read and the subsequent
	// INSERTs see one atomic snapshot. Canonicalization (TrimSpace +
	// empty→NULL for action, empty→'unknown' for actor, NULL→'' for reason)
	// happens in Go so the rules use the literal-same strings.TrimSpace call
	// recordEvent uses for live event writes — [LAW:one-source-of-truth]
	// for the canonical event-row shape across both writers.
	//
	// Buffer-then-loop (rather than interleaved row.Next() + tx.Exec on
	// the same connection) matches ensureIssueRanks in this file: Go's
	// database/sql does not portably support interleaved Query iteration
	// and Exec on a single tx. The field-observed maximum is ~200 rows
	// per workspace (PR #143's unreal-3d-maps recovery hit 184), so the
	// memory cost is bounded; this is once-per-workspace recovery code,
	// not a hot path.
	const selectTranslatable = `
		SELECT
			h.id,
			h.issue_id,
			h.action,
			h.reason,
			h.created_by,
			h.created_at,
			h.from_status,
			h.to_status
		FROM issue_history h
		WHERE EXISTS (SELECT 1 FROM issues i WHERE i.id = h.issue_id)
		  AND NOT EXISTS (SELECT 1 FROM issue_events e WHERE e.id = h.id)
	`
	queryRows, err := tx.QueryContext(ctx, selectTranslatable)
	if err != nil {
		return false, fmt.Errorf("translate issue_history: query translatable rows: %w", err)
	}
	type translation struct {
		id, issueID, createdAt    string
		action, reason, createdBy sql.NullString
		fromStatus, toStatus      sql.NullString
	}
	var pending []translation
	for queryRows.Next() {
		var t translation
		if err := queryRows.Scan(&t.id, &t.issueID, &t.action, &t.reason, &t.createdBy, &t.createdAt, &t.fromStatus, &t.toStatus); err != nil {
			queryRows.Close()
			return false, fmt.Errorf("translate issue_history: scan row: %w", err)
		}
		pending = append(pending, t)
	}
	if err := queryRows.Err(); err != nil {
		queryRows.Close()
		return false, fmt.Errorf("translate issue_history: iterate rows: %w", err)
	}
	queryRows.Close()
	if len(pending) == 0 {
		return false, nil
	}
	insertEvent, err := tx.PrepareContext(ctx, `INSERT INTO issue_events (id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return false, fmt.Errorf("translate issue_history: prepare event insert: %w", err)
	}
	defer insertEvent.Close()
	insertChange, err := tx.PrepareContext(ctx, `INSERT INTO issue_event_changes (event_id, field, from_value, to_value) VALUES (?, 'status', ?, ?)`)
	if err != nil {
		return false, fmt.Errorf("translate issue_history: prepare change insert: %w", err)
	}
	defer insertChange.Close()
	for _, t := range pending {
		actionArg := canonicalEventAction(t.action)
		reasonArg := canonicalEventReason(t.reason)
		actorArg := canonicalEventActor(t.createdBy)
		if _, err := insertEvent.ExecContext(ctx, t.id, t.issueID, actionArg, reasonArg, actorArg, t.createdAt); err != nil {
			return false, fmt.Errorf("translate issue_history: insert event %s: %w", t.id, err)
		}
		// Normalize legacy status spellings BEFORE the transition check —
		// a 'in-progress' → 'in_progress' raw pair both normalize to
		// 'in_progress', which is correctly NOT a transition. The change
		// row's from_value/to_value carry the canonical normalized values
		// so the event log matches issues.status post-ensureUnifiedStatusSchema.
		fromCanon := canonicalLegacyStatus(t.fromStatus)
		toCanon := canonicalLegacyStatus(t.toStatus)
		if isLegacyStatusTransition(fromCanon, toCanon) {
			if _, err := insertChange.ExecContext(ctx, t.id, nullableSQLString(fromCanon), nullableSQLString(toCanon)); err != nil {
				return false, fmt.Errorf("translate issue_history: insert status change for %s: %w", t.id, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("translate issue_history: commit tx: %w", err)
	}
	return true, nil
}

// canonicalEventAction mirrors recordEvent's action canonicalization:
// TrimSpace, then empty (or NULL input) becomes SQL NULL. Returns
// driver-friendly any. [LAW:one-source-of-truth] — same Go function
// shape as recordEvent so translated rows are byte-equivalent to
// live-written rows for the action column.
func canonicalEventAction(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	trimmed := strings.TrimSpace(v.String)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

// canonicalEventReason mirrors recordEvent's reason canonicalization:
// TrimSpace, with NULL input coerced to "" (the column is NOT NULL).
func canonicalEventReason(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return strings.TrimSpace(v.String)
}

// canonicalEventActor mirrors recordEvent's actor canonicalization:
// TrimSpace, then empty (or NULL input) becomes 'unknown' (the
// column is NOT NULL and 'unknown' is the canonical fallback).
func canonicalEventActor(v sql.NullString) string {
	if !v.Valid {
		return "unknown"
	}
	trimmed := strings.TrimSpace(v.String)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

// canonicalLegacyStatus mirrors ensureUnifiedStatusSchema's status
// normalization rules so the change-row from_value/to_value strings
// match the values the issues table carries after reconcile. Without
// this, a legacy 'todo'/'in-progress'/'done' status would be normalized
// in issues.status but copied verbatim into issue_event_changes —
// breaking [LAW:one-source-of-truth] for what "status" means in the
// canonical workspace.
//
// Mapping (exact-match, mirrors the UPDATE predicates in
// ensureUnifiedStatusSchema):
//   - 'in-progress' → 'in_progress'
//   - 'todo'        → 'open'
//   - 'done'        → 'closed'
//   - 'open' / 'in_progress' / 'closed' → unchanged
//   - anything else → 'open' (matches the "normalize invalid status"
//     fallback)
//   - NULL → NULL
func canonicalLegacyStatus(v sql.NullString) sql.NullString {
	if !v.Valid {
		return v
	}
	switch v.String {
	case "open", "in_progress", "closed":
		return v
	case "in-progress":
		return sql.NullString{Valid: true, String: "in_progress"}
	case "todo":
		return sql.NullString{Valid: true, String: "open"}
	case "done":
		return sql.NullString{Valid: true, String: "closed"}
	default:
		return sql.NullString{Valid: true, String: "open"}
	}
}

// isLegacyStatusTransition reports whether a (from, to) status pair
// describes a real workflow movement worth recording as a change row.
// NULL→value and value→NULL both count; value→same-value and
// NULL→NULL do not. [LAW:types-are-the-program] The status-change
// shape is "the value moved" — the predicate makes that exact shape
// the only thing the change-row INSERT can emit.
func isLegacyStatusTransition(from, to sql.NullString) bool {
	if !from.Valid && !to.Valid {
		return false
	}
	if from.Valid && to.Valid && from.String == to.String {
		return false
	}
	return true
}

// nullableSQLString converts a sql.NullString to a driver-friendly any:
// invalid → nil (writes SQL NULL), valid → the underlying string.
func nullableSQLString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
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
			probe:   fmt.Sprintf(`SELECT 1 FROM issues WHERE %s AND status IS NOT NULL LIMIT 1`, containerTypeMembership),
			stmt:    fmt.Sprintf(`UPDATE issues SET status = NULL WHERE %s AND status IS NOT NULL`, containerTypeMembership),
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
	// [LAW:no-silent-failure] The rank backfill is all-or-nothing: a
	// mid-loop failure (context cancel, transient DB error, partial
	// commit) would otherwise leave the first N rows with ranks
	// r1..rN, and the next Open would re-query the unranked set and
	// start over from rank.Initial() — colliding with the existing
	// r1..rN and breaking the fractional-indexing invariant. BeginTx
	// + Commit-or-Rollback ensures the next Open sees either all
	// rows ranked or none of them; the loop can safely restart.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("ensureIssueRanks: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// [LAW:no-silent-failure] Seed `current` from the max existing
	// non-empty rank (or rank.Initial() if there are no ranked rows
	// yet), so the assigned sequence cannot collide with any rank
	// already in the workspace. The mixed-state case — some rows
	// ranked, others empty — could otherwise produce duplicate ranks
	// that break the strict-ordering invariant rank mutations rely on.
	var maxExistingRank sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(item_rank) FROM issues WHERE item_rank != ''`,
	).Scan(&maxExistingRank); err != nil {
		return false, fmt.Errorf("ensureIssueRanks: read max existing rank: %w", err)
	}
	current := rank.Initial()
	if maxExistingRank.Valid && maxExistingRank.String != "" {
		current = rank.After(maxExistingRank.String)
	}
	// [LAW:single-enforcer] Each unranked row needs a distinct,
	// sequential rank string, so a single-statement UPDATE cannot
	// replace the loop. A prepared statement amortizes the parse cost
	// so workspaces with thousands of pre-rank rows finish the
	// one-time backfill in seconds rather than minutes. PrepareContext
	// on the *tx* binds the statement to this transaction, so the
	// rollback path reverts every prepared exec.
	stmt, err := tx.PrepareContext(ctx, "UPDATE issues SET item_rank = ? WHERE id = ?")
	if err != nil {
		return false, fmt.Errorf("ensureIssueRanks: prepare: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, current, id); err != nil {
			return false, fmt.Errorf("ensureIssueRanks: update %s: %w", id, err)
		}
		current = rank.After(current)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("ensureIssueRanks: commit tx: %w", err)
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
