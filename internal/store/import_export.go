package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/issueid"
	"github.com/bmf/links-issue-tracker/internal/model"
)

type ImportIssue struct {
	ID          string
	Title       string
	Description string
	Prompt      string
	Status      string
	Priority    int
	IssueType   string
	Topic       string
	Assignee    string
	Rank        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
	Labels      []string
}

type ImportComment struct {
	ID        string
	IssueID   string
	Body      string
	CreatedAt time.Time
	CreatedBy string
}

type ImportRelation struct {
	SrcID     string
	DstID     string
	Type      string
	CreatedAt time.Time
	CreatedBy string
}

type ImportLabel struct {
	IssueID   string
	Name      string
	CreatedAt time.Time
	CreatedBy string
}

type HealthReport struct {
	IntegrityCheck     string   `json:"integrity_check"`
	ForeignKeyIssues   int      `json:"foreign_key_issues"`
	InvalidRelatedRows int      `json:"invalid_related_rows"`
	OrphanHistoryRows  int      `json:"orphan_history_rows"`
	RankInversions     int      `json:"rank_inversions"`
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
	history, err := s.listAllHistory(ctx)
	if err != nil {
		return model.Export{}, err
	}
	// hydrateIssues guarantees every Issue it returns is fully hydrated
	// (post-condition in store.go), so Export does not re-check. Issue.MarshalJSON
	// remains the boundary that rejects partial values from any other source.
	return model.Export{Version: 1, WorkspaceID: s.workspaceID, ExportedAt: time.Now().UTC(), Issues: issues, Relations: rels, Comments: comments, Labels: labels, History: history}, nil
}

