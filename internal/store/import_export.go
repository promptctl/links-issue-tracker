package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func (s *Store) Export(ctx context.Context) (model.Export, error) {
	issues, err := s.ListIssues(ctx, storage.ListIssuesFilter{Limit: 0, IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		return model.Export{}, err
	}
	rels, err := s.listAllRelations(ctx)
	if err != nil {
		return model.Export{}, err
	}
	comments, err := s.listAllComments(ctx)
	if err != nil {
		return model.Export{}, err
	}
	labels, err := s.listAllLabels(ctx)
	if err != nil {
		return model.Export{}, err
	}
	events, err := s.ListAllEvents(ctx)
	if err != nil {
		return model.Export{}, err
	}
	// hydrateIssues guarantees every Issue it returns is fully hydrated
	// (post-condition in store.go), so Export does not re-check. Issue.MarshalJSON
	// remains the boundary that rejects partial values from any other source.
	return model.Export{Version: 2, WorkspaceID: s.workspaceID, ExportedAt: s.clock.Now(), Issues: issues, Relations: rels, Comments: comments, Labels: labels, Events: events}, nil
}

func (s *Store) Doctor(ctx context.Context) (storage.HealthReport, error) {
	report := storage.HealthReport{
		DependencyCycle: []string{},
		Errors:          []string{},
		Warnings:        []string{},
	}
	report.IntegrityCheck = "ok"
	var violations int
	if err := s.db.QueryRowContext(ctx, `CALL DOLT_VERIFY_CONSTRAINTS()`).Scan(&violations); err != nil {
		return report, fmt.Errorf("verify constraints: %w", err)
	}
	if violations > 0 {
		report.IntegrityCheck = "constraint_violations"
		report.Errors = append(report.Errors, fmt.Sprintf("constraint violations: %d", violations))
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL`,
		`SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL`,
		`SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL`,
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return report, fmt.Errorf("count foreign key issues: %w", err)
		}
		report.ForeignKeyIssues += count
	}
	if report.ForeignKeyIssues > 0 {
		report.Errors = append(report.Errors, fmt.Sprintf("foreign key violations: %d", report.ForeignKeyIssues))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id`).Scan(&report.InvalidRelatedRows); err != nil {
		return report, fmt.Errorf("count invalid related rows: %w", err)
	}
	if report.InvalidRelatedRows > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("invalid related-to ordering rows: %d", report.InvalidRelatedRows))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL`).Scan(&report.OrphanHistoryRows); err != nil {
		return report, fmt.Errorf("count orphan event rows: %w", err)
	}
	if report.OrphanHistoryRows > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("orphan issue event rows: %d", report.OrphanHistoryRows))
	}
	// Rank inversions: blocks relations where the dependency (dst) is ranked
	// below the dependent (src) among lifecycle-live issues. Counted via the
	// same Go-side classifier FixRankInversions consumes so the two cannot
	// disagree about what is an inversion. (Pre-fix this read used a SQL
	// `status != 'closed'` filter that silently excluded every blocks-edge
	// pointing at an epic, since epics carry status=NULL by design.)
	// [LAW:single-enforcer] Doctor count and FixRankInversions are routed
	// through Store.liveRankInversions.
	inversions, err := s.liveRankInversions(ctx)
	if err != nil {
		return report, fmt.Errorf("count rank inversions: %w", err)
	}
	report.RankInversions = len(inversions)
	if report.RankInversions > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("rank inversions: %d (dependencies ranked below dependents)", report.RankInversions))
	}
	// A blocks dependency cycle is the root cause behind a rank inversion that
	// --fix can never clear: it is unsatisfiable by any rank order. Surface the
	// members so the operator knows exactly which edge to remove.
	// [LAW:single-enforcer] Same classifier FixRankInversions refuses on.
	cycle, err := s.liveBlocksCycle(ctx)
	if err != nil {
		return report, fmt.Errorf("detect blocks dependency cycle: %w", err)
	}
	if len(cycle) > 0 {
		report.DependencyCycle = cycle
		report.Warnings = append(report.Warnings, fmt.Sprintf("blocks dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)", strings.Join(cycle, " -> ")))
	}
	return report, nil
}

// FixIntegrity repairs what this engine can be structurally wrong about, then
// re-examines. The repair always runs — the caller that only wanted a look
// called Doctor. [LAW:dataflow-not-control-flow]
func (s *Store) FixIntegrity(ctx context.Context) (storage.HealthReport, error) {
	if err := s.withMutation(ctx, "fsck repair", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_events WHERE issue_id NOT IN (SELECT id FROM issues)`); err != nil {
			return fmt.Errorf("repair orphan events: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE type='related-to' AND src_id = dst_id`); err != nil {
			return fmt.Errorf("repair self related rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE relations SET src_id = dst_id, dst_id = src_id WHERE type='related-to' AND src_id > dst_id`); err != nil {
			return fmt.Errorf("repair related ordering: %w", err)
		}
		return nil
	}); err != nil {
		return storage.HealthReport{}, err
	}
	// The post-repair examination IS Doctor, not a copy of it, so what a repair
	// reports and what an examination reports cannot drift.
	// [LAW:one-source-of-truth]
	return s.Doctor(ctx)
}

