package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Pre-goose reconcile data-survival tests.
//
// These tests pin the contract restored by reconcileToBaseline: every
// workspace at any historical canonical shape forward-migrates to v1
// with every row of user data intact. The reconcile was deleted in
// commit 254f86b; the deletion left pre-v1 workspaces refused with
// "restore from a snapshot or recreate" (i.e. destroy your data).
//
// Each test simulates a specific pre-goose shape by mutating a freshly-
// bootstrapped workspace (drop a column, rename a column, drop a table,
// insert legacy data values), then Opens again and asserts:
//
//   1. Open succeeds — no refusal.
//   2. The shape converges to v1 (verifyBaselineShape returns no gaps).
//   3. goose_db_version is stamped at v1.
//   4. Every seeded row of user data survives, unchanged where the
//      reconcile is a no-op on data and converted predictably where it
//      normalizes legacy values (status canonicalization, etc.).
//
// [LAW:no-silent-fallbacks] Old workspaces reach v1 by forward migration,
// not by being told to recreate themselves.
// [LAW:dataflow-not-control-flow] The reconcile is idempotent and probe-
// driven; every test exercises the same Open path with different
// initial workspace shapes.

// hijackToPreGoose simulates a pre-goose workspace by dropping the
// goose_db_version table after the given Store has bootstrapped. The
// caller's mutation produces the historical shape they want to test;
// hijackToPreGoose strips the goose history so the next Open hits the
// adoption path.
func hijackToPreGoose(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.ExecRawForTest(ctx, `DROP TABLE goose_db_version`); err != nil {
		t.Fatalf("drop goose_db_version error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "test: simulate pre-goose workspace"); err != nil {
		t.Fatalf("commit pre-goose simulation error = %v", err)
	}
}

// assertReachedBaseline opens st and asserts the post-reconcile invariants:
// shape converges, goose stamps v1.
func assertReachedBaseline(t *testing.T, doltRoot string) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v — reconcile must forward-migrate, not refuse", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, missing, err := st.verifyBaselineShape(ctx)
	if err != nil {
		t.Fatalf("verifyBaselineShape() error = %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("post-reconcile shape still missing: %v", missing)
	}
	v, err := st.recordedMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("recordedMigrationVersion() error = %v", err)
	}
	if v != baselineVersion {
		t.Fatalf("goose version = %d, want %d", v, baselineVersion)
	}
	return st
}