func (s *Store) Doctor(ctx context.Context) (HealthReport, error) {
	report := HealthReport{
		Errors:   []string{},
		Warnings: []string{},
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_history h LEFT JOIN issues i ON i.id = h.issue_id WHERE i.id IS NULL`).Scan(&report.OrphanHistoryRows); err != nil {
		return report, fmt.Errorf("count orphan history rows: %w", err)
	}
	if report.OrphanHistoryRows > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("orphan issue history rows: %d", report.OrphanHistoryRows))
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
	return report, nil
}

func (s *Store) Fsck(ctx context.Context, repair bool) (HealthReport, error) {
	if repair {
		if err := s.withMutation(ctx, "fsck repair", func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM issue_history WHERE issue_id NOT IN (SELECT id FROM issues)`); err != nil {
				return fmt.Errorf("repair orphan history: %w", err)
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

func (s *Store) ImportIssue(ctx context.Context, in ImportIssue) error {
	issueType, err := validateIssueType(in.IssueType)
	if err != nil {
		return err
	}
	if err := validatePriority(in.Priority); err != nil {
		return err
	}
	status, err := statusForStorageRaw(issueType, in.Status)
	if err != nil {
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		return errors.New("issue id is required")
	}
	if strings.TrimSpace(in.Title) == "" {
		return errors.New("title is required")
	}
	var closedAt any
	if in.ClosedAt != nil {
		closedAt = in.ClosedAt.Format(time.RFC3339Nano)
	}
	labels, err := canonicalizeLabels(in.Labels)
	if err != nil {
		return err
	}
	return s.withMutation(ctx, "import issue", func(ctx context.Context, tx *sql.Tx) error {
		issueRank := in.Rank
		if issueRank == "" {
			r, err := nextRankAtBottom(ctx, tx)
			if err != nil {
				return fmt.Errorf("import issue rank: %w", err)
			}
			issueRank = r
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(
				id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, NULL, NULL)
			ON DUPLICATE KEY UPDATE
				title = VALUES(title),
				description = VALUES(description),
				agent_prompt = VALUES(agent_prompt),
				status = VALUES(status),
				priority = VALUES(priority),
				issue_type = VALUES(issue_type),
				topic = VALUES(topic),
				assignee = VALUES(assignee),
				item_rank = VALUES(item_rank),
				created_at = VALUES(created_at),
				updated_at = VALUES(updated_at),
				closed_at = VALUES(closed_at)`,
			in.ID,
			strings.TrimSpace(in.Title),
			strings.TrimSpace(in.Description),
			nullableString(strings.TrimSpace(in.Prompt)),
			status,
			in.Priority,
			issueType,
			issueid.NormalizeSlug(in.Topic),
			strings.TrimSpace(in.Assignee),
			issueRank,
			in.CreatedAt.Format(time.RFC3339Nano),
			in.UpdatedAt.Format(time.RFC3339Nano),
			closedAt,
		); err != nil {
			return fmt.Errorf("import issue: %w", err)
		}
		return s.replaceLabelsTx(ctx, tx, in.ID, labels, "import")
	})
}

func (s *Store) ImportComment(ctx context.Context, in ImportComment) error {
	if _, err := s.GetIssue(ctx, in.IssueID); err != nil {
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		return errors.New("comment id is required")
	}
	if strings.TrimSpace(in.Body) == "" {
		return errors.New("comment body is required")
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}
	return s.withMutation(ctx, "import comment", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				issue_id = VALUES(issue_id),
				body = VALUES(body),
				created_at = VALUES(created_at),
				created_by = VALUES(created_by)`,
			in.ID, in.IssueID, strings.TrimSpace(in.Body), in.CreatedAt.Format(time.RFC3339Nano), createdBy); err != nil {
			return fmt.Errorf("import comment: %w", err)
		}
		return nil
	})
}

func (s *Store) ImportRelation(ctx context.Context, in ImportRelation) error {
	if _, err := s.GetIssue(ctx, in.SrcID); err != nil {
		return err
	}
	if _, err := s.GetIssue(ctx, in.DstID); err != nil {
		return err
	}
	relType := strings.TrimSpace(in.Type)
	if relType != "blocks" && relType != "parent-child" && relType != "related-to" {
		return errors.New("relation type must be blocks, parent-child, or related-to")
	}
	srcID, dstID := in.SrcID, in.DstID
	if relType == "related-to" {
		ordered := []string{srcID, dstID}
		sort.Strings(ordered)
		srcID, dstID = ordered[0], ordered[1]
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}
	return s.withMutation(ctx, "import relation", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				created_at = VALUES(created_at),
				created_by = VALUES(created_by)`,
			srcID, dstID, relType, in.CreatedAt.Format(time.RFC3339Nano), createdBy); err != nil {
			return fmt.Errorf("import relation: %w", err)
		}
		return nil
	})
}

func (s *Store) ImportLabel(ctx context.Context, in ImportLabel) error {
	if _, err := s.GetIssue(ctx, in.IssueID); err != nil {
		return err
	}
	label, err := normalizeLabel(in.Name)
	if err != nil {
		return err
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}
	return s.withMutation(ctx, "import label", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				created_at = VALUES(created_at),
				created_by = VALUES(created_by)`, in.IssueID, label, in.CreatedAt.Format(time.RFC3339Nano), createdBy); err != nil {
			return fmt.Errorf("import label: %w", err)
		}
		return nil
	})
}

func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error {
	return s.withMutation(ctx, "replace from export", func(ctx context.Context, tx *sql.Tx) error {
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?)`,
				issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), status, issue.Priority, issue.IssueType, issueid.NormalizeSlug(issue.Topic), issue.AssigneeValue(), issue.Rank, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt)); err != nil {
				return fmt.Errorf("restore issue %s: %w", issue.ID, err)
			}
		}
		for _, relation := range export.Relations {
			if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
				relation.SrcID, relation.DstID, relation.Type, relation.CreatedAt.Format(time.RFC3339Nano), relation.CreatedBy); err != nil {
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
		for _, event := range export.History {
			if _, err := tx.ExecContext(ctx, `INSERT INTO issue_history(id, issue_id, action, reason, from_status, to_status, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				event.ID, event.IssueID, event.Action, event.Reason, event.FromStatus, event.ToStatus, event.CreatedAt.Format(time.RFC3339Nano), event.CreatedBy); err != nil {
				return fmt.Errorf("restore issue history %s: %w", event.ID, err)
			}
		}
		return nil
	})
}