func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error {
	return s.replaceFromExport(ctx, export, commitStamp{Message: "replace from export"})
}

// replaceFromExport clears the live tables and rewrites them from an export
// under the ordinary self-retrying mutation pipeline. Restore uses the default
// message; the reconcile's scratch-branch replay does NOT come through here —
// it uses replayDeltaOnScratch, whose transient failures must bubble to the
// scratch-rebuilding outer retry instead of self-rotating the connection.
// [LAW:single-enforcer] One import body (writeExportTx); the stamp and the
// retry policy are the only per-caller values.
func (s *Store) replaceFromExport(ctx context.Context, export model.Export, stamp commitStamp) error {
	return s.withStampedMutation(ctx, stamp, func(ctx context.Context, tx *sql.Tx) error {
		return writeExportTx(ctx, tx, export)
	})
}

// writeExportTx clears the live tables and rewrites them from an export inside
// one transaction — the path for a caller who does NOT know what the tables
// currently hold (a restore) and so must establish the state wholesale.
//
// It is the degenerate case of applyExportDelta, not a second import body: the
// clear makes the live state provably empty, and the transition from empty to
// export is a delta with nothing to remove and everything to add. The whole-row
// INSERTs therefore still live in exactly one place each, shared with the
// reconcile's incremental replay — sole ownership of the restore-and-replay
// path, not of the tables: CreateIssue, AddComment and AddLabel keep their own
// creation-time statements with their own narrower column lists.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
//
// The clear must not name issue_events or issue_event_changes: both cascade
// from issues (ON DELETE CASCADE), so clearing issues already removes them, and
// re-listing them here would be a second, drift-prone statement of a fact the
// schema owns.
func writeExportTx(ctx context.Context, tx *sql.Tx, export model.Export) error {
	for _, table := range []string{"labels", "comments", "relations", "issues"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	return applyExportDelta(ctx, tx, diffExports(model.Export{}, export))
}

// insertIssueStmt is the restore/replay INSERT, kept beside issueRowValues so
// the column list and the value tuple that fills it are read and edited
// together — they must agree positionally, and separating them is how they
// stop agreeing.
//
// This is the whole-row writer, used by a restore and by the reconcile replay.
// It is NOT the only INSERT into issues: CreateIssue (store.go) writes a
// creation-time row with a narrower column list of its own. A schema change
// that adds a column has to be carried to both.
const insertIssueStmt = `INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// issueRowValues is the issues row an export issue becomes: every persisted
// column, in insertIssueStmt's order, with the three normalizations the storage
// layer owns rather than the caller — the container-vs-leaf status, the
// priority domain, and the topic slug.
//
// It exists as its own function because it has two consumers, and the second
// one is why the first is written this way. insertIssueTx binds it. The
// reconcile's delta COMPARES it, to decide whether an issue's row actually
// changed — and comparing this tuple is not the same as comparing the
// model.Issue it came from. A hydrated Issue carries `Labels`, denormalized
// from the labels table, and for a container a lifecycle composed from every
// child; neither is an issues column. Diffing whole Issue values therefore
// called a row "changed" whenever a label moved or any child of an epic did,
// and rewrote that issue and everything ON DELETE CASCADE takes with it.
//
// Deriving the comparison from the writer keeps the property that motivated
// comparing whole values in the first place — nothing is compared by a
// hand-maintained field list that a new column could fall out of — while
// making the subject the row rather than the view of it. Add a column and it
// enters the write and the diff in the same edit, because they are the same
// tuple. [LAW:one-source-of-truth]
//
// Each normalization is a PURE function of the issue, which is what lets a
// comparison over export values stay faithful to the normalized rows on disk:
// equal inputs normalize equally, so an unchanged row is never missed.
func issueRowValues(issue model.Issue) []any {
	var closedAt any
	if value := issue.ClosedAtValue(); value != nil {
		closedAt = value.Format(time.RFC3339Nano)
	}
	// [LAW:single-enforcer] statusForStorage owns the container-vs-leaf
	// decision; the import path inherits it instead of inventing its own
	// default for containers.
	status := statusForStorage(issue)
	// Legacy exports may carry priorities outside the canonical
	// {normal, urgent} range. model.CanonicalPriority — the same
	// authority the live parse gate rejects against — coerces any such
	// value so the CHECK constraint can never reject a restore, without
	// the import path inventing its own notion of the domain.
	// [LAW:single-enforcer]
	priority := model.CanonicalPriority(int(issue.Priority))
	archivedCol, deletedCol := retentionColumns(issue)
	return []any{
		issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), status, priority,
		issue.IssueType, issueid.NormalizeSlug(issue.Topic), issue.AssigneeValue(), issue.Rank, issue.Lane,
		issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt,
		nullableResolution(issue.ResolutionValue()), nullableStringPtr(issue.RedirectTargetValue()),
		archivedCol, deletedCol,
	}
}

func insertIssueTx(ctx context.Context, tx *sql.Tx, issue model.Issue) error {
	if _, err := tx.ExecContext(ctx, insertIssueStmt, issueRowValues(issue)...); err != nil {
		return fmt.Errorf("restore issue %s: %w", issue.ID, err)
	}
	return nil
}

func insertCommentTx(ctx context.Context, tx *sql.Tx, comment model.Comment) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
		return fmt.Errorf("restore comment %s: %w", comment.ID, err)
	}
	return nil
}

func insertLabelTx(ctx context.Context, tx *sql.Tx, label model.Label) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`,
		label.IssueID, label.Name, label.CreatedAt.Format(time.RFC3339Nano), label.CreatedBy); err != nil {
		return fmt.Errorf("restore label %s:%s: %w", label.IssueID, label.Name, err)
	}
	return nil
}