// TestReconcileAddsMissingIssueEventsTables pins the headline failure shape
// reported in the field: a pre-goose workspace missing issue_events and
// issue_event_changes tables AND missing the issues.agent_prompt column
// must forward-migrate to v1, not refuse. This is the exact shape the user
// hit when the deletion shipped.
func TestReconcileAddsMissingIssueEventsTables(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Seed real data so the migration's data-preservation contract is
	// observable.
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Real issue", Topic: "real", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Simulate the user's workspace shape: drop issue_events + change-log
	// tables, drop the agent_prompt column. (Drop in FK-aware order:
	// issue_event_changes before issue_events.)
	stmts := []string{
		`DROP TABLE issue_event_changes`,
		`DROP TABLE issue_events`,
		`ALTER TABLE issues DROP COLUMN agent_prompt`,
	}
	for _, stmt := range stmts {
		if err := first.ExecRawForTest(ctx, stmt); err != nil {
			_ = first.Close()
			t.Fatalf("simulate-pre-goose %q error = %v", stmt, err)
		}
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// The seeded row survived.
	issue, err := st.GetIssue(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetIssue(%s) error = %v — reconcile dropped the row", seeded.ID, err)
	}
	if issue.Title != "Real issue" {
		t.Fatalf("issue title corrupted: %q", issue.Title)
	}
}

// TestReconcileRenamesPromptToAgentPrompt pins the contract that workspaces
// predating the prompt→agent_prompt rename forward-migrate cleanly: the
// column is renamed (not dropped) so any pre-rename prompt values survive.
func TestReconcileRenamesPromptToAgentPrompt(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Has prompt", Topic: "prompt", IssueType: "task", Priority: 0, Prompt: "the historical prompt body"})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Pre-rename shape: rename agent_prompt back to `prompt`.
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues RENAME COLUMN agent_prompt TO `prompt`"); err != nil {
		_ = first.Close()
		t.Fatalf("rename to legacy `prompt` error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// The seeded prompt value survived the rename.
	issue, err := st.GetIssue(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if issue.Prompt != "the historical prompt body" {
		t.Fatalf("prompt value lost in rename: got %q", issue.Prompt)
	}
}

// TestReconcileNormalizesLegacyStatusValues pins the contract that legacy
// status strings ('in-progress', 'todo', 'done') are normalized to the
// canonical set ('in_progress', 'open', 'closed') AND the corresponding
// rows still exist after migration. Pre-rename data must not be dropped.
func TestReconcileNormalizesLegacyStatusValues(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Drop the status check so we can write legacy values. The reconcile
	// will reinstall the canonical CHECK after normalizing.
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues DROP CHECK issues_status_check"); err != nil {
		_ = first.Close()
		t.Fatalf("drop status check error = %v", err)
	}
	// Insert rows with legacy status values directly. We use raw SQL so the
	// values bypass the canonical Store validation.
	insert := func(id, status string) {
		if err := first.ExecRawForTest(ctx,
			`INSERT INTO issues (id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, created_at, updated_at, item_rank) VALUES (?, ?, '', NULL, ?, 0, 'task', 'misc', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?)`,
			id, id, status, "rank-"+id,
		); err != nil {
			_ = first.Close()
			t.Fatalf("insert legacy %s error = %v", status, err)
		}
	}
	insert("legacy-todo", "todo")
	insert("legacy-inprog", "in-progress")
	insert("legacy-done", "done")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Each legacy status maps to its canonical counterpart, and the row
	// itself survives. closed_at gets nulled for non-closed; do not assert it.
	cases := map[string]string{
		"legacy-todo":   "open",
		"legacy-inprog": "in_progress",
		"legacy-done":   "closed",
	}
	for id, want := range cases {
		issue, err := st.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("GetIssue(%s) error = %v — reconcile dropped a row during status normalization", id, err)
		}
		if string(issue.State()) != want {
			t.Errorf("issue %s state = %q, want %q (legacy not normalized)", id, issue.State(), want)
		}
	}
}

// TestReconcileNullsEpicStatus pins that an epic row's persisted status is
// NULLed during reconcile (containers derive state from children). The
// epic row itself must survive — only the status column is rewritten.
func TestReconcileNullsEpicStatus(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	epic, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "An epic", Topic: "container", IssueType: "epic", Priority: 1})
	if err != nil {
		_ = first.Close()
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	// Drop the constraint so we can write a non-NULL status onto the epic.
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues DROP CHECK issues_status_check"); err != nil {
		_ = first.Close()
		t.Fatalf("drop status check error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, "UPDATE issues SET status = 'open' WHERE id = ?", epic.ID); err != nil {
		_ = first.Close()
		t.Fatalf("set epic status error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// The reconcile's "null out container status" step writes NULL into the
	// database column. State() derives from the lifecycle, which may still
	// report "open" for an epic with no children — we check the column
	// directly to assert the reconcile's contract on disk.
	var statusIsNull bool
	if err := st.db.QueryRowContext(ctx,
		`SELECT status IS NULL FROM issues WHERE id = ?`, epic.ID,
	).Scan(&statusIsNull); err != nil {
		t.Fatalf("query epic status error = %v — reconcile dropped the epic row", err)
	}
	if !statusIsNull {
		t.Fatalf("epic status column is not NULL after reconcile — container status not nulled")
	}
	// Title survived.
	got, err := st.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue(epic) error = %v", err)
	}
	if got.Title != "An epic" {
		t.Fatalf("epic title corrupted by reconcile: %q", got.Title)
	}
}

// TestReconcileBackfillsTopicDefault pins that issues with empty topic
// fields are backfilled to 'misc' so the canonical topic invariant
// holds post-migration. Rows are not dropped.
func TestReconcileBackfillsTopicDefault(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// `topic VARCHAR(191) NOT NULL` admits empty strings — NOT NULL
	// only forbids NULL. Inserting topic='' directly produces the
	// "empty topic" state pre-goose workspaces actually carried.
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issues (id, title, description, status, priority, issue_type, topic, assignee, created_at, updated_at, item_rank) VALUES ('topic-empty', 'no topic', '', 'open', 0, 'task', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'r1')`,
	); err != nil {
		_ = first.Close()
		t.Fatalf("insert empty-topic row error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	got, err := st.GetIssue(ctx, "topic-empty")
	if err != nil {
		t.Fatalf("GetIssue() error = %v — reconcile dropped the row", err)
	}
	if got.Topic != "misc" {
		t.Fatalf("topic after reconcile = %q, want 'misc'", got.Topic)
	}
}

// TestReconcileResetsLegacyPriorities pins that workspaces with the legacy
// 0..4 priority range get all priorities reset to 0 (normal) and the
// canonical priority CHECK installed. Rows survive.
func TestReconcileResetsLegacyPriorities(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Replace the canonical priority CHECK with the legacy one so we can
	// insert priority=3 rows.
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues DROP CHECK issues_priority_check"); err != nil {
		_ = first.Close()
		t.Fatalf("drop priority check error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues ADD CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 4)"); err != nil {
		_ = first.Close()
		t.Fatalf("add legacy priority check error = %v", err)
	}
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issues (id, title, description, status, priority, issue_type, topic, assignee, created_at, updated_at, item_rank) VALUES ('p3-row', 'legacy P3', '', 'open', 3, 'task', 'misc', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'r1')`,
	); err != nil {
		_ = first.Close()
		t.Fatalf("insert legacy P3 error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	got, err := st.GetIssue(ctx, "p3-row")
	if err != nil {
		t.Fatalf("GetIssue() error = %v — reconcile dropped the row", err)
	}
	if got.Priority != 0 {
		t.Fatalf("priority after reconcile = %d, want 0 (normal)", got.Priority)
	}
}

// TestReconcileDropsLegacyIssueHistory pins that an old issue_history table
// (superseded by issue_events) is dropped, and the existing issues stay
// untouched. The deleted reconcile knew about this; the test pins it.
func TestReconcileDropsLegacyIssueHistory(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Survives history drop", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Add the legacy issue_history table that some real workspaces still carry.
	if err := first.ExecRawForTest(ctx, `CREATE TABLE issue_history (id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL)`); err != nil {
		_ = first.Close()
		t.Fatalf("create legacy issue_history error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// issue_history must be gone.
	exists, err := st.tableExists(ctx, "issue_history")
	if err != nil {
		t.Fatalf("tableExists(issue_history) error = %v", err)
	}
	if exists {
		t.Fatal("issue_history table still exists after reconcile")
	}
	// Seeded issue survives.
	got, err := st.GetIssue(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != "Survives history drop" {
		t.Fatalf("issue title corrupted: %q", got.Title)
	}
}

// TestReconcileIsIdempotent pins that running reconcile on a workspace
// already at v1 (the no-op case for every step) writes nothing and the
// adoption path proceeds normally. This guards against the reconcile
// somehow mutating in steady state.
func TestReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Bootstrap, then strip goose so the next Open re-runs adoption.
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Idempotent", Topic: "idem", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Reopen: workspace is already at v1 shape, so reconcile must no-op
	// on every step before adoption stamps.
	st := assertReachedBaseline(t, doltRoot)

	got, err := st.GetIssue(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != "Idempotent" {
		t.Fatalf("issue corrupted by no-op reconcile: %q", got.Title)
	}
}

// TestReconcileCreatedTablesMatchBaselineConstraintNames pins that a
// reconcile-built issues / relations table carries the deterministic
// CHECK constraint names defined in 00001_baseline.sql. If reconcile
// ever has to CREATE these tables (synthetic shape: meta-but-no-issues),
// the result must be byte-equivalent to a baseline-applied workspace —
// otherwise the schema-drift canary breaks and any future migration
// that references the constraint name fails.
//
// [LAW:one-source-of-truth] Both creators (goose applying baseline.sql,
// reconcile via createIssuesTableStmt) produce the same constraint
// names; this test pins them in lockstep so silent drift surfaces.
func TestReconcileCreatedTablesMatchBaselineConstraintNames(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	// Bootstrap to lay down storage and a `links` database, then strip
	// EVERY canonical table except meta. The reconcile must then CREATE
	// issues, relations, comments, labels, issue_events, issue_event_changes.
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// FK-aware drop order: dependents before parents.
	stmts := []string{
		`DROP TABLE issue_event_changes`,
		`DROP TABLE issue_events`,
		`DROP TABLE labels`,
		`DROP TABLE comments`,
		`DROP TABLE relations`,
		`DROP TABLE issues`,
	}
	for _, stmt := range stmts {
		if err := first.ExecRawForTest(ctx, stmt); err != nil {
			_ = first.Close()
			t.Fatalf("drop %q error = %v", stmt, err)
		}
	}
	// Force phaseAdopt: leave meta in place so verifyBaselineShape sees
	// >0 canonical tables present, then strip goose so adoption fires.
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Probe the actual constraint names reconcile installed. They must
	// match the names defined in 00001_baseline.sql.
	expected := []string{
		"issues_status_check",
		"issues_priority_check",
		"issues_type_check",
		"relations_type_check",
	}
	for _, name := range expected {
		var marker int
		err := st.db.QueryRowContext(ctx,
			`SELECT 1 FROM information_schema.table_constraints WHERE table_schema = DATABASE() AND constraint_name = ? AND constraint_type = 'CHECK' LIMIT 1`,
			name,
		).Scan(&marker)
		if err != nil {
			t.Errorf("constraint %q missing after reconcile-built create — drift canary will fail: %v", name, err)
		}
	}
}

// TestReconcileTopicHasNoDefault pins that after reconcile adds the
// topic column, the column has no default — matching baseline.sql.
// The ADD COLUMN needs a DEFAULT for the backfill to satisfy NOT NULL,
// but baseline.sql declares topic without a default; reconcile drops
// the default post-add so the post-reconcile shape is byte-equivalent
// to a baseline-applied workspace.
//
// [LAW:one-source-of-truth] Both creators (goose-baseline and
// reconcile) produce the same column shape, no defaults to drift on.
func TestReconcileTopicHasNoDefault(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Drop the topic column so reconcile must re-add it.
	if err := first.ExecRawForTest(ctx, `ALTER TABLE issues DROP COLUMN topic`); err != nil {
		_ = first.Close()
		t.Fatalf("drop topic error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Post-reconcile, the topic column must exist with NO column_default.
	var hasDefault bool
	if err := st.db.QueryRowContext(ctx,
		`SELECT column_default IS NOT NULL FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'topic'`,
	).Scan(&hasDefault); err != nil {
		t.Fatalf("query topic column_default error = %v", err)
	}
	if hasDefault {
		t.Fatal("reconcile left a DEFAULT on issues.topic; baseline declares it with no default — drift canary will fail")
	}
}

// TestReconcileRankBackfillCoexistsWithExistingRanks pins the mixed-
// state contract: if some issues are already ranked and others have
// item_rank = '', the rank backfill seeds from MAX(existing rank) so
// new ranks never collide. Without this seeding, ensureIssueRanks
// would assign rank.Initial() to the first unranked row, which would
// duplicate any existing rank.Initial() row and break the strict-
// ordering invariant rank mutations depend on.
//
// [LAW:no-silent-fallbacks] Generated ranks cannot duplicate any
// existing rank value in the workspace, even in mixed states.
func TestReconcileRankBackfillCoexistsWithExistingRanks(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Create one issue (gets the default initial rank), then insert a
	// second issue directly with item_rank = '' — simulating the
	// mixed state where ensureIssueRanks must coexist with existing
	// rank values. Capture the first issue's actual rank so we can
	// assert no duplication.
	ranked, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Already ranked", Topic: "rank", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed ranked CreateIssue error = %v", err)
	}
	var existingRank string
	if err := first.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, ranked.ID).Scan(&existingRank); err != nil {
		_ = first.Close()
		t.Fatalf("read existing rank error = %v", err)
	}
	if existingRank == "" {
		_ = first.Close()
		t.Fatalf("seeded ranked issue has empty item_rank — fixture invalid")
	}
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issues (id, title, description, status, priority, issue_type, topic, assignee, created_at, updated_at, item_rank) VALUES ('unranked-row', 'no rank', '', 'open', 0, 'task', 'misc', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '')`,
	); err != nil {
		_ = first.Close()
		t.Fatalf("insert unranked row error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Read both rows' ranks. They must be (a) both non-empty and
	// (b) distinct from each other.
	var rankedAfter, newAfter string
	if err := st.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, ranked.ID).Scan(&rankedAfter); err != nil {
		t.Fatalf("read ranked row error = %v", err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, "unranked-row").Scan(&newAfter); err != nil {
		t.Fatalf("read unranked row error = %v", err)
	}
	if newAfter == "" {
		t.Fatal("backfilled row still has empty item_rank")
	}
	if rankedAfter == newAfter {
		t.Fatalf("backfilled rank %q collides with existing rank %q — seeding from MAX is broken", newAfter, rankedAfter)
	}
}

// TestPostReconcileBaselineVerificationCatchesNonIssuesGaps pins the
// safety net that runs AFTER reconcileToBaseline and BEFORE adoption:
// reconcile's CREATE TABLE steps are gated on table presence (not
// column presence), so if a non-issues canonical table exists but is
// missing required columns, reconcile no-ops the CREATE and the
// malformed table persists. Without the post-reconcile baseline check,
// adoption would stamp v1 on a non-baseline schema — recreating the
// PR #119 failure shape adoption was supposed to prevent.
//
// [LAW:no-silent-fallbacks] After reconcile finishes, runMigration
// verifies the actual shape matches baseline; any remaining gap aborts
// with a structural error before the stamp lands.
func TestPostReconcileBaselineVerificationCatchesNonIssuesGaps(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Drop relations.created_by. The reconcile's {target: "relations"}
	// ddlStep probes table presence — sees relations exists — and skips
	// the CREATE TABLE. Adoption would then stamp v1 on a workspace
	// where relations is missing created_by. The post-reconcile
	// baseline check must catch this.
	// (Cannot drop relations.type — it's part of the PRIMARY KEY.)
	if err := first.ExecRawForTest(ctx, `ALTER TABLE relations DROP COLUMN created_by`); err != nil {
		_ = first.Close()
		t.Fatalf("drop relations.created_by error = %v", err)
	}
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	_, err = Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() stamped v1 on a workspace with a malformed relations table; the post-reconcile baseline check failed to catch the gap")
	}
	// The error must name the specific remaining gap so the operator
	// can act on it — not a vague "partial schema" message.
	if !strings.Contains(err.Error(), "relations.created_by") {
		t.Fatalf("error %q does not name the remaining relations.created_by gap after reconcile", err)
	}
	// And the workspace must NOT have been stamped at v1.
	st, err := openStoreConnection(doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("openStoreConnection() error = %v", err)
	}
	defer st.Close()
	exists, err := st.tableExists(ctx, gooseVersionTable)
	if err != nil {
		t.Fatalf("tableExists(goose_db_version) error = %v", err)
	}
	if exists {
		t.Fatal("goose_db_version was created despite the malformed relations table — the stamp must NOT land before the shape is canonical")
	}
}

// TestReconcileErrorMessageIsActionable pins the contract that the
// reconcile, when it cannot bring a shape forward, names the specific
// operation it failed on. The deleted code's failure message was
// "restore from a snapshot or recreate it" — destructive guidance. The
// replacement must point at the actual structural issue so the operator
// can fix it without data loss.
func TestReconcileErrorMessageIsActionable(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	if _, err := EnsureDatabase(ctx, doltRoot, "test-workspace-id"); err != nil {
		t.Fatalf("EnsureDatabase() error = %v", err)
	}
	seed, err := openStoreConnection(doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("openStoreConnection() error = %v", err)
	}
	// Maximally malformed issues table — no shape reconcile can repair.
	if _, err := seed.db.ExecContext(ctx, `CREATE TABLE issues (id VARCHAR(191) PRIMARY KEY)`); err != nil {
		_ = seed.Close()
		t.Fatalf("create malformed issues error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close() error = %v", err)
	}

	_, err = Open(ctx, doltRoot, "test-workspace-id")
	if err == nil {
		t.Fatal("Open() on a malformed shape returned nil; expected an actionable error")
	}
	// Must name the reconcile phase wrapping the structural refusal.
	if !strings.Contains(err.Error(), "reconcile pre-goose workspace") {
		t.Fatalf("error %q does not name the reconcile phase", err)
	}
	// Must name the specific missing prerequisite column(s) — the
	// upfront structural probe identifies the actual anomaly, not
	// a generic refusal.
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("error %q does not name the missing reconcile prerequisite", err)
	}
	if !strings.Contains(err.Error(), "not a known historical shape") {
		t.Fatalf("error %q does not classify the shape as unknown-history", err)
	}
	// Must NOT contain the destructive guidance the old code emitted.
	if strings.Contains(err.Error(), "restore it from a snapshot or recreate") {
		t.Fatalf("error still contains the data-destroying guidance from the deleted gate: %q", err)
	}
}
