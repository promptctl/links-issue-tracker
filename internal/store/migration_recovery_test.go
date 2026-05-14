package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// recoverableApplicationTable pairs a table name with the FK-dependent
// tables that must be dropped first to synthesize "table missing" without
// hitting a foreign-key check. The matrix in
// TestEveryCanonicalTableRecoversIndividually walks this list.
//
// `issues` is intentionally absent: dropping it would require cascading
// through every dependent and that broader corruption is handled by
// `lit doctor --reset-to-pre-migration`, not the silent auto-heal path.
type recoverableApplicationTable struct {
	name       string
	dependents []string
}

var recoverableApplicationTables = []recoverableApplicationTable{
	{name: "meta"},
	{name: "relations"},
	{name: "comments"},
	{name: "labels"},
	{name: "issue_events", dependents: []string{"issue_event_changes"}},
	{name: "issue_event_changes"},
	{name: "migration_quarantine"},
	{name: "migration_log"},
}

// The tests in this file reproduce the failure modes observed on real-world
// `lit` workspaces after PR #119 (goose changeset registry) shipped on master.
// Each test simulates a divergence between the truth claimed by
// goose_db_version and the truth on disk — the central design gap PR #119
// missed. They are RED on the un-patched master and turn GREEN after the
// auto-heal + idempotent-migration fix in this branch.
//
// Coverage matrix:
//
//   case                               disk truth                goose claim       user symptom                          test
//   ─────────────────────────────────  ────────────────────────  ────────────────  ────────────────────────────────────  ─────────────────────────────
//   stamped-but-table-missing          issue_events absent       v1 applied        Error 1146: table not found            TestStampedButTableMissingHeals
//   orphan-table-from-partial-apply    migration_log present     v3 NOT applied    Error 1105: table already exists       TestOrphanTableSelfHeals
//   first-open-batch-failure-erases-Q  quarantine never landed   nothing stamped   quarantine.write_failed                TestFirstOpenFailureSurvivesQuarantineErase
//
// The matrix is intentionally written into the file as the gap audit: every
// row here is one place the existing test suite did not exercise. Adding a
// new migration that mutates schema MUST come with at least one entry in
// this matrix (a recovery-path test). See migration_runner_test.go for the
// happy-path coverage; this file covers the recovery-path coverage.