// insertEventTx writes an event together with its field changes: the nested
// rows are part of the event's value, so they land and leave with it (their
// table cascades from issue_events).
//
// The event's attribution is replayed exactly as the dump carried it — the
// checkout performing the restore never substitutes its own. Attribution is
// historical fact about who produced the work, so re-stamping here would
// rewrite history into a claim for whoever happened to run the restore, and
// preserving it is also what makes an export/import round trip lossless.
//
// Writing it verbatim is safe against a corrupted or hand-edited dump because
// model.Attribution.UnmarshalJSON already collapsed any half pair to the absent
// one on the way in. Re-checking here would be a second enforcer of that
// invariant, free to drift from the first and covering only this one consumer.
// [LAW:single-enforcer]
func insertEventTx(ctx context.Context, tx *sql.Tx, event model.IssueEvent) error {
	var actionArg any
	if event.Action != "" {
		actionArg = event.Action
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at, stream_id, workspace_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano),
		nullableString(event.Attribution.Stream()), nullableString(event.Attribution.Workspace())); err != nil {
		return fmt.Errorf("restore issue event %s: %w", event.ID, err)
	}
	for _, change := range event.Changes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES (?, ?, ?, ?)`,
			event.ID, change.Field, nullableString(change.From), nullableString(change.To)); err != nil {
			return fmt.Errorf("restore issue event change %s.%s: %w", event.ID, change.Field, err)
		}
	}
	return nil
}
