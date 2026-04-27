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
	"github.com/bmf/links-issue-tracker/internal/rank"
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
	doltRootDir    string
	commitLockPath string
	telemetryDir   string
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

type CreateIssueInput struct {
	Title       string
	Description string
	IssueType   string
	Topic       string
	ParentID    string
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
	Statuses          []string
	IssueTypes        []string
	ExcludeIssueTypes []string
	Assignees         []string
	PriorityMin       *int
	PriorityMax       *int
	SearchTerms       []string
	IDs               []string
	HasComments       *bool
	LabelsAll         []string
	UpdatedAfter      *time.Time
	UpdatedBefore     *time.Time
	IncludeArchived   bool
	IncludeDeleted    bool
	SortBy            []SortSpec
	Limit             int
}

type AddCommentInput struct {
	IssueID   string
	Body      string
	CreatedBy string
}

type TransitionIssueInput struct {
	IssueID   string
	Action    string
	Reason    string
	CreatedBy string
}

func Open(ctx context.Context, doltRootDir string, workspaceID string) (*Store, error) {
	if err := EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Store-level commit lock is the single writer gate for all startup and runtime mutations.
	if err := s.withCommitLock(ctx, s.migrate); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func OpenForRead(ctx context.Context, doltRootDir string, workspaceID string) (*Store, error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	// Auto-migrate stale schemas so read paths don't fail on missing columns/tables.
	// Unlike Open, this does NOT call EnsureDatabase — the DB must already exist.
	if err := s.withCommitLock(ctx, s.migrate); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func EnsureDatabase(ctx context.Context, doltRootDir string, workspaceID string) error {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return err
	}
	return ensureDoltDatabase(ctx, doltRootDir, workspaceID)
}

func validateOpenArgs(doltRootDir string, workspaceID string) error {
	if strings.TrimSpace(doltRootDir) == "" {
		return errors.New("dolt root dir is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("workspace id is required")
	}
	return nil
}

// ExecRawForTest executes a raw SQL statement without acquiring the commit lock
// or calling commitWorkingSet. It exists solely for test fixtures that need to
// manipulate database state outside normal Store operations (e.g., backdating timestamps).
func (s *Store) ExecRawForTest(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) Close() error {
	err := s.db.Close()
	// [LAW:single-enforcer] Benign driver shutdown cancellation is normalized at the Store boundary so callers see one close contract.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func openStoreConnection(doltRootDir string, workspaceID string) (*Store, error) {
	db, err := sql.Open(doltDriverName, buildDoltDSN(doltRootDir, workspaceID, true))
	if err != nil {
		return nil, fmt.Errorf("open dolt: %w", err)
	}
	// [LAW:single-enforcer] Each Store owns one embedded Dolt SQL connection so the process cannot self-conflict through the database/sql pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	return &Store{
		db:             db,
		workspaceID:    workspaceID,
		doltRootDir:    doltRootDir,
		commitLockPath: filepath.Join(filepath.Clean(doltRootDir), ".links-commit.lock"),
		telemetryDir:   filepath.Join(filepath.Clean(doltRootDir), "telemetry"),
	}, nil
}

// reconnect swaps s.db for a fresh connection using the same DSN.
// Dolt's online garbage collection invalidates any SQL connection that was
// open when it ran ("this connection can no longer be used. please reconnect."),
// so callers that invoke DOLT_GC must rotate the pooled connection before
// any subsequent query. Must be called while the commit lock is held so no
// concurrent caller observes a torn s.db pointer.
//
// The new handle is opened and configured before the old one is closed, so a
// failure to open the replacement leaves s.db pointing at the still-working
// original handle rather than tearing the Store.
func (s *Store) reconnect() error {
	// [LAW:dataflow-not-control-flow] Reconnect runs unconditionally on every invocation; what varies is the DSN's doltRootDir/workspaceID, not whether the rotation occurs.
	next, err := sql.Open(doltDriverName, buildDoltDSN(s.doltRootDir, s.workspaceID, true))
	if err != nil {
		return fmt.Errorf("reopen dolt: %w", err)
	}
	next.SetMaxOpenConns(1)
	next.SetMaxIdleConns(1)
	next.SetConnMaxLifetime(0)
	prev := s.db
	s.db = next
	if err := prev.Close(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("close prior dolt connection after reconnect: %w", err)
	}
	return nil
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
	topic, err := normalizeIssueTopicForCreate(in.Topic)
	if err != nil {
		return model.Issue{}, err
	}
	createdBy := "links"
	issue := model.Issue{
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Priority:    priority,
		IssueType:   issueType,
		Topic:       topic,
		Labels:      labels,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if model.IsContainerType(issueType) {
		issue, err = model.HydrateAllOf(issue, nil)
	} else {
		issue, err = model.HydrateOwnedStatus(issue, model.StatusView{Value: model.StateOpen, Assignee: strings.TrimSpace(in.Assignee)})
	}
	if err != nil {
		return model.Issue{}, err
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
	parentID := strings.TrimSpace(in.ParentID)
	if parentID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE id = ?`, parentID).Scan(new(string)); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return model.Issue{}, NotFoundError{Entity: "issue", ID: parentID}
			}
			return model.Issue{}, fmt.Errorf("lookup parent issue %q: %w", parentID, err)
		}
	}
	prefix, err := s.issuePrefixForTx(ctx, tx)
	if err != nil {
		return model.Issue{}, err
	}
	issue.ID, err = newIssueID(ctx, tx, prefix, issue.Topic, issue.Title, issue.Description, createdBy, issue.CreatedAt, parentID)
	if err != nil {
		return model.Issue{}, err
	}
	issue.Rank, err = nextRankAtBottom(ctx, tx)
	if err != nil {
		return model.Issue{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO issues(
		id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL)`,
		issue.ID, issue.Title, issue.Description, statusForStorage(issue), issue.Priority, issue.IssueType, issue.Topic,
		issue.AssigneeValue(), issue.Rank, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return model.Issue{}, fmt.Errorf("insert issue: %w", err)
	}
	if parentID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`,
			issue.ID, parentID, issue.CreatedAt.Format(time.RFC3339Nano), createdBy); err != nil {
			return model.Issue{}, fmt.Errorf("insert parent relation: %w", err)
		}
	}
	if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, createdBy); err != nil {
		return model.Issue{}, err
	}
	if err := s.insertHistoryTx(ctx, tx, issue.ID, "created", "issue created", "", "open", createdBy); err != nil {
		return model.Issue{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit create issue: %w", err)
	}
	if err := s.commitWorkingSet(ctx, "create issue"); err != nil {
		return model.Issue{}, err
	}
	if err := s.smoothRanksIfNeeded(ctx, issue.Rank); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error) {
	query := `SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at FROM issues i`
	var where []string
	var args []any
	if !filter.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	if !filter.IncludeDeleted {
		where = append(where, "i.deleted_at IS NULL")
	}
	// [LAW:one-source-of-truth] Container DB status is dead data; the lifecycle
	// derivation in hydrateIssues is the only truth source for epic state. The
	// status filter therefore lives entirely past the hydration boundary; here we
	// only validate the requested tokens so bad input fails before the query runs.
	allowedStates, err := parseStatusFilter(filter.Statuses)
	if err != nil {
		return nil, err
	}
	if len(filter.IssueTypes) > 0 {
		var placeholders []string
		for _, t := range filter.IssueTypes {
			trimmed := strings.TrimSpace(t)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			where = append(where, "i.issue_type IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(filter.ExcludeIssueTypes) > 0 {
		// [LAW:single-enforcer] Exclusion filter mirrors the IssueTypes positive
		// filter above; keeping both at the store boundary means one definition
		// of "which types qualify" regardless of caller.
		var placeholders []string
		for _, t := range filter.ExcludeIssueTypes {
			trimmed := strings.TrimSpace(t)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			where = append(where, "i.issue_type NOT IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(filter.Assignees) > 0 {
		var placeholders []string
		for _, a := range filter.Assignees {
			trimmed := strings.TrimSpace(a)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			where = append(where, "i.assignee IN ("+strings.Join(placeholders, ",")+")")
		}
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
		where = append(where, "(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR LOWER(i.topic) LIKE ?)")
		like := "%" + trimmed + "%"
		args = append(args, like, like, like)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	orderClause, err := buildIssueOrderClause(filter.SortBy)
	if err != nil {
		return nil, err
	}
	query += " ORDER BY " + orderClause
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w (query=%s)", err, query)
	}
	defer rows.Close()
	rowsOut := []issueRow{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		rowsOut = append(rowsOut, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hydrated, err := s.hydrateIssues(ctx, rowsOut)
	if err != nil {
		return nil, err
	}
	// [LAW:dataflow-not-control-flow] Filter and cap always run; the helpers absorb
	// "no filter" and "no limit" as data so the body stays a straight pipe.
	return capLimit(filterByState(hydrated, allowedStates), filter.Limit), nil
}

func parseStatusFilter(input []string) ([]model.State, error) {
	out := make([]model.State, 0, len(input))
	for _, raw := range input {
		normalized, err := normalizeStatus(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, model.State(normalized))
	}
	return out, nil
}

// filterByState keeps only issues whose lifecycle State() is in allowed; an
// empty allow-list passes everything through so callers can route every list
// through one boundary that always reads State(), never the DB column.
// [LAW:single-enforcer] Status filtering happens against derived lifecycle
// state because container DB status is dead data; hydration is the truth source.
func filterByState(issues []model.Issue, allowed []model.State) []model.Issue {
	if len(allowed) == 0 {
		return issues
	}
	allow := make(map[model.State]struct{}, len(allowed))
	for _, state := range allowed {
		allow[state] = struct{}{}
	}
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := allow[issue.State()]; ok {
			out = append(out, issue)
		}
	}
	return out
}

// capLimit truncates issues to the first n entries. limit <= 0 means uncapped,
// matching the existing ListIssuesFilter.Limit convention; the helper exists so
// the LIMIT semantic is one expression at the boundary rather than a branch in
// the body.
func capLimit(issues []model.Issue, limit int) []model.Issue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
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
		Blocks:    []model.Issue{},
	}
	for _, rel := range relations {
		switch rel.Type {
		case "blocks":
			// blocks convention: src_id=dependent, dst_id=dependency.
			if rel.SrcID == id {
				// This issue is the dependent; DstID is what it depends on.
				dep, err := s.GetIssue(ctx, rel.DstID)
				if err == nil {
					detail.DependsOn = append(detail.DependsOn, dep)
				}
			}
			if rel.DstID == id {
				// This issue is the dependency; SrcID depends on it.
				dependent, err := s.GetIssue(ctx, rel.SrcID)
				if err == nil {
					detail.Blocks = append(detail.Blocks, dependent)
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
	sortIssuesByRank(detail.Children)
	sortIssuesByRank(detail.DependsOn)
	sortIssuesByRank(detail.Related)
	sortIssuesByRank(detail.Blocks)
	return detail, nil
}

func (s *Store) GetIssue(ctx context.Context, id string) (model.Issue, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, title, description, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at FROM issues WHERE id = ?`, id)
	scanned, err := scanIssue(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Issue{}, NotFoundError{Entity: "issue", ID: id}
		}
		return model.Issue{}, err
	}
	hydrated, err := s.hydrateIssues(ctx, []issueRow{scanned})
	if err != nil {
		return model.Issue{}, err
	}
	return hydrated[0], nil
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
		// [LAW:single-enforcer] Container vs leaf is encoded in the lifecycle
		// expression at hydration time. Switching across that boundary would
		// orphan the lifecycle: epic → leaf would leave AllOf attached to a
		// row whose schema requires an OwnedStatus, and leaf → epic would
		// silently drop the leaf's status/assignee/closed_at. Refuse here
		// instead of patching it up downstream with an invented default.
		if model.IsContainerType(issue.IssueType) != model.IsContainerType(issueType) {
			return model.Issue{}, fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerIssueTypes)
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
		caps := issue.Capabilities()
		if caps.Status == nil {
			return model.Issue{}, fmt.Errorf("issue %s does not expose a status capability", issue.ID)
		}
		updated, err := model.UpdateStatusCapability(issue, model.StatusView{
			Value:    caps.Status.Value,
			Assignee: strings.TrimSpace(*in.Assignee),
			ClosedAt: caps.Status.ClosedAt,
		})
		if err != nil {
			return model.Issue{}, err
		}
		issue = updated
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
	if value := issue.ClosedAtValue(); value != nil {
		closedAt = value.Format(time.RFC3339Nano)
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
		WHERE id = ?`, issue.Title, issue.Description, statusForStorage(issue), issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID)
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
	return s.GetIssue(ctx, issue.ID)
}

// RankToTop moves an issue to rank above all other issues.
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
	actor := strings.TrimSpace(in.CreatedBy)
	if actor == "" {
		actor = "unknown"
	}
	switch action {
	case "start", "done", "close", "reopen":
		return s.writeStatusTransition(ctx, issue, actor, reason, action)
	}
	now := time.Now().UTC()
	fromStatus := issue.StatusValue()
	toStatus := issue.StatusValue()
	switch action {
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
		statusForStorage(issue), issue.UpdatedAt.Format(time.RFC3339Nano), nullableTime(issue.ClosedAtValue()), nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID); err != nil {
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
	reloaded, err := s.GetIssue(ctx, issue.ID)
	if err != nil {
		// [LAW:dataflow-not-control-flow] Write succeeded; surface the in-memory
		// post-mutation state so callers don't see a write+error combo and retry
		// an already-applied transition.
		return issue, nil
	}
	return reloaded, nil
}

func (s *Store) writeStatusTransition(ctx context.Context, issue model.Issue, actor string, reason string, action string) (model.Issue, error) {
	if issue.DeletedAt != nil || issue.ArchivedAt != nil {
		return model.Issue{}, fmt.Errorf("cannot %s archived or deleted issue", action)
	}
	updated, err := issue.Apply(model.ActionName(action), actor, reason)
	if err != nil {
		return model.Issue{}, err
	}
	fromStatus := issue.StatusValue()
	toStatus := updated.StatusValue()
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
	if value := updated.ClosedAtValue(); value != nil {
		closedAt = value.Format(time.RFC3339Nano)
	}
	// [LAW:dataflow-not-control-flow] Status transitions always execute one guarded write; contention is modeled by affected row count.
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
	updated.UpdatedAt = now
	return updated, nil
}

func currentStatusTx(ctx context.Context, tx *sql.Tx, issueID string) (string, error) {
	// status column is nullable since #79 (containers store NULL); the scan target
	// must match the column shape, not the subset of rows this caller expects.
	var status sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status FROM issues WHERE id = ?`, issueID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", NotFoundError{Entity: "issue", ID: issueID}
		}
		return "", fmt.Errorf("read issue status: %w", err)
	}
	return status.String, nil
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
func (s *Store) ListTopics(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT topic FROM issues WHERE deleted_at IS NULL AND topic <> '' ORDER BY topic ASC`)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()
	topics := []string{}
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, rows.Err()
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

func (s *Store) ensureMetaValue(ctx context.Context, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if current == value {
		return false, nil
	}
	if err := s.setMeta(ctx, nil, key, value); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ensureMetaDefault(ctx context.Context, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) != "" {
		return false, nil
	}
	// [LAW:one-source-of-truth] Schema-version writes preserve the recorded version as the canonical migration state once it exists.
	if err := s.setMeta(ctx, nil, key, value); err != nil {
		return false, err
	}
	return true, nil
}

func buildIssueOrderClause(specs []SortSpec) (string, error) {
	if len(specs) == 0 {
		// [LAW:one-source-of-truth] rank is the canonical ordering authority.
		return "i.item_rank ASC, i.id ASC", nil
	}
	allowed := map[string]string{
		"id":         "i.id",
		"title":      "i.title",
		"status":     "i.status",
		"priority":   "i.priority",
		"rank":       "i.item_rank",
		"type":       "i.issue_type",
		"topic":      "i.topic",
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

func sortIssuesByRank(issues []model.Issue) {
	// [LAW:one-source-of-truth] Rank is the canonical default ordering for
	// derived issue groups assembled outside the list query path.
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Rank == issues[j].Rank {
			return issues[i].ID < issues[j].ID
		}
		return issues[i].Rank < issues[j].Rank
	})
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

type issueRow struct {
	Issue  partialIssue
	Status model.StatusView
}

// partialIssue is row data only; hydrateIssues is the only path that may turn
// it into a returned model.Issue.
// [LAW:single-enforcer] Store hydration, not raw row decoding, owns lifecycle construction.
type partialIssue struct {
	ID          string
	Title       string
	Description string
	Priority    int
	IssueType   string
	Topic       string
	Rank        string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ArchivedAt  *time.Time
	DeletedAt   *time.Time
}

// nextRankAtBottom returns a rank that sorts after all existing items.
// Called within a transaction to ensure consistency.
func nextRankAtBottom(ctx context.Context, tx *sql.Tx) (string, error) {
	var lastRank sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank DESC LIMIT 1").Scan(&lastRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query last rank: %w", err)
	}
	if !lastRank.Valid || lastRank.String == "" {
		return rank.Initial(), nil
	}
	return rank.After(lastRank.String), nil
}

func scanIssue(row issueScanner) (issueRow, error) {
	var issue partialIssue
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&issue.ID, &issue.Title, &issue.Description, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &createdAt, &updatedAt, &closedAt, &archivedAt, &deletedAt); err != nil {
		return issueRow{}, err
	}
	return parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, archivedAt, deletedAt)
}

func scanIssueWithParent(row issueScanner) (string, issueRow, error) {
	var parentID string
	var issue partialIssue
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&parentID, &issue.ID, &issue.Title, &issue.Description, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &createdAt, &updatedAt, &closedAt, &archivedAt, &deletedAt); err != nil {
		return "", issueRow{}, err
	}
	parsed, err := parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, archivedAt, deletedAt)
	return parentID, parsed, err
}

func parsedIssueRow(issue partialIssue, status sql.NullString, assignee string, createdAt string, updatedAt string, closedAt sql.NullString, archivedAt sql.NullString, deletedAt sql.NullString) (issueRow, error) {
	var err error
	issue.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return issueRow{}, err
	}
	issue.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return issueRow{}, err
	}
	// Container rows store NULL status; hydrateIssues ignores StatusView for them
	// and constructs the lifecycle via HydrateAllOf instead.
	// [LAW:single-enforcer] The decision "use StatusView vs derive from children"
	// lives in hydrateIssues; here we just carry the row data as it appears.
	statusView := model.StatusView{Value: model.State(status.String), Assignee: assignee}
	if closedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, closedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		statusView.ClosedAt = &t
	}
	if archivedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, archivedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		issue.ArchivedAt = &t
	}
	if deletedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, deletedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		issue.DeletedAt = &t
	}
	issue.Labels = []string{}
	return issueRow{Issue: issue, Status: statusView}, nil
}

// statusForStorage returns the value to persist in the issues.status column.
// Container issues store NULL because their state derives from children;
// hydrateIssues never reads this column for them. Leaf issues persist their
// owned status string. The schema CHECK constraint enforces this invariant —
// the function does not have to defend against bad inputs because the type
// system upstream (OwnedStatus vs AllOf) already does.
// [LAW:one-source-of-truth] Container state lives in the AllOf lifecycle, not
// the DB column. Writing NULL keeps the column from lying about what it owns.
func statusForStorage(issue model.Issue) sql.NullString {
	if issue.IsContainer() {
		return sql.NullString{}
	}
	return sql.NullString{String: issue.StatusValue(), Valid: true}
}

// statusForStorageRaw is the issueType+status equivalent of statusForStorage
// for write paths that don't have a hydrated Issue (import / restore). Same
// container/leaf decision; for leaves it validates the status token through
// the canonical normalizer.
// [LAW:single-enforcer] One rule for "what goes in the status column" applies
// to every write path; the hydrated and raw entrypoints share it.
func statusForStorageRaw(issueType string, status string) (sql.NullString, error) {
	if model.IsContainerType(issueType) {
		return sql.NullString{}, nil
	}
	normalized, err := normalizeStatus(status)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: normalized, Valid: true}, nil
}

func (s *Store) hydrateIssues(ctx context.Context, rows []issueRow) ([]model.Issue, error) {
	if len(rows) == 0 {
		return []model.Issue{}, nil
	}
	issueIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		issueIDs = append(issueIDs, row.Issue.ID)
	}
	labelsByID, err := s.loadLabelsByIssueIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	epicIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if model.IsContainerType(row.Issue.IssueType) {
			epicIDs = append(epicIDs, row.Issue.ID)
		}
	}
	childrenByEpicID, err := s.lifecycleChildrenByEpicIDs(ctx, epicIDs)
	if err != nil {
		return nil, err
	}
	hydrated := make([]model.Issue, 0, len(rows))
	for _, row := range rows {
		base := model.Issue{
			ID:          row.Issue.ID,
			Title:       row.Issue.Title,
			Description: row.Issue.Description,
			Priority:    row.Issue.Priority,
			IssueType:   row.Issue.IssueType,
			Topic:       row.Issue.Topic,
			Rank:        row.Issue.Rank,
			Labels:      labelsByID[row.Issue.ID],
			CreatedAt:   row.Issue.CreatedAt,
			UpdatedAt:   row.Issue.UpdatedAt,
			ArchivedAt:  row.Issue.ArchivedAt,
			DeletedAt:   row.Issue.DeletedAt,
		}
		if base.Labels == nil {
			base.Labels = []string{}
		}
		var issue model.Issue
		// [LAW:single-enforcer] This store hydrator is the only read boundary that turns row status plus child relations into model lifecycle state.
		if model.IsContainerType(row.Issue.IssueType) {
			issue, err = model.HydrateAllOf(base, childrenByEpicID[row.Issue.ID])
		} else {
			issue, err = model.HydrateOwnedStatus(base, row.Status)
		}
		if err != nil {
			return nil, err
		}
		hydrated = append(hydrated, issue)
	}
	return hydrated, nil
}

func (s *Store) lifecycleChildrenByEpicIDs(ctx context.Context, epicIDs []string) (map[string][]model.Issue, error) {
	out := make(map[string][]model.Issue, len(epicIDs))
	if len(epicIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(epicIDs))
	args := make([]any, 0, len(epicIDs))
	for _, epicID := range epicIDs {
		placeholders = append(placeholders, "?")
		args = append(args, epicID)
	}
	// [LAW:one-source-of-truth] Active containers derive progress from active children; archived/deleted containers keep a full child snapshot so their lifecycle state does not collapse to empty/open.
	// Children-of-epic visibility truth table:
	//   parent live, child live -> include
	//   parent live, child dead -> exclude (active container shows only active children)
	//   parent dead, child live -> include (snapshot semantics: container's state at archive)
	//   parent dead, child dead -> include (snapshot semantics)
	// The WHERE clause encodes "include if parent is dead OR child is live."
	rows, err := s.db.QueryContext(ctx, `SELECT r.dst_id, i.id, i.title, i.description, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at
		FROM relations r
		JOIN issues i ON i.id = r.src_id
		JOIN issues p ON p.id = r.dst_id
		WHERE r.dst_id IN (`+strings.Join(placeholders, ", ")+`) AND r.type = 'parent-child'
			AND (p.archived_at IS NOT NULL OR p.deleted_at IS NOT NULL OR (i.archived_at IS NULL AND i.deleted_at IS NULL))
		ORDER BY r.dst_id ASC, i.item_rank ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("load lifecycle children: %w", err)
	}
	defer rows.Close()
	childRowsByEpicID := make(map[string][]issueRow, len(epicIDs))
	for rows.Next() {
		parentID, child, err := scanIssueWithParent(rows)
		if err != nil {
			return nil, err
		}
		childRowsByEpicID[parentID] = append(childRowsByEpicID[parentID], child)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for epicID, childRows := range childRowsByEpicID {
		hydrated, err := s.hydrateIssues(ctx, childRows)
		if err != nil {
			return nil, err
		}
		out[epicID] = hydrated
	}
	return out, nil
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

func validateIssueType(issueType string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(issueType))
	if trimmed == "" {
		return "task", nil
	}
	if !model.IsValidIssueType(trimmed) {
		return "", errors.New("issue type must be task, feature, bug, chore, or epic")
	}
	return trimmed, nil
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
	db, err = sql.Open(doltDriverName, buildDoltDSN(root, workspaceID, true))
	if err != nil {
		return fmt.Errorf("open dolt bootstrap database: %w", err)
	}
	defer db.Close()
	if err := ensureMasterDefaultBranch(ctx, db); err != nil {
		return err
	}
	return nil
}

