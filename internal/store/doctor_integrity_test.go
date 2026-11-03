package store

import (
	"context"
	"testing"
)

// commitOrphanComment stores a comment whose issue_id names no issue and
// commits it, the way lit commits every write: the violation is in HEAD, with
// a clean working set. FOREIGN_KEY_CHECKS=0 is how a store comes to hold the
// row at all — an older lit, or a sync, writes past the constraint.
func commitOrphanComment(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	if _, err := st.db.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable foreign key checks: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES ('orphan-comment', 'no-such-issue', 'b', '2026-01-01T00:00:00Z', 'import')`); err != nil {
		t.Fatalf("seed orphan comment: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("restore foreign key checks: %v", err)
	}
	if err := st.commitWorkingSet(ctx, "seed a constraint violation"); err != nil {
		t.Fatalf("commit orphan comment: %v", err)
	}
}

// lit commits every write, so a violation a store holds is already in HEAD and
// a verification of the working set's changes finds nothing. The integrity
// check is a statement about the whole store, so it must name the violation
// however the store was opened. [LAW:verifiable-goals]
func TestDoctorIntegrityCheckNamesACommittedConstraintViolation(t *testing.T) {
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	writer, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer writer.Close()

	clean, err := writer.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() on a clean store error = %v", err)
	}
	if clean.IntegrityCheck != "ok" {
		t.Fatalf("Doctor().IntegrityCheck = %q on a clean store, want ok", clean.IntegrityCheck)
	}

	commitOrphanComment(t, ctx, writer)

	written, err := writer.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() through a write open error = %v", err)
	}
	if written.IntegrityCheck != "constraint_violations" {
		t.Errorf("write open: Doctor().IntegrityCheck = %q beside %d foreign key violations, want constraint_violations", written.IntegrityCheck, written.ForeignKeyIssues)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A read open refuses working-set writes, so the check must be one that
	// records nothing; it also must not leave the store dirty for the next open.
	reader, err := OpenForRead(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer reader.Close()
	read, err := reader.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() through a read open error = %v", err)
	}
	if read.IntegrityCheck != "constraint_violations" {
		t.Errorf("read open: Doctor().IntegrityCheck = %q beside %d foreign key violations, want constraint_violations", read.IntegrityCheck, read.ForeignKeyIssues)
	}
	var dirty int
	if err := reader.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_status`).Scan(&dirty); err != nil {
		t.Fatalf("read dolt_status: %v", err)
	}
	if dirty != 0 {
		t.Errorf("a doctor run left %d changed tables in the working set; the check must record nothing", dirty)
	}
}

// The audit log's orphans have one verdict — the orphan-history warning, which
// --fix repairs and which a faithful rebuild of such a source must not be
// refused over. The constraint verification covers every other table, so it
// must not hand the same rows a second verdict of the opposite severity.
// [LAW:single-enforcer]
func TestDoctorGivesAnOrphanAuditRowOneVerdict(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	for _, q := range []string{
		`SET FOREIGN_KEY_CHECKS = 0`,
		`INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at) VALUES ('orphan-event', 'no-such-issue', 'x', 'r', 'a', '2026-01-01T00:00:00Z')`,
		`INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES ('no-such-event', 'f', 'a', 'b')`,
		`SET FOREIGN_KEY_CHECKS = 1`,
	} {
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed orphan audit rows (%s): %v", q, err)
		}
	}
	if err := st.commitWorkingSet(ctx, "seed orphan audit rows"); err != nil {
		t.Fatalf("commit orphan audit rows: %v", err)
	}

	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if report.IntegrityCheck != "ok" || len(report.Errors) != 0 {
		t.Errorf("Doctor() = integrity %q, errors %v for orphan audit rows, want the warning alone", report.IntegrityCheck, report.Errors)
	}
	if report.OrphanHistoryRows != 2 {
		t.Errorf("Doctor().OrphanHistoryRows = %d, want 2: the event and the change under no event", report.OrphanHistoryRows)
	}
	if findings := healthFindings(report); len(findings) != 0 {
		t.Errorf("healthFindings() = %v, want none: the rebuild gate must keep accepting a source with orphan audit rows", findings)
	}

	fixed, err := st.FixIntegrity(ctx)
	if err != nil {
		t.Fatalf("FixIntegrity() error = %v", err)
	}
	if fixed.OrphanHistoryRows != 0 {
		t.Errorf("FixIntegrity().OrphanHistoryRows = %d, want 0 after the repair", fixed.OrphanHistoryRows)
	}
}

// The verification covers every table but the audit log, not the one table a
// first test happened to seed: a committed orphan row in any of them is named.
func TestDoctorIntegrityCheckNamesAViolationInEveryNonAuditTable(t *testing.T) {
	for name, insert := range map[string]string{
		"comments":  `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES ('c', 'no-such-issue', 'b', '2026-01-01T00:00:00Z', 'import')`,
		"labels":    `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES ('no-such-issue', 'l', '2026-01-01T00:00:00Z', 'import')`,
		"relations": `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES ('no-such-src', 'no-such-dst', 'blocked-by', '2026-01-01T00:00:00Z', 'import')`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := openIssueStore(t, ctx)
			for _, q := range []string{`SET FOREIGN_KEY_CHECKS = 0`, insert, `SET FOREIGN_KEY_CHECKS = 1`} {
				if _, err := st.db.ExecContext(ctx, q); err != nil {
					t.Fatalf("seed orphan %s row (%s): %v", name, q, err)
				}
			}
			if err := st.commitWorkingSet(ctx, "seed a constraint violation"); err != nil {
				t.Fatalf("commit orphan %s row: %v", name, err)
			}
			report, err := st.Doctor(ctx)
			if err != nil {
				t.Fatalf("Doctor() error = %v", err)
			}
			if report.IntegrityCheck != "constraint_violations" {
				t.Errorf("Doctor().IntegrityCheck = %q for an orphan %s row, want constraint_violations", report.IntegrityCheck, name)
			}
		})
	}
}