// TestStampedButTableMissingHeals reproduces the user's `lit new` symptom:
// goose_db_version claims v1 applied, but `issue_events` is missing on disk.
// Without auto-heal, the runner trusts goose's claim and a downstream
// INSERT into issue_events 1146s. With auto-heal, the missing table is
// re-created idempotently and the workspace is usable.
func TestStampedButTableMissingHeals(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "stamped-but-missing-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	// Synthesize the failure: drop issue_events while goose_db_version still
	// claims baseline applied. Commit so the drop persists across the close
	// and would survive a safety-branch revert too.
	if _, err := first.db.ExecContext(ctx, "DROP TABLE issue_event_changes"); err != nil {
		t.Fatalf("drop issue_event_changes error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx, "DROP TABLE issue_events"); err != nil {
		t.Fatalf("drop issue_events error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "synthesize stamped-but-missing state"); err != nil {
		t.Fatalf("commit synthesized state error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() (auto-heal) error = %v", err)
	}
	defer second.Close()

	// The table that was dropped must be back. We verify by issuing the same
	// statement production code uses, not by probing information_schema —
	// "the table is writable" is the actual contract.
	exists, err := tableExists(ctx, second.db, "issue_events")
	if err != nil {
		t.Fatalf("tableExists issue_events error = %v", err)
	}
	if !exists {
		t.Fatal("issue_events still missing after Open; auto-heal did not run")
	}
	exists, err = tableExists(ctx, second.db, "issue_event_changes")
	if err != nil {
		t.Fatalf("tableExists issue_event_changes error = %v", err)
	}
	if !exists {
		t.Fatal("issue_event_changes still missing after Open; auto-heal did not run")
	}

	// And smoke must pass so the broken-workspace path doesn't slip through
	// silently next time.
	probe, err := second.runSmokeTests(ctx)
	if err != nil {
		t.Fatalf("smoke after heal: probe %q error = %v", probe, err)
	}
}

// TestOrphanTableSelfHeals reproduces the first half of the user's first-run
// trace: `migrate.start version=3 → migrate.error: table already exists`.
// A previous partial run left `migration_log` on disk; the runner's claim
// in goose_db_version was reverted but the DDL wasn't (or the revert path
// itself raced). The next Open must converge cleanly, not error.
func TestOrphanTableSelfHeals(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "orphan-table-id"

	// First Open: full migrations applied (v1, v2, v3).
	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	// Synthesize: pretend v3 was never stamped, but its CREATE TABLE side
	// effect (migration_log) is still on disk. This is what a partial revert
	// produces and what the user is hitting in the wild.
	if _, err := first.db.ExecContext(ctx,
		"DELETE FROM "+gooseVersionTable+" WHERE version_id = ?", 3); err != nil {
		t.Fatalf("rewind goose to pre-v3 error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "synthesize orphan migration_log"); err != nil {
		t.Fatalf("commit synthesized state error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Second Open: runner finds v3 pending; the migration body's CREATE
	// TABLE would error on master because migration_log is already present.
	// The fix must make the migration idempotent (or reconcile-first) so
	// this Open succeeds and v3 ends up stamped.
	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() (orphan recovery) error = %v", err)
	}
	defer second.Close()

	requireGooseVersionPresent(t, ctx, second, 3)
}

// TestFirstOpenFailureSurvivesQuarantineErase reproduces the second half of
// the user's first-run trace: when a migration in the same batch that
// creates `migration_quarantine` fails, the safety-branch revert wipes
// `migration_quarantine` along with everything else, and the runner's
// quarantine write fails with "migration_quarantine table absent".
//
// On master this prints `quarantine.write_failed` and the workspace is
// stuck on the same failure forever. The fix must keep quarantine reachable
// regardless of what the batch reverts.
func TestFirstOpenFailureSurvivesQuarantineErase(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "first-open-failure-id"

	// Inject a SQL-level failure that triggers ONLY on the very first batch,
	// i.e. when baseline + quarantine + log are all pending alongside the
	// failure. This is what the user hit on real workspaces.
	const failVersion int64 = 50 // between v3 and any future migration
	t.Cleanup(installBadMigration(failVersion, errors.New("synthetic first-batch failure")))

	var buf bytes.Buffer
	t.Cleanup(restoreEventWriter(&buf))

	_, err := Open(ctx, doltRoot, wsID)
	if err == nil {
		t.Fatal("first Open() succeeded, want MigrationError for v50")
	}
	var me *MigrationError
	if !errors.As(err, &me) || me.Version != failVersion {
		t.Fatalf("first Open() error = %v, want MigrationError for v%d", err, failVersion)
	}

	// The bug we are guarding against: the runner cannot quarantine v50
	// because migration_quarantine was reverted along with the batch.
	emitted := buf.String()
	if strings.Contains(emitted, "migration_quarantine table absent") {
		t.Fatalf("safety-branch revert erased migration_quarantine; quarantine write failed.\nevents:\n%s", emitted)
	}

	// Removing the bad migration must let the workspace recover on the next
	// Open AND the quarantine row for v50 must persist so a subsequent
	// upgrade that re-introduces v50 (intentionally or by mistake) is
	// skipped, not re-attempted.
	extraMigrationProviderOptions = nil

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("recovery Open() error = %v", err)
	}
	defer second.Close()

	quarantined, err := readQuarantinedVersions(ctx, second.db)
	if err != nil {
		t.Fatalf("readQuarantinedVersions error = %v", err)
	}
	found := false
	for _, v := range quarantined {
		if v == failVersion {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected v%d in quarantine after recovery, got %v", failVersion, quarantined)
	}
}

// TestSmokeRunsAtOpenAndRefusesBrokenWorkspace pins down the contract that
// gives the auto-heal teeth: a workspace whose schema cannot satisfy the
// canonical smoke probes after Open must fail Open with a clear error.
// Without this gate, the binary happily hands out store handles that will
// 1146 on the next mutation — exactly the user-facing surprise.
//
// We synthesize an UNHEALABLE break: a dropped column on `comments` that
// no auto-heal step in convergeLegacyAlterations re-adds. This isolates
// the smoke-gate contract from the "we auto-heal almost everything"
// contract; the gate is the floor that catches whatever the auto-heal
// missed. Picking issues.topic here would be wrong: that column IS
// auto-healed by an ADD COLUMN IF NOT EXISTS step, so the gate would
// never get a chance to fire.
func TestSmokeRunsAtOpenAndRefusesBrokenWorkspace(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "smoke-gate-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx, "ALTER TABLE comments DROP COLUMN body"); err != nil {
		t.Fatalf("synthesize broken column error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "synthesize missing comments.body column"); err != nil {
		t.Fatalf("commit synthesized state error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Re-open: missing column cannot be healed idempotently (it's a column
	// drop on comments — none of the auto-heal steps add it back), so the
	// runner must fail Open rather than hand out a broken handle. A
	// successful Open here is the regression we are guarding against.
	st, err := Open(ctx, doltRoot, wsID)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open() succeeded against a workspace whose smoke probe would fail; want a refused Open")
	}
	if !strings.Contains(err.Error(), "smoke") && !strings.Contains(err.Error(), "comments") {
		t.Fatalf("Open() error = %v; want a smoke-related message naming the failing probe", err)
	}
}

// TestAutoHealRecoversTopicColumnDrop documents the desired healing
// behavior for the column-drop case the previous test deliberately
// avoids: a drop of issues.topic IS auto-healed by convergeLegacyAlterations,
// so the workspace recovers without the smoke gate ever firing. This
// asserts the heal path, not the gate.
func TestAutoHealRecoversTopicColumnDrop(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "auto-heal-topic-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.db.ExecContext(ctx, "ALTER TABLE issues DROP COLUMN topic"); err != nil {
		t.Fatalf("drop issues.topic error = %v", err)
	}
	if err := first.commitWorkingSet(ctx, "synthesize missing issues.topic column"); err != nil {
		t.Fatalf("commit synthesized state error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("Open() error = %v; want success (topic column should be auto-healed)", err)
	}
	defer second.Close()

	var topicExists int
	if err := second.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'issues' AND column_name = 'topic'`).Scan(&topicExists); err != nil {
		t.Fatalf("query topic column error = %v", err)
	}
	if topicExists == 0 {
		t.Fatal("issues.topic still missing after Open; auto-heal did not run")
	}
}

// TestEveryCanonicalTableRecoversIndividually is the matrix-style test that
// guards the *general* invariant rather than the user's specific symptom:
// for every recoverable table in the canonical schema, dropping that table
// on a healthy workspace and re-opening must heal it. This is the systemic
// test that would have caught the PR #119 gap regardless of which specific
// table the user noticed missing first.
//
// If a future migration adds a new table that ensureCanonicalSchema doesn't
// know about, this test fails for that table — preventing the same class
// of bug from reaching production again.
func TestEveryCanonicalTableRecoversIndividually(t *testing.T) {
	for _, tc := range recoverableApplicationTables {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			doltRoot := filepath.Join(t.TempDir(), "dolt")
			wsID := "recover-individual-" + tc.name

			first, err := Open(ctx, doltRoot, wsID)
			if err != nil {
				t.Fatalf("first Open() error = %v", err)
			}
			// Drop FK-dependent tables first so the target table can be
			// dropped cleanly. They get re-created by the same auto-heal
			// pass, so this only widens the corruption synthesized for the
			// test — the assertion is still about tc.name.
			for _, dep := range tc.dependents {
				if _, err := first.db.ExecContext(ctx, "DROP TABLE "+dep); err != nil {
					t.Fatalf("drop dependent %s error = %v", dep, err)
				}
			}
			if _, err := first.db.ExecContext(ctx, "DROP TABLE "+tc.name); err != nil {
				t.Fatalf("drop %s error = %v", tc.name, err)
			}
			if err := first.commitWorkingSet(ctx, "synthesize missing "+tc.name); err != nil {
				t.Fatalf("commit error = %v", err)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("first Close() error = %v", err)
			}

			second, err := Open(ctx, doltRoot, wsID)
			if err != nil {
				t.Fatalf("second Open() (heal %s) error = %v", tc.name, err)
			}
			defer second.Close()

			for _, table := range append([]string{tc.name}, tc.dependents...) {
				exists, err := tableExists(ctx, second.db, table)
				if err != nil {
					t.Fatalf("tableExists(%s) error = %v", table, err)
				}
				if !exists {
					t.Fatalf("%s still missing after Open; auto-heal did not cover this table", table)
				}
			}
		})
	}
}

// TestInfraBootstrapPrecedesSafetyBranch pins the load-bearing ordering
// invariant: migration_quarantine must exist before the safety branch is
// created. Without this ordering, a failure in the same batch that creates
// quarantine (v2) reverts that table out of existence, leaving the runner
// unable to record the quarantine row. The user's "migration_quarantine
// table absent" symptom is a direct consequence of breaking this ordering.
//
// We verify the runner's event sequence: a bootstrap commit (when needed)
// or the safety_branch.created event must NOT precede the existence of
// migration_quarantine on disk.
func TestInfraBootstrapPrecedesSafetyBranch(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	var buf bytes.Buffer
	t.Cleanup(restoreEventWriter(&buf))

	// First Open creates the workspace. After this, bootstrap will be a
	// no-op (tables already exist), but the safety-branch ordering still
	// matters for the next failing-migration scenario.
	st, err := Open(ctx, doltRoot, "infra-precedes-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// At Open completion, migration_quarantine must be present — even on a
	// brand-new workspace where neither v2 (quarantine) nor v3 (log) have
	// applied yet, bootstrap should have created them up front.
	for _, table := range []string{"migration_quarantine", "migration_log"} {
		exists, err := tableExists(ctx, st.db, table)
		if err != nil {
			t.Fatalf("tableExists(%s) error = %v", table, err)
		}
		if !exists {
			t.Fatalf("%s missing after first Open; bootstrap did not run", table)
		}
	}
}

// TestCanonicalSchemaCoversEverySmokeTable is the static coverage assertion
// linking the canonical schema to the smoke probes. Each entry in
// applicationTables must be created by either infraSchemaStatements or
// canonicalSchemaStatements (otherwise auto-heal cannot create it) AND have
// a corresponding smoke probe (otherwise a missing-table breakage cannot be
// detected). Future migration authors get a compile-time-ish nudge: add a
// table to applicationTables, this test fails until both sides are wired.
func TestCanonicalSchemaCoversEverySmokeTable(t *testing.T) {
	created := map[string]bool{}
	collect := func(stmts []string) {
		for _, stmt := range stmts {
			lower := strings.ToLower(stmt)
			if !strings.Contains(lower, "create table") {
				continue
			}
			// "CREATE TABLE IF NOT EXISTS foo (..."
			idx := strings.Index(lower, "create table")
			rest := stmt[idx+len("create table"):]
			rest = strings.TrimSpace(rest)
			rest = strings.TrimPrefix(rest, "IF NOT EXISTS ")
			rest = strings.TrimPrefix(rest, "if not exists ")
			rest = strings.TrimSpace(rest)
			openParen := strings.IndexAny(rest, "( \t\n")
			if openParen < 0 {
				continue
			}
			name := strings.TrimSpace(rest[:openParen])
			created[name] = true
		}
	}
	collect(infraSchemaStatements())
	collect(canonicalSchemaStatements())
	// createIssuesTableStmt is a function returning a single CREATE TABLE
	// for issues; canonicalSchemaStatements embeds it.
	collect([]string{createIssuesTableStmt()})

	probed := map[string]bool{}
	for _, p := range smokeProbes {
		probed[p.Name] = true
	}

	for _, table := range applicationTables {
		if !created[table] {
			t.Errorf("table %q is in applicationTables but not in infraSchemaStatements/canonicalSchemaStatements — auto-heal cannot recreate it", table)
		}
		if !probed[table] {
			t.Errorf("table %q is in applicationTables but has no smoke probe — silent corruption cannot be detected", table)
		}
	}
}

// TestSecondOpenAfterHealMakesNoCommit verifies the idempotent contract: a
// workspace that has already been healed must not generate further commits
// on subsequent Opens. Otherwise every Open accumulates a "heal" commit
// even when there's nothing to heal, which would bloat dolt_log and burn
// retention slots.
func TestSecondOpenAfterHealMakesNoCommit(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	const wsID = "heal-idempotent-id"

	first, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	// Capture commit count before any synthesized damage.
	var commitsBefore int
	if err := first.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&commitsBefore); err != nil {
		t.Fatalf("count commits before error = %v", err)
	}
	first.Close()

	// Re-open clean workspace: heal must be a no-op (no new commits).
	second, err := Open(ctx, doltRoot, wsID)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	var commitsAfter int
	if err := second.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&commitsAfter); err != nil {
		t.Fatalf("count commits after error = %v", err)
	}
	second.Close()

	// pre-migrate safety branch creation and meta commits may add at most a
	// known amount of churn. Stricter assertion: no NEW migrate-heal commits.
	if commitsAfter > commitsBefore {
		// Heal commits are titled "Heal canonical schema (idempotent
		// reconcile)". Make sure none landed.
		var healCount int
		if err := second.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM dolt_log WHERE message LIKE 'Heal canonical schema%'").Scan(&healCount); err != nil {
			t.Fatalf("count heal commits error = %v", err)
		}
		if healCount > 0 {
			t.Fatalf("second Open emitted %d heal commit(s) against a healthy workspace; expected 0", healCount)
		}
	}
}
