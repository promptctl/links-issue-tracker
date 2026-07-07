package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

type HealthReport struct {
	IntegrityCheck     string   `json:"integrity_check"`
	ForeignKeyIssues   int      `json:"foreign_key_issues"`
	InvalidRelatedRows int      `json:"invalid_related_rows"`
	OrphanHistoryRows  int      `json:"orphan_history_rows"`
	RankInversions     int      `json:"rank_inversions"`
	DependencyCycle    []string `json:"dependency_cycle"`
	Errors             []string `json:"errors"`
	Warnings           []string `json:"warnings"`
}

func (s *Store) Export(ctx context.Context) (model.Export, error) {
	issues, err := s.ListIssues(ctx, ListIssuesFilter{Limit: 0, IncludeArchived: true, IncludeDeleted: true})
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
	events, err := s.listAllEvents(ctx)
	if err != nil {
		return model.Export{}, err
	}
	// hydrateIssues guarantees every Issue it returns is fully hydrated
	// (post-condition in store.go), so Export does not re-check. Issue.MarshalJSON
	// remains the boundary that rejects partial values from any other source.
	return model.Export{Version: 2, WorkspaceID: s.workspaceID, ExportedAt: time.Now().UTC(), Issues: issues, Relations: rels, Comments: comments, Labels: labels, Events: events}, nil
}

func (s *Store) Doctor(ctx context.Context) (HealthReport, error) {
	report := HealthReport{
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

func (s *Store) Fsck(ctx context.Context, repair bool) (HealthReport, error) {
	if repair {
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
			return HealthReport{}, err
		}
	}
	return s.Doctor(ctx)
}

func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error {
	return s.replaceFromExport(ctx, export, "replace from export")
}

// replaceFromExport is the single body that clears the live tables and rewrites
// them from an export, parameterized only by the commit message. Restore uses
// the default message; the field-aware reconcile passes its own so a forward-
// replayed merge reads as a reconcile in history rather than a generic restore.
// [LAW:single-enforcer] One import body; the message is the only per-caller value.
func (s *Store) replaceFromExport(ctx context.Context, export model.Export, message string) error {
	return s.withMutation(ctx, message, func(ctx context.Context, tx *sql.Tx) error {
		for _, table := range []string{"labels", "comments", "relations", "issues"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("clear %s: %w", table, err)
			}
		}
		for _, issue := range export.Issues {
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), status, priority, issue.IssueType, issueid.NormalizeSlug(issue.Topic), issue.AssigneeValue(), issue.Rank, issue.Lane, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableResolution(issue.ResolutionValue()), nullableStringPtr(issue.RedirectTargetValue()), archivedCol, deletedCol); err != nil {
				return fmt.Errorf("restore issue %s: %w", issue.ID, err)
			}
		}
		for _, relation := range export.Relations {
			// [LAW:one-source-of-truth] Restore each exported edge through
			// insertRelationTx so the relations INSERT lives only there.
			if err := insertRelationTx(ctx, tx, relation); err != nil {
				return fmt.Errorf("restore relation %s->%s: %w", relation.SrcID, relation.DstID, err)
			}
		}
		for _, comment := range export.Comments {
			if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
				comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
				return fmt.Errorf("restore comment %s: %w", comment.ID, err)
			}
		}
		for _, label := range export.Labels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`,
				label.IssueID, label.Name, label.CreatedAt.Format(time.RFC3339Nano), label.CreatedBy); err != nil {
				return fmt.Errorf("restore label %s:%s: %w", label.IssueID, label.Name, err)
			}
		}
		for _, event := range export.Events {
			var actionArg any
			if event.Action != "" {
				actionArg = event.Action
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("restore issue event %s: %w", event.ID, err)
			}
			for _, change := range event.Changes {
				if _, err := tx.ExecContext(ctx, `INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES (?, ?, ?, ?)`,
					event.ID, change.Field, nullableString(change.From), nullableString(change.To)); err != nil {
					return fmt.Errorf("restore issue event change %s.%s: %w", event.ID, change.Field, err)
				}
			}
		}
		return nil
	})
}
