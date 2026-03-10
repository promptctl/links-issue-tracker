package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/dolthub/driver"

	"github.com/google/uuid"

	"github.com/bmf/links-issue-tracker/internal/model"
)

const (
	doltDriverName   = "dolt"
	doltDatabaseName = "links"
)

var ErrTransientManifestReadOnly = errors.New("transient manifest read-only")
var processCommitMutex sync.Mutex
var commitLockPIDRunning = isCommitLockPIDRunning

const (
	transientManifestRetryMaxAttempts = 12
	transientManifestRetryBaseDelay   = 50 * time.Millisecond
	transientManifestRetryMaxDelay    = 1 * time.Second
	commitLockStaleAfter              = 10 * time.Minute
)

type Store struct {
	db             *sql.DB
	workspaceID    string
	commitLockPath string
}

type retryOperation func(context.Context) error
type retryDelayFunc func(attempt int) time.Duration
type retrySleepFunc func(context.Context, time.Duration) error
type commitLockContextKey struct{}

type NotFoundError struct {
	Entity string
	ID     string
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Entity, e.ID)
}

type SyncState struct {
	Path        string
	ContentHash string
}

type ImportIssue struct {
	ID          string
	Title       string
	Description string
	Status      string
	Priority    int
	IssueType   string
	Assignee    string
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

type CreateIssueInput struct {
	Title       string
	Description string
	IssueType   string
	Priority    int
	Assignee    string
	Labels      []string
}

type UpdateIssueInput struct {
	Title       *string
	Description *string
	IssueType   *string
	Status      *string
	Priority    *int
	Assignee    *string
	Labels      *[]string
}

type SortSpec struct {
	Field string
	Desc  bool
}

type ListIssuesFilter struct {
	Status          string
	IssueType       string
	Assignee        string
	PriorityMin     *int
	PriorityMax     *int
	SearchTerms     []string
	IDs             []string
	HasComments     *bool
	LabelsAll       []string
	UpdatedAfter    *time.Time
	UpdatedBefore   *time.Time
	IncludeArchived bool
	IncludeDeleted  bool
	SortBy          []SortSpec
	Limit           int
}

type AddCommentInput struct {
	IssueID   string
	Body      string
	CreatedBy string
}

type AddRelationInput struct {
	SrcID     string
	DstID     string
	Type      string
	CreatedBy string
}

type SetParentInput struct {
	ChildID   string
	ParentID  string
	CreatedBy string
}

type AddLabelInput struct {
	IssueID   string
	Name      string
	CreatedBy string
}

type TransitionIssueInput struct {
	IssueID   string
	Action    string
	Reason    string
	CreatedBy string
}

type HealthReport struct {
	IntegrityCheck     string   `json:"integrity_check"`
	ForeignKeyIssues   int      `json:"foreign_key_issues"`
	InvalidRelatedRows int      `json:"invalid_related_rows"`
	OrphanHistoryRows  int      `json:"orphan_history_rows"`
	Errors             []string `json:"errors"`
	Warnings           []string `json:"warnings"`
}

func Open(ctx context.Context, doltRootDir string, workspaceID string) (*Store, error) {
	if strings.TrimSpace(doltRootDir) == "" {
		return nil, errors.New("dolt root dir is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("workspace id is required")
	}
	if err := EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	db, err := sql.Open(doltDriverName, buildDoltDSN(doltRootDir, workspaceID, true))
	if err != nil {
		return nil, fmt.Errorf("open dolt: %w", err)
	}
	s := &Store{
		db:             db,
		workspaceID:    workspaceID,
		commitLockPath: filepath.Join(filepath.Clean(doltRootDir), ".links-commit.lock"),
	}
	// [LAW:single-enforcer] Store-level commit lock is the single writer gate for all startup and runtime mutations.
	if err := s.withCommitLock(ctx, s.migrate); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func EnsureDatabase(ctx context.Context, doltRootDir string, workspaceID string) error {
	if strings.TrimSpace(doltRootDir) == "" {
		return errors.New("dolt root dir is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("workspace id is required")
	}
	return ensureDoltDatabase(ctx, doltRootDir, workspaceID)
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	schema := []string{
		`CREATE TABLE meta (
			meta_key VARCHAR(191) PRIMARY KEY,
			meta_value TEXT NOT NULL
		);`,
		`CREATE TABLE issues (
			id VARCHAR(191) PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			status VARCHAR(32) NOT NULL,
			priority INT NOT NULL,
			issue_type VARCHAR(32) NOT NULL,
			assignee TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL,
			updated_at VARCHAR(64) NOT NULL,
			closed_at VARCHAR(64) NULL,
			archived_at VARCHAR(64) NULL,
			deleted_at VARCHAR(64) NULL,
			CHECK(status IN ('open','in_progress','closed')),
			CHECK(priority >= 0 AND priority <= 4),
			CHECK(issue_type IN ('task','feature','bug','chore','epic'))
		);`,
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
		if err := execIgnoreAlreadyExists(ctx, s.db, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureUnifiedStatusSchema(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(meta_key, meta_value) VALUES ('workspace_id', ?)
		 ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`, s.workspaceID); err != nil {
		return fmt.Errorf("store workspace_id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO meta(meta_key, meta_value) VALUES ('schema_version', '1')`); err != nil {
		return fmt.Errorf("store schema_version: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "Initialize links schema"); err != nil {
		return err
	}
	return nil
}

type issueCheckConstraint struct {
	name   string
	clause string
}

func (s *Store) ensureUnifiedStatusSchema(ctx context.Context) error {
	// [LAW:one-source-of-truth] `status` is the canonical workflow state.
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET status = 'in_progress' WHERE status = 'in-progress'`); err != nil {
		return fmt.Errorf("normalize legacy in-progress status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET status = 'open' WHERE status = 'todo'`); err != nil {
		return fmt.Errorf("normalize legacy todo status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET status = 'closed' WHERE status = 'done'`); err != nil {
		return fmt.Errorf("normalize legacy done status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET status = 'open' WHERE status NOT IN ('open','in_progress','closed')`); err != nil {
		return fmt.Errorf("normalize invalid status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET status = 'closed' WHERE closed_at IS NOT NULL AND status <> 'closed'`); err != nil {
		return fmt.Errorf("normalize closed_at status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issues SET closed_at = NULL WHERE status <> 'closed'`); err != nil {
		return fmt.Errorf("normalize non-closed closed_at: %w", err)
	}
	if err := s.ensureStatusConstraint(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureStatusConstraint(ctx context.Context) error {
	checks, err := s.listIssueStatusCheckConstraints(ctx)
	if err != nil {
		return err
	}
	if hasCanonicalStatusConstraint(checks) {
		return nil
	}
	for _, constraint := range checks {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE issues DROP CHECK `"+strings.ReplaceAll(constraint.name, "`", "``")+"`"); err != nil {
			return fmt.Errorf("drop status check %s: %w", constraint.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE issues ADD CONSTRAINT issues_status_check CHECK (status IN ('open','in_progress','closed'))`); err != nil {
		return fmt.Errorf("add canonical status check: %w", err)
	}
	return nil
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
	return strings.Contains(normalizeConstraintClause(constraints[0].clause), "statusin('open','in_progress','closed')")
}

func normalizeConstraintClause(clause string) string {
	replacer := strings.NewReplacer(" ", "", "\t", "", "\n", "", "`", "")
	return strings.ToLower(replacer.Replace(clause))
}
func (s *Store) GetSyncState(ctx context.Context) (SyncState, error) {
	state := SyncState{}
	var err error
	state.Path, err = s.getMeta(ctx, nil, "last_sync_path")
	if err != nil {
		return SyncState{}, err
	}
	state.ContentHash, err = s.getMeta(ctx, nil, "last_sync_hash")
	if err != nil {
		return SyncState{}, err
	}
	return state, nil
}

func (s *Store) RecordSyncState(ctx context.Context, state SyncState) error {
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record sync state tx: %w", err)
	}
	defer tx.Rollback()
	for key, value := range map[string]string{
		"last_sync_path": strings.TrimSpace(state.Path),
		"last_sync_hash": strings.TrimSpace(state.ContentHash),
	} {
		if err := s.setMeta(ctx, tx, key, value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record sync state: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "record sync state"); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateIssue(ctx context.Context, in CreateIssueInput) (model.Issue, error) {
	var issue model.Issue
	err := retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
		created, createErr := s.createIssueOnce(ctx, in)
		if createErr != nil {
			return createErr
		}
		issue = created
		return nil
	}, transientManifestRetryDelay, waitWithContext)
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) createIssueOnce(ctx context.Context, in CreateIssueInput) (model.Issue, error) {
	if strings.TrimSpace(in.Title) == "" {
		return model.Issue{}, errors.New("title is required")
	}
	issueType, err := validateIssueType(in.IssueType)
	if err != nil {
		return model.Issue{}, err
	}
	priority := in.Priority
	if err := validatePriority(priority); err != nil {
		return model.Issue{}, err
	}
	now := time.Now().UTC()
	labels, err := canonicalizeLabels(in.Labels)
	if err != nil {
		return model.Issue{}, err
	}
	issue := model.Issue{
		ID:          newIssueID(s.workspaceID),
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Status:      "open",
		Priority:    priority,
		IssueType:   issueType,
		Assignee:    strings.TrimSpace(in.Assignee),
		Labels:      labels,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Issue{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin create issue tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO issues(
		id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at, archived_at, deleted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL)`,
		issue.ID, issue.Title, issue.Description, issue.Status, issue.Priority, issue.IssueType,
		issue.Assignee, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return model.Issue{}, fmt.Errorf("insert issue: %w", err)
	}
	if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, "links"); err != nil {
		return model.Issue{}, err
	}
	if err := s.insertHistoryTx(ctx, tx, issue.ID, "created", "issue created", "", "open", "links"); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit create issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "create issue"); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error) {
	query := `SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type, i.assignee, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at FROM issues i`
	var where []string
	var args []any
	if !filter.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	if !filter.IncludeDeleted {
		where = append(where, "i.deleted_at IS NULL")
	}
	if filter.Status != "" {
		status, err := normalizeStatus(filter.Status)
		if err != nil {
			return nil, err
		}
		where = append(where, "i.status = ?")
		args = append(args, status)
	}
	if filter.IssueType != "" {
		where = append(where, "i.issue_type = ?")
		args = append(args, filter.IssueType)
	}
	if filter.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, filter.Assignee)
	}
	if filter.PriorityMin != nil {
		where = append(where, "i.priority >= ?")
		args = append(args, *filter.PriorityMin)
	}
	if filter.PriorityMax != nil {
		where = append(where, "i.priority <= ?")
		args = append(args, *filter.PriorityMax)
	}
	if filter.UpdatedAfter != nil {
		where = append(where, "i.updated_at >= ?")
		args = append(args, filter.UpdatedAfter.UTC().Format(time.RFC3339Nano))
	}
	if filter.UpdatedBefore != nil {
		where = append(where, "i.updated_at <= ?")
		args = append(args, filter.UpdatedBefore.UTC().Format(time.RFC3339Nano))
	}
	if filter.HasComments != nil {
		if *filter.HasComments {
			where = append(where, "EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)")
		} else {
			where = append(where, "NOT EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)")
		}
	}
	if len(filter.LabelsAll) > 0 {
		labels, err := canonicalizeLabels(filter.LabelsAll)
		if err != nil {
			return nil, err
		}
		for _, label := range labels {
			where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)")
			args = append(args, label)
		}
	}
	if len(filter.IDs) > 0 {
		placeholders := make([]string, 0, len(filter.IDs))
		for _, id := range filter.IDs {
			trimmed := strings.TrimSpace(id)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			where = append(where, "i.id IN ("+strings.Join(placeholders, ", ")+")")
		}
	}
	for _, term := range filter.SearchTerms {
		trimmed := strings.ToLower(strings.TrimSpace(term))
		if trimmed == "" {
			continue
		}
		where = append(where, "(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ?)")
		like := "%" + trimmed + "%"
		args = append(args, like, like)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	orderClause, err := buildIssueOrderClause(filter.SortBy)
	if err != nil {
		return nil, err
	}
	query += " ORDER BY " + orderClause
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w (query=%s)", err, query)
	}
	defer rows.Close()
	issues := []model.Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.attachLabels(ctx, issues)
}

func (s *Store) GetIssueDetail(ctx context.Context, id string) (model.IssueDetail, error) {
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	relations, err := s.listRelations(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	comments, err := s.listComments(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	history, err := s.listHistory(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	detail := model.IssueDetail{
		Issue:     issue,
		Relations: relations,
		Comments:  comments,
		History:   history,
		Children:  []model.Issue{},
		DependsOn: []model.Issue{},
		Related:   []model.Issue{},
		BlockedBy: []model.Issue{},
	}
	for _, rel := range relations {
		switch rel.Type {
		case "blocks":
			if rel.SrcID == id {
				dep, err := s.GetIssue(ctx, rel.DstID)
				if err == nil {
					detail.DependsOn = append(detail.DependsOn, dep)
				}
			}
			if rel.DstID == id {
				blocked, err := s.GetIssue(ctx, rel.SrcID)
				if err == nil {
					detail.BlockedBy = append(detail.BlockedBy, blocked)
				}
			}
		case "parent-child":
			if rel.SrcID == id {
				parent, err := s.GetIssue(ctx, rel.DstID)
				if err == nil {
					detail.Parent = &parent
				}
			}
			if rel.DstID == id {
				child, err := s.GetIssue(ctx, rel.SrcID)
				if err == nil {
					detail.Children = append(detail.Children, child)
				}
			}
		case "related-to":
			otherID := rel.SrcID
			if otherID == id {
				otherID = rel.DstID
			}
			related, err := s.GetIssue(ctx, otherID)
			if err == nil {
				detail.Related = append(detail.Related, related)
			}
		}
	}
	labeled, err := s.attachLabels(ctx, detail.Children)
	if err != nil {
		return model.IssueDetail{}, err
	}
	detail.Children = labeled
	labeled, err = s.attachLabels(ctx, detail.DependsOn)
	if err != nil {
		return model.IssueDetail{}, err
	}
	detail.DependsOn = labeled
	labeled, err = s.attachLabels(ctx, detail.Related)
	if err != nil {
		return model.IssueDetail{}, err
	}
	detail.Related = labeled
	labeled, err = s.attachLabels(ctx, detail.BlockedBy)
	if err != nil {
		return model.IssueDetail{}, err
	}
	detail.BlockedBy = labeled
	if detail.Parent != nil {
		parentIssues, err := s.attachLabels(ctx, []model.Issue{*detail.Parent})
		if err != nil {
			return model.IssueDetail{}, err
		}
		detail.Parent = &parentIssues[0]
	}
	return detail, nil
}

func (s *Store) GetIssue(ctx context.Context, id string) (model.Issue, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at, archived_at, deleted_at FROM issues WHERE id = ?`, id)
	issue, err := scanIssue(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Issue{}, NotFoundError{Entity: "issue", ID: id}
		}
		return model.Issue{}, err
	}
	labeled, err := s.attachLabels(ctx, []model.Issue{issue})
	if err != nil {
		return model.Issue{}, err
	}
	return labeled[0], nil
}

func (s *Store) UpdateIssue(ctx context.Context, id string, in UpdateIssueInput) (model.Issue, error) {
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if in.Title != nil {
		issue.Title = strings.TrimSpace(*in.Title)
		if issue.Title == "" {
			return model.Issue{}, errors.New("title cannot be empty")
		}
	}
	if in.Description != nil {
		issue.Description = strings.TrimSpace(*in.Description)
	}
	if in.IssueType != nil {
		issueType, err := validateIssueType(*in.IssueType)
		if err != nil {
			return model.Issue{}, err
		}
		issue.IssueType = issueType
	}
	if in.Status != nil {
		return model.Issue{}, errors.New("status transitions require dedicated lifecycle commands")
	}
	if in.Priority != nil {
		if err := validatePriority(*in.Priority); err != nil {
			return model.Issue{}, err
		}
		issue.Priority = *in.Priority
	}
	if in.Assignee != nil {
		issue.Assignee = strings.TrimSpace(*in.Assignee)
	}
	if in.Labels != nil {
		labels, err := canonicalizeLabels(*in.Labels)
		if err != nil {
			return model.Issue{}, err
		}
		issue.Labels = labels
	}
	issue.UpdatedAt = time.Now().UTC()
	var closedAt any
	if issue.ClosedAt != nil {
		closedAt = issue.ClosedAt.Format(time.RFC3339Nano)
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Issue{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin update issue tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE issues SET
		title = ?, description = ?, status = ?, priority = ?, issue_type = ?, assignee = ?, updated_at = ?, closed_at = ?, archived_at = ?, deleted_at = ?
		WHERE id = ?`, issue.Title, issue.Description, issue.Status, issue.Priority, issue.IssueType, issue.Assignee, issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID)
	if err != nil {
		return model.Issue{}, fmt.Errorf("update issue: %w", err)
	}
	if in.Labels != nil {
		if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, "links"); err != nil {
			return model.Issue{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit update issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "update issue"); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) AddComment(ctx context.Context, in AddCommentInput) (model.Comment, error) {
	var comment model.Comment
	err := retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
		created, createErr := s.addCommentOnce(ctx, in)
		if createErr != nil {
			return createErr
		}
		comment = created
		return nil
	}, transientManifestRetryDelay, waitWithContext)
	if err != nil {
		return model.Comment{}, err
	}
	return comment, nil
}

func (s *Store) addCommentOnce(ctx context.Context, in AddCommentInput) (model.Comment, error) {
	if _, err := s.GetIssue(ctx, in.IssueID); err != nil {
		return model.Comment{}, err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return model.Comment{}, errors.New("comment body is required")
	}
	now := time.Now().UTC()
	comment := model.Comment{ID: "cmt-" + uuid.NewString(), IssueID: in.IssueID, Body: body, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}
	if comment.CreatedBy == "" {
		comment.CreatedBy = "unknown"
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Comment{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Comment{}, fmt.Errorf("begin add comment tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
		return model.Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Comment{}, fmt.Errorf("commit add comment: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "add comment"); err != nil {
		return model.Comment{}, err
	}
	return comment, nil
}

func (s *Store) AddRelation(ctx context.Context, in AddRelationInput) (model.Relation, error) {
	if _, err := s.GetIssue(ctx, in.SrcID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.DstID); err != nil {
		return model.Relation{}, err
	}
	relType := strings.TrimSpace(in.Type)
	if relType != "blocks" && relType != "parent-child" && relType != "related-to" {
		return model.Relation{}, errors.New("relation type must be blocks, parent-child, or related-to")
	}
	srcID, dstID := in.SrcID, in.DstID
	if relType == "related-to" {
		if srcID == dstID {
			return model.Relation{}, errors.New("related-to cannot target itself")
		}
		ordered := []string{srcID, dstID}
		sort.Strings(ordered)
		srcID, dstID = ordered[0], ordered[1]
	}
	now := time.Now().UTC()
	rel := model.Relation{SrcID: srcID, DstID: dstID, Type: relType, CreatedAt: now, CreatedBy: strings.TrimSpace(in.CreatedBy)}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Relation{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Relation{}, fmt.Errorf("begin add relation tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
		return model.Relation{}, fmt.Errorf("insert relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Relation{}, fmt.Errorf("commit add relation: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "add relation"); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

func (s *Store) RemoveRelation(ctx context.Context, srcID, dstID, relType string) error {
	if relType == "related-to" {
		ordered := []string{srcID, dstID}
		sort.Strings(ordered)
		srcID, dstID = ordered[0], ordered[1]
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remove relation tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND dst_id = ? AND type = ?`, srcID, dstID, relType)
	if err != nil {
		return fmt.Errorf("delete relation: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("relation not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remove relation: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "remove relation"); err != nil {
		return err
	}
	return nil
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
	return report, nil
}

func (s *Store) Fsck(ctx context.Context, repair bool) (HealthReport, error) {
	if repair {
		ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
		if err != nil {
			return HealthReport{}, err
		}
		defer releaseCommitLock()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return HealthReport{}, fmt.Errorf("begin fsck repair tx: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_history WHERE issue_id NOT IN (SELECT id FROM issues)`); err != nil {
			return HealthReport{}, fmt.Errorf("repair orphan history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE type='related-to' AND src_id = dst_id`); err != nil {
			return HealthReport{}, fmt.Errorf("repair self related rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE relations SET src_id = dst_id, dst_id = src_id WHERE type='related-to' AND src_id > dst_id`); err != nil {
			return HealthReport{}, fmt.Errorf("repair related ordering: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return HealthReport{}, fmt.Errorf("commit fsck repair: %w", err)
		}
		if err := s.commitWorkingSet(ctx, "fsck repair"); err != nil {
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
	status, err := normalizeStatus(in.Status)
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
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import issue tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO issues(
			id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at, archived_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
		ON DUPLICATE KEY UPDATE
			title = VALUES(title),
			description = VALUES(description),
			status = VALUES(status),
			priority = VALUES(priority),
			issue_type = VALUES(issue_type),
			assignee = VALUES(assignee),
			created_at = VALUES(created_at),
			updated_at = VALUES(updated_at),
			closed_at = VALUES(closed_at)`,
		in.ID,
		strings.TrimSpace(in.Title),
		strings.TrimSpace(in.Description),
		status,
		in.Priority,
		issueType,
		strings.TrimSpace(in.Assignee),
		in.CreatedAt.Format(time.RFC3339Nano),
		in.UpdatedAt.Format(time.RFC3339Nano),
		closedAt,
	)
	if err != nil {
		return fmt.Errorf("import issue: %w", err)
	}
	labels, err := canonicalizeLabels(in.Labels)
	if err != nil {
		return err
	}
	if err := s.replaceLabelsTx(ctx, tx, in.ID, labels, "import"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "import issue"); err != nil {
		return err
	}
	return nil
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
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import comment tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			issue_id = VALUES(issue_id),
			body = VALUES(body),
			created_at = VALUES(created_at),
			created_by = VALUES(created_by)`,
		in.ID, in.IssueID, strings.TrimSpace(in.Body), in.CreatedAt.Format(time.RFC3339Nano), createdBy)
	if err != nil {
		return fmt.Errorf("import comment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import comment: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "import comment"); err != nil {
		return err
	}
	return nil
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
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import relation tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			created_at = VALUES(created_at),
			created_by = VALUES(created_by)`,
		srcID, dstID, relType, in.CreatedAt.Format(time.RFC3339Nano), createdBy)
	if err != nil {
		return fmt.Errorf("import relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import relation: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "import relation"); err != nil {
		return err
	}
	return nil
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
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import label tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			created_at = VALUES(created_at),
			created_by = VALUES(created_by)`, in.IssueID, label, in.CreatedAt.Format(time.RFC3339Nano), createdBy)
	if err != nil {
		return fmt.Errorf("import label: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import label: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "import label"); err != nil {
		return err
	}
	return nil
}

func (s *Store) AddLabel(ctx context.Context, in AddLabelInput) ([]string, error) {
	if _, err := s.GetIssue(ctx, in.IssueID); err != nil {
		return nil, err
	}
	label, err := normalizeLabel(in.Name)
	if err != nil {
		return nil, err
	}
	createdBy := strings.TrimSpace(in.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin add label tx: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE issue_id = issue_id`, in.IssueID, label, time.Now().UTC().Format(time.RFC3339Nano), createdBy)
	if err != nil {
		return nil, fmt.Errorf("insert label: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit add label: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "add label"); err != nil {
		return nil, err
	}
	return s.ListLabels(ctx, in.IssueID)
}

func (s *Store) RemoveLabel(ctx context.Context, issueID, labelName string) ([]string, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return nil, err
	}
	label, err := normalizeLabel(labelName)
	if err != nil {
		return nil, err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin remove label tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ? AND label = ?`, issueID, label)
	if err != nil {
		return nil, fmt.Errorf("delete label: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil, fmt.Errorf("label %q not found on issue %q", label, issueID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit remove label: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "remove label"); err != nil {
		return nil, err
	}
	return s.ListLabels(ctx, issueID)
}

func (s *Store) ReplaceLabels(ctx context.Context, issueID string, labels []string, createdBy string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	normalized, err := canonicalizeLabels(labels)
	if err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace labels tx: %w", err)
	}
	defer tx.Rollback()
	if err := s.replaceLabelsTx(ctx, tx, issueID, normalized, createdBy); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace labels: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "replace labels"); err != nil {
		return err
	}
	return nil
}

func (s *Store) TransitionIssue(ctx context.Context, in TransitionIssueInput) (model.Issue, error) {
	var issue model.Issue
	err := retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
		transitioned, transitionErr := s.transitionIssueOnce(ctx, in)
		if transitionErr != nil {
			return transitionErr
		}
		issue = transitioned
		return nil
	}, transientManifestRetryDelay, waitWithContext)
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) transitionIssueOnce(ctx context.Context, in TransitionIssueInput) (model.Issue, error) {
	issue, err := s.GetIssue(ctx, in.IssueID)
	if err != nil {
		return model.Issue{}, err
	}
	action := strings.TrimSpace(in.Action)
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return model.Issue{}, errors.New("reason is required")
	}
	actor := strings.TrimSpace(in.CreatedBy)
	if actor == "" {
		actor = "unknown"
	}
	switch action {
	case "start":
		return s.transitionStatusAtomic(ctx, issue, actor, reason, action, "open", "in_progress")
	case "done":
		return s.transitionStatusAtomic(ctx, issue, actor, reason, action, "in_progress", "closed")
	}
	now := time.Now().UTC()
	fromStatus := issue.Status
	toStatus := issue.Status
	switch action {
	case "close":
		if issue.DeletedAt != nil || issue.ArchivedAt != nil {
			return model.Issue{}, errors.New("cannot close archived or deleted issue")
		}
		if issue.Status == "closed" {
			return model.Issue{}, errors.New("issue is already closed")
		}
		issue.Status = "closed"
		issue.ClosedAt = &now
		toStatus = "closed"
	case "reopen":
		if issue.DeletedAt != nil || issue.ArchivedAt != nil {
			return model.Issue{}, errors.New("cannot reopen archived or deleted issue")
		}
		if issue.Status == "open" {
			return model.Issue{}, errors.New("issue is already open")
		}
		if issue.Status != "closed" {
			return model.Issue{}, errors.New("issue is not closed")
		}
		issue.Status = "open"
		issue.ClosedAt = nil
		toStatus = "open"
	case "archive":
		if issue.DeletedAt != nil {
			return model.Issue{}, errors.New("cannot archive deleted issue")
		}
		if issue.ArchivedAt != nil {
			return model.Issue{}, errors.New("issue is already archived")
		}
		issue.ArchivedAt = &now
	case "unarchive":
		if issue.DeletedAt != nil {
			return model.Issue{}, errors.New("cannot unarchive deleted issue")
		}
		if issue.ArchivedAt == nil {
			return model.Issue{}, errors.New("issue is not archived")
		}
		issue.ArchivedAt = nil
	case "delete":
		if issue.DeletedAt != nil {
			return model.Issue{}, errors.New("issue is already deleted")
		}
		issue.DeletedAt = &now
	case "restore":
		if issue.DeletedAt == nil {
			return model.Issue{}, errors.New("issue is not deleted")
		}
		issue.DeletedAt = nil
	default:
		return model.Issue{}, fmt.Errorf("unsupported lifecycle action %q", action)
	}
	issue.UpdatedAt = now
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Issue{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin transition issue tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, updated_at = ?, closed_at = ?, archived_at = ?, deleted_at = ? WHERE id = ?`,
		issue.Status, issue.UpdatedAt.Format(time.RFC3339Nano), nullableTime(issue.ClosedAt), nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID); err != nil {
		return model.Issue{}, fmt.Errorf("update issue lifecycle: %w", err)
	}
	if err := s.insertHistoryTx(ctx, tx, issue.ID, action, reason, fromStatus, toStatus, actor); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit transition issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "transition issue"); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) transitionStatusAtomic(ctx context.Context, issue model.Issue, actor string, reason string, action string, fromStatus string, toStatus string) (model.Issue, error) {
	if issue.DeletedAt != nil || issue.ArchivedAt != nil {
		return model.Issue{}, fmt.Errorf("cannot %s archived or deleted issue", action)
	}
	now := time.Now().UTC()
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Issue{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin transition issue tx: %w", err)
	}
	defer tx.Rollback()
	var closedAt any
	if toStatus == "closed" {
		closedAt = now.Format(time.RFC3339Nano)
	}
	// [LAW:dataflow-not-control-flow] Claim/done transitions always execute one guarded write; contention is modeled by affected row count.
	result, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, updated_at = ?, closed_at = ? WHERE id = ? AND status = ?`,
		toStatus, now.Format(time.RFC3339Nano), closedAt, issue.ID, fromStatus)
	if err != nil {
		return model.Issue{}, fmt.Errorf("update issue status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return model.Issue{}, fmt.Errorf("read status transition result: %w", err)
	}
	if affected == 0 {
		currentStatus, lookupErr := currentStatusTx(ctx, tx, issue.ID)
		if lookupErr != nil {
			return model.Issue{}, lookupErr
		}
		return model.Issue{}, fmt.Errorf("%s conflict: issue status is %q", action, currentStatus)
	}
	if err := s.insertHistoryTx(ctx, tx, issue.ID, action, reason, fromStatus, toStatus, actor); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit transition issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "transition issue"); err != nil {
		return model.Issue{}, err
	}
	issue.Status = toStatus
	issue.UpdatedAt = now
	if toStatus == "closed" {
		issue.ClosedAt = &now
	}
	return issue, nil
}

func currentStatusTx(ctx context.Context, tx *sql.Tx, issueID string) (string, error) {
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM issues WHERE id = ?`, issueID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NotFoundError{Entity: "issue", ID: issueID}
		}
		return "", fmt.Errorf("read issue status: %w", err)
	}
	return status, nil
}

func retryTransientManifestReadOnly(ctx context.Context, operation retryOperation, delayForAttempt retryDelayFunc, sleep retrySleepFunc) error {
	var lastErr error
	for attempt := 1; attempt <= transientManifestRetryMaxAttempts; attempt++ {
		err := classifyTransientManifestError(operation(ctx))
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, ErrTransientManifestReadOnly) || attempt == transientManifestRetryMaxAttempts {
			break
		}
		if waitErr := sleep(ctx, delayForAttempt(attempt)); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

func transientManifestRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := transientManifestRetryBaseDelay << (attempt - 1)
	if delay > transientManifestRetryMaxDelay {
		delay = transientManifestRetryMaxDelay
	}
	return delay
}

func waitWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (s *Store) ListRelationsForIssue(ctx context.Context, issueID string, relType string) ([]model.Relation, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return nil, err
	}
	rels, err := s.listRelations(ctx, issueID)
	if err != nil {
		return nil, err
	}
	normalizedType := strings.TrimSpace(relType)
	if normalizedType == "" {
		return rels, nil
	}
	out := make([]model.Relation, 0, len(rels))
	for _, rel := range rels {
		if rel.Type == normalizedType {
			out = append(out, rel)
		}
	}
	return out, nil
}

func (s *Store) SetParent(ctx context.Context, in SetParentInput) (model.Relation, error) {
	if strings.TrimSpace(in.ChildID) == "" || strings.TrimSpace(in.ParentID) == "" {
		return model.Relation{}, errors.New("child and parent ids are required")
	}
	if in.ChildID == in.ParentID {
		return model.Relation{}, errors.New("child and parent cannot be the same issue")
	}
	if _, err := s.GetIssue(ctx, in.ChildID); err != nil {
		return model.Relation{}, err
	}
	if _, err := s.GetIssue(ctx, in.ParentID); err != nil {
		return model.Relation{}, err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return model.Relation{}, err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Relation{}, fmt.Errorf("begin set parent tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, in.ChildID); err != nil {
		return model.Relation{}, fmt.Errorf("clear parent relation: %w", err)
	}
	rel := model.Relation{
		SrcID:     in.ChildID,
		DstID:     in.ParentID,
		Type:      "parent-child",
		CreatedAt: time.Now().UTC(),
		CreatedBy: strings.TrimSpace(in.CreatedBy),
	}
	if rel.CreatedBy == "" {
		rel.CreatedBy = "unknown"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`, rel.SrcID, rel.DstID, rel.CreatedAt.Format(time.RFC3339Nano), rel.CreatedBy); err != nil {
		return model.Relation{}, fmt.Errorf("insert parent relation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Relation{}, fmt.Errorf("commit set parent: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "set parent"); err != nil {
		return model.Relation{}, err
	}
	return rel, nil
}

func (s *Store) ClearParent(ctx context.Context, childID string) error {
	if _, err := s.GetIssue(ctx, childID); err != nil {
		return err
	}
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear parent tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE src_id = ? AND type = 'parent-child'`, childID)
	if err != nil {
		return fmt.Errorf("delete parent relation: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return NotFoundError{Entity: "parent relation", ID: childID}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear parent: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "clear parent"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListChildren(ctx context.Context, parentID string) ([]model.Issue, error) {
	if _, err := s.GetIssue(ctx, parentID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type, i.assignee, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at
		FROM relations r
		JOIN issues i ON i.id = r.src_id
		WHERE r.type = 'parent-child' AND r.dst_id = ?
		ORDER BY i.updated_at DESC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()
	children := []model.Issue{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.attachLabels(ctx, children)
}

func (s *Store) ListLabels(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT label FROM labels WHERE issue_id = ? ORDER BY label ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) error {
	ctx, releaseCommitLock, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer releaseCommitLock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace from export tx: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"labels", "comments", "relations", "issues"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for _, issue := range export.Issues {
		var closedAt any
		if issue.ClosedAt != nil {
			closedAt = issue.ClosedAt.Format(time.RFC3339Nano)
		}
		status, err := normalizeStatus(issue.Status)
		if err != nil {
			return fmt.Errorf("restore issue %s: %w", issue.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at, archived_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			issue.ID, issue.Title, issue.Description, status, issue.Priority, issue.IssueType, issue.Assignee, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt)); err != nil {
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace from export: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "replace from export"); err != nil {
		return err
	}
	return nil
}

func (s *Store) listRelations(ctx context.Context, issueID string) ([]model.Relation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, dst_id, type, created_at, created_by FROM relations WHERE src_id = ? OR dst_id = ? ORDER BY created_at ASC`, issueID, issueID)
	if err != nil {
		return nil, fmt.Errorf("list relations: %w", err)
	}
	defer rows.Close()
	rels := []model.Relation{}
	for rows.Next() {
		var rel model.Relation
		var createdAt string
		if err := rows.Scan(&rel.SrcID, &rel.DstID, &rel.Type, &createdAt, &rel.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		rel.CreatedAt = t
		rels = append(rels, rel)
	}
	return rels, rows.Err()
}
func (s *Store) getMeta(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var row *sql.Row
	if tx != nil {
		row = tx.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, key)
	} else {
		row = s.db.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, key)
	}
	var value string
	if err := row.Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get meta %q: %w", key, err)
	}
	return value, nil
}

func (s *Store) setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	var execer interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}
	if tx != nil {
		execer = tx
	} else {
		execer = s.db
	}
	if _, err := execer.ExecContext(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`, key, value); err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}
func buildIssueOrderClause(specs []SortSpec) (string, error) {
	if len(specs) == 0 {
		return "i.status ASC, i.priority ASC, i.updated_at DESC, i.id ASC", nil
	}
	allowed := map[string]string{
		"id":         "i.id",
		"title":      "i.title",
		"status":     "i.status",
		"priority":   "i.priority",
		"type":       "i.issue_type",
		"assignee":   "i.assignee",
		"created_at": "i.created_at",
		"updated_at": "i.updated_at",
	}
	order := make([]string, 0, len(specs))
	for _, spec := range specs {
		field := strings.ToLower(strings.TrimSpace(spec.Field))
		column, ok := allowed[field]
		if !ok {
			return "", fmt.Errorf("unsupported sort field %q", spec.Field)
		}
		direction := "ASC"
		if spec.Desc {
			direction = "DESC"
		}
		order = append(order, column+" "+direction)
	}
	order = append(order, "i.id ASC")
	return strings.Join(order, ", "), nil
}

func (s *Store) listAllLabels(ctx context.Context) ([]model.Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all labels: %w", err)
	}
	defer rows.Close()
	out := []model.Label{}
	for rows.Next() {
		var label model.Label
		var createdAt string
		if err := rows.Scan(&label.IssueID, &label.Name, &createdAt, &label.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		label.CreatedAt = t
		out = append(out, label)
	}
	return out, rows.Err()
}

func (s *Store) insertHistoryTx(ctx context.Context, tx *sql.Tx, issueID, action, reason, fromStatus, toStatus, createdBy string) error {
	event := model.IssueHistory{
		ID:         "hist-" + uuid.NewString(),
		IssueID:    issueID,
		Action:     action,
		Reason:     strings.TrimSpace(reason),
		FromStatus: strings.TrimSpace(fromStatus),
		ToStatus:   strings.TrimSpace(toStatus),
		CreatedAt:  time.Now().UTC(),
		CreatedBy:  strings.TrimSpace(createdBy),
	}
	if event.CreatedBy == "" {
		event.CreatedBy = "unknown"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_history(id, issue_id, action, reason, from_status, to_status, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.IssueID, event.Action, event.Reason, event.FromStatus, event.ToStatus, event.CreatedAt.Format(time.RFC3339Nano), event.CreatedBy); err != nil {
		return fmt.Errorf("insert issue history: %w", err)
	}
	return nil
}

func (s *Store) listComments(ctx context.Context, issueID string) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, issue_id, body, created_at, created_by FROM comments WHERE issue_id = ? ORDER BY created_at ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	out := []model.Comment{}
	for rows.Next() {
		var c model.Comment
		var createdAt string
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Body, &createdAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		c.CreatedAt = t
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) listHistory(ctx context.Context, issueID string) ([]model.IssueHistory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, issue_id, action, reason, from_status, to_status, created_at, created_by FROM issue_history WHERE issue_id = ? ORDER BY created_at ASC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue history: %w", err)
	}
	defer rows.Close()
	out := []model.IssueHistory{}
	for rows.Next() {
		var event model.IssueHistory
		var createdAt string
		if err := rows.Scan(&event.ID, &event.IssueID, &event.Action, &event.Reason, &event.FromStatus, &event.ToStatus, &createdAt, &event.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		event.CreatedAt = t
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) listAllRelations(ctx context.Context) ([]model.Relation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT src_id, dst_id, type, created_at, created_by FROM relations ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all relations: %w", err)
	}
	defer rows.Close()
	rels := []model.Relation{}
	for rows.Next() {
		var rel model.Relation
		var createdAt string
		if err := rows.Scan(&rel.SrcID, &rel.DstID, &rel.Type, &createdAt, &rel.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		rel.CreatedAt = t
		rels = append(rels, rel)
	}
	return rels, rows.Err()
}

func (s *Store) listAllComments(ctx context.Context) ([]model.Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, issue_id, body, created_at, created_by FROM comments ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all comments: %w", err)
	}
	defer rows.Close()
	out := []model.Comment{}
	for rows.Next() {
		var c model.Comment
		var createdAt string
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Body, &createdAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		c.CreatedAt = t
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) listAllHistory(ctx context.Context) ([]model.IssueHistory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, issue_id, action, reason, from_status, to_status, created_at, created_by FROM issue_history ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all issue history: %w", err)
	}
	defer rows.Close()
	out := []model.IssueHistory{}
	for rows.Next() {
		var event model.IssueHistory
		var createdAt string
		if err := rows.Scan(&event.ID, &event.IssueID, &event.Action, &event.Reason, &event.FromStatus, &event.ToStatus, &createdAt, &event.CreatedBy); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		event.CreatedAt = t
		out = append(out, event)
	}
	return out, rows.Err()
}

type issueScanner interface{ Scan(dest ...any) error }

func scanIssue(row issueScanner) (model.Issue, error) {
	var issue model.Issue
	var createdAt, updatedAt string
	var closedAt, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&issue.ID, &issue.Title, &issue.Description, &issue.Status, &issue.Priority, &issue.IssueType, &issue.Assignee, &createdAt, &updatedAt, &closedAt, &archivedAt, &deletedAt); err != nil {
		return model.Issue{}, err
	}
	var err error
	issue.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return model.Issue{}, err
	}
	issue.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return model.Issue{}, err
	}
	if closedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, closedAt.String)
		if err != nil {
			return model.Issue{}, err
		}
		issue.ClosedAt = &t
	}
	if archivedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, archivedAt.String)
		if err != nil {
			return model.Issue{}, err
		}
		issue.ArchivedAt = &t
	}
	if deletedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, deletedAt.String)
		if err != nil {
			return model.Issue{}, err
		}
		issue.DeletedAt = &t
	}
	issue.Labels = []string{}
	return issue, nil
}

func (s *Store) attachLabels(ctx context.Context, issues []model.Issue) ([]model.Issue, error) {
	if len(issues) == 0 {
		return issues, nil
	}
	issueIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		issueIDs = append(issueIDs, issue.ID)
	}
	labelMap, err := s.loadLabelsByIssueIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	for index := range issues {
		issues[index].Labels = labelMap[issues[index].ID]
		if issues[index].Labels == nil {
			issues[index].Labels = []string{}
		}
	}
	return issues, nil
}

func (s *Store) loadLabelsByIssueIDs(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	placeholders := make([]string, 0, len(issueIDs))
	args := make([]any, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		placeholders = append(placeholders, "?")
		args = append(args, issueID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id, label FROM labels WHERE issue_id IN (`+strings.Join(placeholders, ", ")+`) ORDER BY label ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("load labels by issue ids: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var issueID, label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return nil, err
		}
		out[issueID] = append(out[issueID], label)
	}
	return out, rows.Err()
}

func (s *Store) replaceLabelsTx(ctx context.Context, tx *sql.Tx, issueID string, labels []string, createdBy string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("clear labels: %w", err)
	}
	author := strings.TrimSpace(createdBy)
	if author == "" {
		author = "unknown"
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`, issueID, label, timestamp, author); err != nil {
			return fmt.Errorf("insert label %q: %w", label, err)
		}
	}
	return nil
}

func validateIssueType(issueType string) (string, error) {
	issueType = strings.TrimSpace(strings.ToLower(issueType))
	switch issueType {
	case "", "task", "feature", "bug", "chore", "epic":
		if issueType == "" {
			return "task", nil
		}
		return issueType, nil
	default:
		return "", errors.New("issue type must be task, feature, bug, chore, or epic")
	}
}

func validatePriority(priority int) error {
	if priority < 0 || priority > 4 {
		return errors.New("priority must be between 0 and 4")
	}
	return nil
}

func NormalizeStatusToken(status string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(status))
	if normalized == "in-progress" {
		normalized = "in_progress"
	}
	switch normalized {
	case "open", "in_progress", "closed":
		return normalized, nil
	case "":
		return "", nil
	default:
		return "", errors.New("status must be open, in_progress, or closed")
	}
}

func normalizeStatus(status string) (string, error) {
	normalized, err := NormalizeStatusToken(status)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "open", nil
	}
	return normalized, nil
}

func canonicalizeLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		normalized, err := normalizeLabel(label)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeLabel(label string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return "", errors.New("label is required")
	}
	if strings.Contains(normalized, ",") {
		return "", errors.New("label cannot contain commas")
	}
	return normalized, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.Format(time.RFC3339Nano)
}

func ensureDoltDatabase(ctx context.Context, doltRootDir string, workspaceID string) error {
	root := filepath.Clean(doltRootDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create dolt root dir: %w", err)
	}
	db, err := sql.Open(doltDriverName, buildDoltDSN(root, workspaceID, false))
	if err != nil {
		return fmt.Errorf("open dolt bootstrap: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", doltDatabaseName)); err != nil {
		return fmt.Errorf("create dolt database: %w", err)
	}
	return nil
}

func buildDoltDSN(doltRootDir, workspaceID string, includeDatabase bool) string {
	author := strings.TrimSpace(workspaceID)
	if author == "" {
		author = "links"
	}
	author = strings.ReplaceAll(author, "@", "_")
	query := url.Values{}
	query.Set("commitname", author)
	query.Set("commitemail", fmt.Sprintf("%s@links.local", author))
	if includeDatabase {
		query.Set("database", doltDatabaseName)
	}
	return "file://" + filepath.ToSlash(filepath.Clean(doltRootDir)) + "?" + query.Encode()
}

func execIgnoreAlreadyExists(ctx context.Context, db *sql.DB, stmt string) error {
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		normalized := strings.ToLower(err.Error())
		if strings.Contains(normalized, "already exists") || strings.Contains(normalized, "duplicate column") || strings.Contains(normalized, "duplicate key name") {
			return nil
		}
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

func (s *Store) commitWorkingSet(ctx context.Context, message string) error {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		trimmed = "links mutation"
	}
	// [LAW:single-enforcer] commitWorkingSet is the single mutation boundary that owns transient commit retry behavior.
	// [LAW:one-source-of-truth] A process-shared commit lock at this boundary is the canonical writer serialization mechanism.
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientManifestReadOnly(ctx, func(ctx context.Context) error {
			return s.commitWorkingSetOnce(ctx, trimmed)
		}, transientManifestRetryDelay, waitWithContext)
	})
}

func (s *Store) commitWorkingSetOnce(ctx context.Context, message string) error {
	var commitHash string
	err := s.db.QueryRowContext(ctx, `CALL DOLT_COMMIT('-Am', ?)`, message).Scan(&commitHash)
	if err == nil {
		return nil
	}
	normalized := strings.ToLower(err.Error())
	if strings.Contains(normalized, "nothing to commit") {
		return nil
	}
	return wrapCommitWorkingSetError(err)
}

func (s *Store) withCommitLock(ctx context.Context, operation retryOperation) error {
	lockedCtx, release, err := s.acquireCommitLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return operation(lockedCtx)
}

func (s *Store) AcquireMutationLock(ctx context.Context) (context.Context, func(), error) {
	return s.acquireCommitLock(ctx)
}

func (s *Store) acquireCommitLock(ctx context.Context) (context.Context, func(), error) {
	if alreadyLocked, _ := ctx.Value(commitLockContextKey{}).(bool); alreadyLocked {
		return ctx, func() {}, nil
	}

	processCommitMutex.Lock()
	locked, err := tryAcquireFileLock(s.commitLockPath)
	for errors.Is(err, os.ErrExist) && !locked {
		if staleErr := removeStaleCommitLock(s.commitLockPath, commitLockStaleAfter); staleErr != nil {
			processCommitMutex.Unlock()
			return ctx, nil, fmt.Errorf("acquire commit lock: %w", staleErr)
		}
		if waitErr := waitWithContext(ctx, transientManifestRetryBaseDelay); waitErr != nil {
			processCommitMutex.Unlock()
			return ctx, nil, waitErr
		}
		locked, err = tryAcquireFileLock(s.commitLockPath)
	}
	if err != nil {
		processCommitMutex.Unlock()
		return ctx, nil, fmt.Errorf("acquire commit lock: %w", err)
	}
	if !locked {
		processCommitMutex.Unlock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctx, nil, ctxErr
		}
		return ctx, nil, errors.New("acquire commit lock: lock not acquired")
	}

	release := func() {
		_ = os.Remove(s.commitLockPath)
		processCommitMutex.Unlock()
	}
	return context.WithValue(ctx, commitLockContextKey{}, true), release, nil
}

func tryAcquireFileLock(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, err
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return false, closeErr
	}
	return true, nil
}

func removeStaleCommitLock(path string, staleAfter time.Duration) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	isStaleByAge := time.Since(info.ModTime()) > staleAfter
	isStaleByOwner, err := commitLockOwnedByDeadProcess(path)
	if err != nil {
		return err
	}
	if !isStaleByAge && !isStaleByOwner {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func commitLockOwnedByDeadProcess(path string) (bool, error) {
	// [LAW:single-enforcer] Commit-lock owner liveness classification is centralized here to keep stale-lock handling deterministic.
	pid, hasOwnerPID, err := readCommitLockOwnerPID(path)
	if err != nil {
		return false, err
	}
	if !hasOwnerPID {
		return false, nil
	}
	running, err := commitLockPIDRunning(pid)
	if err != nil {
		return false, err
	}
	return !running, nil
}

func readCommitLockOwnerPID(path string) (int, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	pidText := strings.TrimSpace(string(content))
	if pidText == "" {
		return 0, false, nil
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false, nil
	}
	return pid, true, nil
}

func isCommitLockPIDRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	// Unknown probe errors are treated as running to avoid removing an active lock.
	return true, nil
}

type transientManifestReadOnlyError struct {
	err error
}

func (e transientManifestReadOnlyError) Error() string {
	return e.err.Error()
}

func (e transientManifestReadOnlyError) Unwrap() error {
	return e.err
}

func (e transientManifestReadOnlyError) Is(target error) bool {
	return target == ErrTransientManifestReadOnly
}

func wrapCommitWorkingSetError(err error) error {
	wrapped := fmt.Errorf("dolt commit working set: %w", err)
	if !isManifestReadOnlyCommitError(err) {
		return wrapped
	}
	// [LAW:one-source-of-truth] Store commit wrapping is the canonical transient classifier for manifest read-only failures.
	return transientManifestReadOnlyError{err: wrapped}
}

func classifyTransientManifestError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTransientManifestReadOnly) {
		return err
	}
	if !isManifestReadOnlyCommitError(err) {
		return err
	}
	return transientManifestReadOnlyError{err: err}
}

func isManifestReadOnlyCommitError(err error) bool {
	if err == nil {
		return false
	}
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "cannot update manifest") && strings.Contains(normalized, "read only")
}

func newIssueID(workspaceID string) string {
	prefix := strings.SplitN(workspaceID, "-", 2)[0]
	return fmt.Sprintf("lit-%s-%s", prefix, uuid.NewString()[:8])
}