func ensureMasterDefaultBranch(ctx context.Context, db *sql.DB) error {
	activeBranch := ""
	if err := db.QueryRowContext(ctx, `SELECT active_branch()`).Scan(&activeBranch); err != nil {
		return fmt.Errorf("query dolt active branch: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM dolt_branches ORDER BY name`)
	if err != nil {
		return fmt.Errorf("query dolt branches: %w", err)
	}
	defer rows.Close()
	hasMaster := false
	branchCount := 0
	for rows.Next() {
		var branchName string
		if err := rows.Scan(&branchName); err != nil {
			return fmt.Errorf("scan dolt branch: %w", err)
		}
		hasMaster = hasMaster || branchName == "master"
		branchCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate dolt branches: %w", err)
	}
	if activeBranch == "master" || hasMaster || branchCount != 1 {
		return nil
	}
	// [LAW:one-source-of-truth] Embedded bootstrap normalizes the initial Dolt branch name at database creation time so callers do not re-encode branch-policy drift.
	renameQuery := fmt.Sprintf(
		"CALL DOLT_BRANCH('-m', '%s', 'master')",
		strings.ReplaceAll(activeBranch, "'", "''"),
	)
	if _, err := db.ExecContext(ctx, renameQuery); err != nil {
		return fmt.Errorf("rename dolt default branch to master: %w", err)
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
