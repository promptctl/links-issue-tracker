package store

import (
	"context"
	"database/sql"
	"fmt"
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
// [LAW:no-silent-failure] Old workspaces reach v1 by forward migration,
// not by being told to recreate themselves.
// [LAW:dataflow-not-control-flow] The reconcile is idempotent and probe-
// driven; every test exercises the same Open path with different
// initial workspace shapes.

// hijackToPreGoose simulates a pre-goose workspace by reverting the schema to
// the baseline shape and then dropping the goose_db_version table. The caller's
// own mutations produce the historical *baseline-or-earlier* shape they want to
// test; hijackToPreGoose strips post-baseline migrations and the goose history
// so the next Open hits the adoption path.
//
// A real pre-goose workspace predates every migration, so it cannot carry any
// post-baseline column (lane, and whatever later migrations add). Reverting via
// the migrations' own Down sections — the single source of "how to reach
// baseline" — keeps this helper correct as migrations accrue with no
// per-migration edits here. [LAW:one-source-of-truth]
func hijackToPreGoose(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	revertToBaseline(t, st)
	if err := st.ExecRawForTest(ctx, `DROP TABLE goose_db_version`); err != nil {
		t.Fatalf("drop goose_db_version error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "test: simulate pre-goose workspace"); err != nil {
		t.Fatalf("commit pre-goose simulation error = %v", err)
	}
}

// revertToBaseline rolls the live schema back to the baseline shape by running
// the post-baseline migrations' own Down sections — the single source of "how
// to reach baseline" — so pre-goose simulation stays correct as migrations
// accrue, with no per-migration edits. [LAW:one-source-of-truth]
//
// The Down migrations read the intact head schema (they touch issues, and
// 00004's Down also writes relations), so callers simulating a malformed
// legacy shape must mutilate tables AFTER reverting, not before. A caller
// that has already dropped issues is simulating a below-baseline workspace
// (reconcile recreates every table at baseline), so there is nothing to
// revert — running the Down would fail on the missing table. Revert only when
// the schema it targets is present; that is a precondition of the operation,
// not a swallowed failure.
func revertToBaseline(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	issuesPresent, err := st.tableExists(ctx, "issues")
	if err != nil {
		t.Fatalf("tableExists(issues) error = %v", err)
	}
	if !issuesPresent {
		return
	}
	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("construct goose provider error = %v", err)
	}
	if _, err := provider.DownTo(ctx, baselineVersion); err != nil {
		t.Fatalf("revert to baseline shape error = %v", err)
	}
}

// assertReachedBaseline opens st and asserts the post-reconcile invariants:
// shape converges to baseline, goose forward-migrates to HEAD.
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
	// Open reconciles to baseline, adopts, then forward-migrates to HEAD.
	if v != headVersion(t) {
		t.Fatalf("goose version = %d, want %d", v, headVersion(t))
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

// TestReconcileDropsLegacyIssueHistory pins the partial-shape contract:
// when issue_history is present but does NOT carry the canonical legacy
// column set (action, reason, from_status, to_status, created_at,
// created_by), the translation step no-ops and the drop step removes
// the table without losing audit data (there is no canonical audit
// data in a partial shape — the columns that would carry it are absent).
// The seeded issue must survive.
//
// Real-world workspaces with a canonical-shape issue_history get their
// rows translated by TestReconcileTranslatesLegacyIssueHistoryToEvents
// below; this test guards the synthetic/partial fallback path.
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

// TestIsLegacyStatusTransition pins all four (from, to) discriminations
// the predicate distinguishes. Without exhaustive coverage a regression
// could silently flip the semantics in either direction — same-value
// transitions starting to emit change rows, or NULL transitions getting
// dropped.
//
// [LAW:types-are-the-program] The predicate is a function from
// (Nullable × Nullable) → bool. Truth-table coverage is the only
// proof its branches do what their names say.
func TestIsLegacyStatusTransition(t *testing.T) {
	null := sql.NullString{}
	open := sql.NullString{Valid: true, String: "open"}
	openAlso := sql.NullString{Valid: true, String: "open"}
	closed := sql.NullString{Valid: true, String: "closed"}
	cases := []struct {
		name     string
		from, to sql.NullString
		want     bool
	}{
		{name: "null to null is not a transition", from: null, to: null, want: false},
		{name: "value to same value is not a transition", from: open, to: openAlso, want: false},
		{name: "null to value is a transition", from: null, to: open, want: true},
		{name: "value to null is a transition", from: open, to: null, want: true},
		{name: "value to different value is a transition", from: open, to: closed, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isLegacyStatusTransition(c.from, c.to)
			if got != c.want {
				t.Errorf("isLegacyStatusTransition(%+v, %+v) = %v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

// TestCanonicalEventCanonicalization pins that translateIssueHistoryToEvents
// applies the literal-same canonicalization rules recordEvent uses, so
// translated event rows are byte-equivalent to live-written rows.
// [LAW:one-source-of-truth] — both writers produce the same shape.
func TestCanonicalEventCanonicalization(t *testing.T) {
	null := sql.NullString{}
	t.Run("action: null → SQL NULL", func(t *testing.T) {
		if got := canonicalEventAction(null); got != nil {
			t.Errorf("canonicalEventAction(NULL) = %v, want nil", got)
		}
	})
	t.Run("action: whitespace-only → SQL NULL", func(t *testing.T) {
		if got := canonicalEventAction(sql.NullString{Valid: true, String: "   "}); got != nil {
			t.Errorf("canonicalEventAction(\"   \") = %v, want nil", got)
		}
	})
	t.Run("action: padded value → trimmed string", func(t *testing.T) {
		if got := canonicalEventAction(sql.NullString{Valid: true, String: "  start  "}); got != "start" {
			t.Errorf("canonicalEventAction(\"  start  \") = %v, want %q", got, "start")
		}
	})
	t.Run("reason: null → empty string", func(t *testing.T) {
		if got := canonicalEventReason(null); got != "" {
			t.Errorf("canonicalEventReason(NULL) = %q, want %q", got, "")
		}
	})
	t.Run("reason: padded value → trimmed string", func(t *testing.T) {
		if got := canonicalEventReason(sql.NullString{Valid: true, String: "  began work  "}); got != "began work" {
			t.Errorf("canonicalEventReason(\"  began work  \") = %q, want %q", got, "began work")
		}
	})
	t.Run("actor: null → unknown", func(t *testing.T) {
		if got := canonicalEventActor(null); got != "unknown" {
			t.Errorf("canonicalEventActor(NULL) = %q, want %q", got, "unknown")
		}
	})
	t.Run("actor: whitespace-only → unknown", func(t *testing.T) {
		if got := canonicalEventActor(sql.NullString{Valid: true, String: "   "}); got != "unknown" {
			t.Errorf("canonicalEventActor(\"   \") = %q, want %q", got, "unknown")
		}
	})
	t.Run("actor: padded value → trimmed string", func(t *testing.T) {
		if got := canonicalEventActor(sql.NullString{Valid: true, String: "  alice  "}); got != "alice" {
			t.Errorf("canonicalEventActor(\"  alice  \") = %q, want %q", got, "alice")
		}
	})
}

// TestCanonicalLegacyStatus pins that the translation's status
// normalization mirrors ensureUnifiedStatusSchema's UPDATE rules
// exactly. Drift between these two writers would land non-canonical
// status spellings in issue_event_changes while issues.status was
// already normalized — a [LAW:one-source-of-truth] violation.
func TestCanonicalLegacyStatus(t *testing.T) {
	cases := []struct {
		name string
		in   sql.NullString
		want sql.NullString
	}{
		{name: "null stays null", in: sql.NullString{}, want: sql.NullString{}},
		{name: "open passes through", in: sql.NullString{Valid: true, String: "open"}, want: sql.NullString{Valid: true, String: "open"}},
		{name: "in_progress passes through", in: sql.NullString{Valid: true, String: "in_progress"}, want: sql.NullString{Valid: true, String: "in_progress"}},
		{name: "closed passes through", in: sql.NullString{Valid: true, String: "closed"}, want: sql.NullString{Valid: true, String: "closed"}},
		{name: "in-progress normalized to in_progress", in: sql.NullString{Valid: true, String: "in-progress"}, want: sql.NullString{Valid: true, String: "in_progress"}},
		{name: "todo normalized to open", in: sql.NullString{Valid: true, String: "todo"}, want: sql.NullString{Valid: true, String: "open"}},
		{name: "done normalized to closed", in: sql.NullString{Valid: true, String: "done"}, want: sql.NullString{Valid: true, String: "closed"}},
		{name: "unknown value falls back to open", in: sql.NullString{Valid: true, String: "weird"}, want: sql.NullString{Valid: true, String: "open"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := canonicalLegacyStatus(c.in)
			if got != c.want {
				t.Errorf("canonicalLegacyStatus(%+v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// seedCanonicalIssueHistory creates the canonical-shape legacy
// issue_history table — the exact column set the deleted insertHistoryTx
// wrote in production. Tests use this to exercise the translation
// (translate-then-drop) path of the reconcile.
func seedCanonicalIssueHistory(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.ExecRawForTest(ctx, `CREATE TABLE issue_history (
		id VARCHAR(191) PRIMARY KEY,
		issue_id VARCHAR(191) NOT NULL,
		action VARCHAR(64) NULL,
		reason TEXT NULL,
		from_status VARCHAR(32) NULL,
		to_status VARCHAR(32) NULL,
		created_at VARCHAR(64) NOT NULL,
		created_by TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create canonical issue_history error = %v", err)
	}
}

// insertLegacyHistory writes one row into the canonical-shape
// issue_history fixture. All nullable columns (action, from_status,
// to_status) accept *string so tests can distinguish NULL (nil)
// from an explicit empty string (strPtr("")) — historically both
// shapes appear in real workspaces and both must normalize to NULL
// in issue_events.action.
func insertLegacyHistory(t *testing.T, st *Store, id, issueID string, action *string, reason string, fromStatus, toStatus *string, createdAt, createdBy string) {
	t.Helper()
	ctx := context.Background()
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO issue_history (id, issue_id, action, reason, from_status, to_status, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, issueID, nullableStrPtr(action), reason, nullableStrPtr(fromStatus), nullableStrPtr(toStatus), createdAt, createdBy,
	); err != nil {
		t.Fatalf("insert legacy history %s error = %v", id, err)
	}
}

func strPtr(s string) *string { return &s }

// nullableStrPtr converts a *string into a driver-friendly any: nil
// → SQL NULL, non-nil → the pointed-to string (including the empty
// string).
func nullableStrPtr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestReconcileTranslatesLegacyIssueHistoryToEvents pins the headline
// contract of links-recovery-icqp.3: every row in a canonical-shape
// issue_history table is preserved as an issue_events row (+ one
// issue_event_changes row per non-trivial status transition) before
// the legacy table is dropped. The previous bridge silently destroyed
// these rows; PR #143's recovery on unreal-3d-maps lost 184 of them
// and PR #145's on cc-nerf-buster lost 4.
//
// [LAW:no-silent-failure] Drop-without-translate was the silent
// fallback this ticket eliminates.
func TestReconcileTranslatesLegacyIssueHistoryToEvents(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Has history", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	seedCanonicalIssueHistory(t, first)
	// Six legacy rows exercise the canonical mappings:
	//   - hist-start:         status transition, named action
	//   - hist-comment-null:  NULL action (the explicit-NULL no-action shape)
	//   - hist-comment-empty: explicit empty-string action (the other no-action
	//     shape historically written by insertHistoryTx; MUST normalize to NULL
	//     post-translation to match recordEvent's convention)
	//   - hist-close:         status transition, named action, different actor
	//   - hist-same-status:   from_status == to_status — must NOT emit a
	//     change row (isLegacyStatusTransition value→same-value branch)
	//   - hist-whitespace:    padded action/reason/created_by values — MUST
	//     normalize via TrimSpace to byte-equivalence with recordEvent's
	//     live-write canonicalization [LAW:one-source-of-truth]
	insertLegacyHistory(t, first, "hist-start", seeded.ID, strPtr("start"), "began work", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:00:00Z", "alice")
	insertLegacyHistory(t, first, "hist-comment-null", seeded.ID, nil, "added context", nil, nil, "2026-01-01T10:05:00Z", "alice")
	insertLegacyHistory(t, first, "hist-comment-empty", seeded.ID, strPtr(""), "added more context", nil, nil, "2026-01-01T10:06:00Z", "alice")
	insertLegacyHistory(t, first, "hist-close", seeded.ID, strPtr("close"), "shipped", strPtr("in_progress"), strPtr("closed"), "2026-01-01T11:00:00Z", "bob")
	insertLegacyHistory(t, first, "hist-same-status", seeded.ID, strPtr("touch"), "no movement", strPtr("closed"), strPtr("closed"), "2026-01-01T11:30:00Z", "bob")
	insertLegacyHistory(t, first, "hist-whitespace", seeded.ID, strPtr("  start  "), "  padded reason  ", nil, nil, "2026-01-01T12:00:00Z", "  carol  ")
	// Drop the issues.status check constraint so we can write legacy
	// status spellings into issue_history's referenced workspace state
	// without violating the canonical CHECK. (issue_history itself has
	// no status check — the constraint lives on the issues table.)
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues DROP CHECK issues_status_check"); err != nil {
		_ = first.Close()
		t.Fatalf("drop status check error = %v", err)
	}
	// Legacy-status transition: ('todo' → 'done') must normalize to
	// ('open' → 'closed') in the canonical event log.
	insertLegacyHistory(t, first, "hist-legacy-transition", seeded.ID, strPtr("close"), "legacy close", strPtr("todo"), strPtr("done"), "2026-01-01T12:30:00Z", "carol")
	// Legacy-status non-transition: ('in-progress' → 'in_progress') both
	// normalize to 'in_progress' — must produce NO change row even though
	// the raw values differ.
	insertLegacyHistory(t, first, "hist-legacy-nontransition", seeded.ID, strPtr("touch"), "spelling differs only", strPtr("in-progress"), strPtr("in_progress"), "2026-01-01T12:45:00Z", "carol")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// issue_history is gone.
	exists, err := st.tableExists(ctx, "issue_history")
	if err != nil {
		t.Fatalf("tableExists(issue_history) error = %v", err)
	}
	if exists {
		t.Fatal("issue_history table still exists after reconcile")
	}
	// All eight rows show up in issue_events with the canonical mapping.
	type translatedEvent struct {
		ID        string
		IssueID   string
		Action    sql.NullString
		Reason    string
		Actor     string
		CreatedAt string
	}
	rows, err := st.db.QueryContext(ctx,
		`SELECT id, issue_id, action, reason, actor, created_at FROM issue_events WHERE id IN (?, ?, ?, ?, ?, ?, ?, ?) ORDER BY id ASC`,
		"hist-close", "hist-comment-empty", "hist-comment-null", "hist-legacy-nontransition", "hist-legacy-transition", "hist-same-status", "hist-start", "hist-whitespace",
	)
	if err != nil {
		t.Fatalf("query translated events error = %v", err)
	}
	defer rows.Close()
	got := map[string]translatedEvent{}
	for rows.Next() {
		var e translatedEvent
		if err := rows.Scan(&e.ID, &e.IssueID, &e.Action, &e.Reason, &e.Actor, &e.CreatedAt); err != nil {
			t.Fatalf("scan translated event error = %v", err)
		}
		got[e.ID] = e
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate translated events error = %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("expected 8 translated events, got %d: %+v", len(got), got)
	}
	cases := []struct {
		id, action, reason, actor string
		actionIsNull              bool
	}{
		{id: "hist-start", action: "start", reason: "began work", actor: "alice"},
		{id: "hist-comment-null", reason: "added context", actor: "alice", actionIsNull: true},
		{id: "hist-comment-empty", reason: "added more context", actor: "alice", actionIsNull: true},
		{id: "hist-close", action: "close", reason: "shipped", actor: "bob"},
		{id: "hist-same-status", action: "touch", reason: "no movement", actor: "bob"},
		// hist-whitespace asserts TrimSpace parity with recordEvent: the
		// padded "  start  " action / "  padded reason  " / "  carol  "
		// actor must come out trimmed.
		{id: "hist-whitespace", action: "start", reason: "padded reason", actor: "carol"},
		{id: "hist-legacy-transition", action: "close", reason: "legacy close", actor: "carol"},
		{id: "hist-legacy-nontransition", action: "touch", reason: "spelling differs only", actor: "carol"},
	}
	for _, c := range cases {
		e, ok := got[c.id]
		if !ok {
			t.Errorf("event %q missing from translation", c.id)
			continue
		}
		if c.actionIsNull && e.Action.Valid {
			t.Errorf("event %q action = %q, want NULL (empty/NULL action must normalize)", c.id, e.Action.String)
		}
		if !c.actionIsNull {
			if !e.Action.Valid || e.Action.String != c.action {
				t.Errorf("event %q action = %v, want %q", c.id, e.Action, c.action)
			}
		}
		if e.Reason != c.reason {
			t.Errorf("event %q reason = %q, want %q", c.id, e.Reason, c.reason)
		}
		if e.Actor != c.actor {
			t.Errorf("event %q actor = %q, want %q (created_by must map to actor)", c.id, e.Actor, c.actor)
		}
		if e.IssueID != seeded.ID {
			t.Errorf("event %q issue_id = %q, want %q", c.id, e.IssueID, seeded.ID)
		}
	}
	// issue_event_changes has one row per status transition; rows without
	// a real transition (comment-only, same-status, whitespace,
	// legacy-spelling-only-difference) must have zero change rows.
	changeRows, err := st.db.QueryContext(ctx,
		`SELECT event_id, field, from_value, to_value FROM issue_event_changes WHERE event_id IN (?, ?, ?, ?, ?, ?, ?, ?) ORDER BY event_id ASC`,
		"hist-close", "hist-comment-empty", "hist-comment-null", "hist-legacy-nontransition", "hist-legacy-transition", "hist-same-status", "hist-start", "hist-whitespace",
	)
	if err != nil {
		t.Fatalf("query translated changes error = %v", err)
	}
	defer changeRows.Close()
	type change struct {
		Field, From, To string
	}
	changes := map[string]change{}
	for changeRows.Next() {
		var eventID, field string
		var from, to sql.NullString
		if err := changeRows.Scan(&eventID, &field, &from, &to); err != nil {
			t.Fatalf("scan translated change error = %v", err)
		}
		changes[eventID] = change{Field: field, From: from.String, To: to.String}
	}
	if err := changeRows.Err(); err != nil {
		t.Fatalf("iterate translated changes error = %v", err)
	}
	for _, id := range []string{"hist-comment-null", "hist-comment-empty", "hist-same-status", "hist-whitespace", "hist-legacy-nontransition"} {
		if _, ok := changes[id]; ok {
			t.Errorf("non-transition row %q produced a change record; only real status transitions should emit changes", id)
		}
	}
	wantChanges := map[string]change{
		"hist-start": {Field: "status", From: "open", To: "in_progress"},
		"hist-close": {Field: "status", From: "in_progress", To: "closed"},
		// hist-legacy-transition: raw legacy 'todo'→'done' must normalize
		// to 'open'→'closed' in the change row (matching the rules
		// ensureUnifiedStatusSchema applies to issues.status).
		"hist-legacy-transition": {Field: "status", From: "open", To: "closed"},
	}
	for id, want := range wantChanges {
		got, ok := changes[id]
		if !ok {
			t.Errorf("status change for %q missing from translation", id)
			continue
		}
		if got != want {
			t.Errorf("change for %q = %+v, want %+v", id, got, want)
		}
	}
}

// TestReconcileTranslateSkipsOrphanedHistoryRows pins that issue_history
// rows referencing a non-existent issue_id are silently skipped (the
// issue was deleted at some point between the history row being written
// and the bridge running). Without this filter the INSERT would fail
// the FK constraint on issue_events.issue_id and the whole reconcile
// would abort — turning a recoverable workspace into a refused one.
//
// [LAW:no-silent-failure] Orphan-skipping is the only safe behavior:
// the alternative is "refuse the bridge" which strands the workspace.
// The orphan rows are unrecoverable regardless — there is no issue to
// attach the audit row to. Logging the skipped IDs would be an
// improvement; the count of dropped rows is implicit in (legacy count -
// translated count) and surfaces via the existing reconcile logs.
func TestReconcileTranslateSkipsOrphanedHistoryRows(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Existing", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	seedCanonicalIssueHistory(t, first)
	insertLegacyHistory(t, first, "hist-real", seeded.ID, strPtr("start"), "", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:00:00Z", "alice")
	insertLegacyHistory(t, first, "hist-orphan", "does-not-exist", strPtr("start"), "", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:01:00Z", "alice")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Real row translated.
	var realCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events WHERE id = ?`, "hist-real").Scan(&realCount); err != nil {
		t.Fatalf("count translated real row error = %v", err)
	}
	if realCount != 1 {
		t.Fatalf("hist-real event count = %d, want 1 (translation dropped a real row)", realCount)
	}
	// Orphan row skipped.
	var orphanCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events WHERE id = ?`, "hist-orphan").Scan(&orphanCount); err != nil {
		t.Fatalf("count orphan event error = %v", err)
	}
	if orphanCount != 0 {
		t.Fatalf("hist-orphan event count = %d, want 0 (orphan must not violate FK)", orphanCount)
	}
}

// TestReconcileTranslateRunsAfterActorRename pins the ordering
// constraint Copilot caught on PR #147 review: workspaces whose
// issue_events table still carries the pre-rename `assignee` column
// must NOT cause the translation INSERT to fail with unknown-column-
// `actor`. The reconcile must run the assignee→actor rename BEFORE
// the translation, so the translation's INSERT INTO issue_events(...,
// actor, ...) targets a column that exists on every legacy shape.
//
// [LAW:dataflow-not-control-flow] The translate step sees the
// canonical column layout because it follows the rename in the
// reconcile sequence; the dataflow makes the precondition implicit.
func TestReconcileTranslateRunsAfterActorRename(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Has legacy events", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Reshape issue_events to the pre-rename layout (assignee instead
	// of actor). Drop event_changes first to satisfy the FK; recreate
	// it after. Both must be present going into reconcile or the
	// schema-list CREATE TABLE step will rebuild them with the
	// canonical shape and the ordering bug wouldn't surface.
	pre := []string{
		`DROP TABLE issue_event_changes`,
		`DROP TABLE issue_events`,
		`CREATE TABLE issue_events (
			id VARCHAR(191) PRIMARY KEY,
			issue_id VARCHAR(191) NOT NULL,
			action VARCHAR(64) NULL,
			reason TEXT NOT NULL,
			assignee TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE issue_event_changes (
			event_id VARCHAR(191) NOT NULL,
			field VARCHAR(64) NOT NULL,
			from_value TEXT NULL,
			to_value TEXT NULL,
			PRIMARY KEY (event_id, field),
			FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE
		)`,
	}
	for _, stmt := range pre {
		if err := first.ExecRawForTest(ctx, stmt); err != nil {
			_ = first.Close()
			t.Fatalf("reshape issue_events to legacy shape (%q) error = %v", stmt, err)
		}
	}
	seedCanonicalIssueHistory(t, first)
	insertLegacyHistory(t, first, "hist-pre-rename", seeded.ID, strPtr("start"), "began work", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:00:00Z", "alice")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	// Translation must have succeeded — actor column is in place and the
	// event landed.
	var actor string
	if err := st.db.QueryRowContext(ctx, `SELECT actor FROM issue_events WHERE id = ?`, "hist-pre-rename").Scan(&actor); err != nil {
		t.Fatalf("query translated event error = %v — translation failed because actor column did not exist when INSERT ran", err)
	}
	if actor != "alice" {
		t.Errorf("translated actor = %q, want %q", actor, "alice")
	}
}

// TestReconcileTranslateIsIdempotentWithExistingEvents pins that a
// translation re-run against a workspace where an issue_events row
// already carries an id matching an issue_history row leaves the
// existing row untouched AND does not invent a status-change row
// attached to it. The two-row fixture forces the translate function
// past its early-exit (pending > 0), so the per-row INSERTs actually
// fire — without this shape the LEFT-JOIN-only change-INSERT bug
// Copilot caught on PR #147 would not be exercised.
//
// [LAW:types-are-the-program] The uniqueness of issue_events.id is
// the type-level encoding of "have we translated this row already";
// the per-row INSERT pairing in translateIssueHistoryToEvents
// encodes "this change row belongs to the event I just inserted on
// this iteration" so the change row cannot attach to a pre-existing
// unrelated event.
func TestReconcileTranslateIsIdempotentWithExistingEvents(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Idem", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Pre-populate issue_events with a row matching the id of a legacy
	// row we're about to insert — the canonical state after a previous
	// translation that already ran. Re-translation must see "row already
	// exists" and skip.
	if err := first.ExecRawForTest(ctx,
		`INSERT INTO issue_events (id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"hist-already-translated", seeded.ID, "start", "already migrated", "alice", "2026-01-01T10:00:00Z",
	); err != nil {
		_ = first.Close()
		t.Fatalf("seed pre-existing event error = %v", err)
	}
	seedCanonicalIssueHistory(t, first)
	// The legacy row carries different action/reason values from the
	// pre-existing event AND a status transition that would (under the
	// old JOIN-only change-INSERT bug) attach to the pre-existing event.
	insertLegacyHistory(t, first, "hist-already-translated", seeded.ID, strPtr("DIFFERENT"), "different reason", strPtr("open"), strPtr("closed"), "2026-01-01T10:00:00Z", "DIFFERENT_ACTOR")
	// A second legacy row that is genuinely new — forces the translate
	// function past its "pending == 0" early-exit so the per-row INSERTs
	// actually run. Without this row the change-INSERT bug Copilot
	// caught on PR #147 would not be exercised.
	insertLegacyHistory(t, first, "hist-fresh", seeded.ID, strPtr("start"), "fresh row", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:30:00Z", "alice")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	st := assertReachedBaseline(t, doltRoot)

	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events WHERE id = ?`, "hist-already-translated").Scan(&count); err != nil {
		t.Fatalf("count event error = %v", err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1 (translation duplicated a pre-existing row)", count)
	}
	// Pre-existing row's values must have survived intact.
	var action, reason, actor sql.NullString
	if err := st.db.QueryRowContext(ctx,
		`SELECT action, reason, actor FROM issue_events WHERE id = ?`, "hist-already-translated",
	).Scan(&action, &reason, &actor); err != nil {
		t.Fatalf("read event values error = %v", err)
	}
	if action.String != "start" {
		t.Errorf("action = %q, want %q (pre-existing event was overwritten)", action.String, "start")
	}
	if reason.String != "already migrated" {
		t.Errorf("reason = %q, want %q (pre-existing event was overwritten)", reason.String, "already migrated")
	}
	if actor.String != "alice" {
		t.Errorf("actor = %q, want %q (pre-existing event was overwritten)", actor.String, "alice")
	}
	// No change row was created on the pre-existing event either — the
	// per-row INSERT pairing ensures change rows only attach to events
	// the same iteration just inserted.
	var changeCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_event_changes WHERE event_id = ?`, "hist-already-translated").Scan(&changeCount); err != nil {
		t.Fatalf("count change error = %v", err)
	}
	if changeCount != 0 {
		t.Fatalf("change count = %d, want 0 (translation invented a change row on a previously-translated event)", changeCount)
	}
	// The genuinely-new row landed with its own event and status change,
	// proving the change-INSERT did execute this run (i.e. the
	// pre-existing event was correctly excluded from change attachment,
	// not skipped because the whole change-INSERT was bypassed).
	var freshEventCount int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events WHERE id = ?`, "hist-fresh").Scan(&freshEventCount); err != nil {
		t.Fatalf("count fresh event error = %v", err)
	}
	if freshEventCount != 1 {
		t.Fatalf("fresh event count = %d, want 1 (translation skipped a new row)", freshEventCount)
	}
	var freshChange struct {
		Field, From, To string
	}
	var fromNull, toNull sql.NullString
	if err := st.db.QueryRowContext(ctx,
		`SELECT field, from_value, to_value FROM issue_event_changes WHERE event_id = ?`, "hist-fresh",
	).Scan(&freshChange.Field, &fromNull, &toNull); err != nil {
		t.Fatalf("read fresh change error = %v (change row missing — change-INSERT was bypassed entirely)", err)
	}
	freshChange.From = fromNull.String
	freshChange.To = toNull.String
	if freshChange != (struct{ Field, From, To string }{Field: "status", From: "open", To: "in_progress"}) {
		t.Errorf("fresh change = %+v, want {status open in_progress}", freshChange)
	}
}

// TestReconcileRecoversFromFabricatedGooseRows pins the contract for
// workspaces that an older buggy binary partially-upgraded: the legacy
// issue_history table is still present AND goose_db_version carries
// fabricated rows (rows inserted at one tstamp without the migrations
// actually running). Such workspaces were previously trapped in
// phaseManaged with an ahead-of-registry refusal, because the lying log
// claimed a v1+ shape the workspace never had.
//
// Fix shape: disk-truth classification. issue_history's presence routes
// the workspace through the legacy bridge regardless of goose
// bookkeeping; reconcile drops the lying log along with issue_history;
// adoption stamps a clean v1.
//
// [LAW:types-are-the-program] The accept-shape of "pre-goose workspace"
// is "has issue_history," not "lacks goose_db_version." A workspace
// that satisfies the first but violates the second is still pre-goose
// — the goose log was fabricated, not produced by migrations.
func TestReconcileRecoversFromFabricatedGooseRows(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Survives fabricated-goose recovery", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue error = %v", err)
	}
	// Revert post-baseline migrations to the genuine pre-goose (baseline) shape
	// while goose history is still intact, so adoption + forward-migration on
	// reopen do not collide with a post-baseline column the bootstrap left behind.
	revertToBaseline(t, first)
	// Reproduce the field shape: legacy issue_history table present
	// AND a goose_db_version log claiming a version beyond this binary's
	// registry (the "stamped ahead by a buggy older binary" pattern seen in
	// cc-nerf-buster).
	if err := first.ExecRawForTest(ctx, `CREATE TABLE issue_history (id VARCHAR(191) PRIMARY KEY, issue_id VARCHAR(191) NOT NULL)`); err != nil {
		_ = first.Close()
		t.Fatalf("create legacy issue_history error = %v", err)
	}
	if err := first.ExecRawForTest(ctx, `DELETE FROM goose_db_version`); err != nil {
		_ = first.Close()
		t.Fatalf("clear real goose row error = %v", err)
	}
	// Rows at one tstamp = the field-observed fabrication shape (a single INSERT
	// loop, not real goose apply traces). The last claims a version beyond the
	// registry max — the ahead-of-registry row recovery must wipe.
	aheadVersion := headVersion(t) + 1
	fabricated := []string{
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (0, 1, '2026-05-08 23:33:37')`,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (1, 1, '2026-05-08 23:33:37')`,
		fmt.Sprintf(`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (%d, 1, '2026-05-08 23:33:37')`, aheadVersion),
	}
	for _, stmt := range fabricated {
		if err := first.ExecRawForTest(ctx, stmt); err != nil {
			_ = first.Close()
			t.Fatalf("insert fabricated row error = %v", err)
		}
	}
	if err := first.commitWorkingSet(ctx, "test: fabricated goose rows + legacy issue_history"); err != nil {
		_ = first.Close()
		t.Fatalf("commit fabricated state error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	// Next Open must classify phaseAdopt via disk-truth (issue_history
	// present), reconcile (drops issue_history + fabricated goose log),
	// adopt at baseline, then forward-migrate to a clean HEAD stamp.
	st := assertReachedBaseline(t, doltRoot)

	// issue_history dropped.
	histExists, err := st.tableExists(ctx, "issue_history")
	if err != nil {
		t.Fatalf("tableExists(issue_history) error = %v", err)
	}
	if histExists {
		t.Fatal("issue_history not dropped during fabricated-goose recovery")
	}
	// goose_db_version has no rows claiming versions beyond HEAD. The fabricated
	// ahead-of-registry row (the kind that traps a workspace in an
	// ahead-of-registry refusal) must be gone; recovery re-stamps a clean log
	// whose max is HEAD.
	var aheadRows int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id > ?`,
		headVersion(t),
	).Scan(&aheadRows); err != nil {
		t.Fatalf("count goose rows error = %v", err)
	}
	if aheadRows != 0 {
		t.Fatalf("goose_db_version still has %d rows beyond HEAD; fabricated rows survived recovery", aheadRows)
	}
	// Seeded issue survives unchanged.
	got, err := st.GetIssue(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != "Survives fabricated-goose recovery" {
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
// [LAW:no-silent-failure] Generated ranks cannot duplicate any
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
// [LAW:no-silent-failure] After reconcile finishes, runMigration
// verifies the actual shape matches baseline; any remaining gap aborts
// with a structural error before the stamp lands.
func TestPostReconcileBaselineVerificationCatchesNonIssuesGaps(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	// Revert to the pre-goose shape FIRST — the revert replays the
	// post-baseline Down migrations against the intact head schema
	// (00004's Down names relations.created_by in its INSERT column
	// list, so the column must still exist) — then drop
	// relations.created_by from the baseline shape. The reconcile's
	// {target: "relations"} ddlStep probes table presence — sees
	// relations exists — and skips the CREATE TABLE. Adoption would
	// then stamp v1 on a workspace where relations is missing
	// created_by. The post-reconcile baseline check must catch this.
	// (Cannot drop relations.type — it's part of the PRIMARY KEY.)
	hijackToPreGoose(t, first)
	if err := first.ExecRawForTest(ctx, `ALTER TABLE relations DROP COLUMN created_by`); err != nil {
		_ = first.Close()
		t.Fatalf("drop relations.created_by error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "test: drop relations.created_by"); err != nil {
		_ = first.Close()
		t.Fatalf("commit dropped column error = %v", err)
	}
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

// The issue-type CHECK clauses are derived from the sealed model.IssueTypes
// vocabulary (links-recut-types-mweb.3). This pins the derivation to the exact
// literals reconcile has always installed: a byte-level change would make
// every existing workspace's normalized-clause probes miss, dropping and
// re-adding constraints on each Open. [LAW:one-source-of-truth] The literals
// below are the test's independent copy of the at-rest schema, not a second
// authority in production code.
func TestDerivedTypeCheckClausesMatchHistoricalLiterals(t *testing.T) {
	if want := `issue_type IN ('task','feature','bug','chore','epic')`; issueTypeCheckClause != want {
		t.Fatalf("issueTypeCheckClause = %q, want %q", issueTypeCheckClause, want)
	}
	if want := `issue_type IN ('epic')`; containerTypeMembership != want {
		t.Fatalf("containerTypeMembership = %q, want %q", containerTypeMembership, want)
	}
	want := `(issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))`
	if canonicalStatusCheckClause != want {
		t.Fatalf("canonicalStatusCheckClause = %q, want %q", canonicalStatusCheckClause, want)
	}
}
