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
	"strings"
	"time"

	_ "github.com/dolthub/driver"

	"github.com/google/uuid"

	"github.com/bmf/links-issue-tracker/internal/issueid"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/rank"
)

const (
	doltDriverName   = "dolt"
	doltDatabaseName = "links"
)

type Store struct {
	db             *sql.DB
	workspaceID    string
	doltRootDir    string
	commitLockPath string
	telemetryDir   string
}

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
	Prompt      string
	IssueType   string
	Topic       string
	ParentID    string
	Priority    int
	Assignee    string
	Labels      []string
	// Prefix is the workspace's cosmetic ID prefix (e.g., "links" → "links-foo-abc1").
	// Sourced from workspace config at the call site. Not persisted as derived state.
	Prefix string
}

type UpdateIssueInput struct {
	Title       *string
	Description *string
	Prompt      *string
	IssueType   *string
	Status      *string
	Priority    *int
	Assignee    *string
	Labels      *[]string
}

func (u UpdateIssueInput) IsEmpty() bool {
	return u.Title == nil && u.Description == nil && u.Prompt == nil && u.IssueType == nil &&
		u.Status == nil && u.Priority == nil && u.Assignee == nil && u.Labels == nil
}

// ApplyUpdateInput is the single input for the unified update path.
// TargetStatus == "" means no transition; empty Fields means no field mutations.
type ApplyUpdateInput struct {
	Fields           UpdateIssueInput
	TargetStatus     string // empty = no status change
	TransitionReason string
	TransitionBy     string
}

func (a ApplyUpdateInput) IsEmpty() bool {
	return a.TargetStatus == "" && a.Fields.IsEmpty()
}

type statusTransitionKey struct {
	From model.State
	To   model.State
}

// [LAW:one-source-of-truth] Status transition action map is the single authoritative source for lit update transitions.
var updateStatusTransitionActions = map[statusTransitionKey]string{
	{From: model.StateOpen, To: model.StateInProgress}:    "start",
	{From: model.StateInProgress, To: model.StateClosed}: "done",
	{From: model.StateOpen, To: model.StateClosed}:        "close",
	{From: model.StateClosed, To: model.StateOpen}:        "reopen",
	{From: model.StateClosed, To: model.StateInProgress}: "reopen+start",
	{From: model.StateInProgress, To: model.StateOpen}:   "done+reopen",
}

func statusTransitionActionsForApplyUpdate(fromStatus, toStatus string) ([]string, error) {
	if toStatus == "" || fromStatus == toStatus {
		return nil, nil
	}
	from := model.DefaultOpen(fromStatus)
	to := model.DefaultOpen(toStatus)
	action, exists := updateStatusTransitionActions[statusTransitionKey{From: from, To: to}]
	if !exists {
		return nil, fmt.Errorf("unsupported status transition %q -> %q for lit update", fromStatus, toStatus)
	}
	return strings.Split(action, "+"), nil
}

type SortSpec struct {
	Field string
	Desc  bool
}

type ListIssuesFilter struct {
	Statuses          []model.State
	IssueTypes        []string
	ExcludeIssueTypes []string
	Assignees         []string
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
	if _, err := EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
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
	if _, err := os.Stat(doltRootDir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("repository not initialized with lit — run 'lit init' first")
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

func EnsureDatabase(ctx context.Context, doltRootDir string, workspaceID string) (bool, error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return false, err
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
	return s.withMutation(ctx, "record sync state", func(ctx context.Context, tx *sql.Tx) error {
		for key, value := range map[string]string{
			"last_sync_path": strings.TrimSpace(state.Path),
			"last_sync_hash": strings.TrimSpace(state.ContentHash),
		} {
			if err := s.setMeta(ctx, tx, key, value); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CreateIssue(ctx context.Context, in CreateIssueInput) (model.Issue, error) {
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
	topic, err := issueid.NormalizeTopicForCreate(in.Topic)
	if err != nil {
		return model.Issue{}, err
	}
	createdBy := "links"
	issue := model.Issue{
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Prompt:      strings.TrimSpace(in.Prompt),
		Priority:    priority,
		IssueType:   issueType,
		Topic:       topic,
		Labels:      labels,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	issue, err = model.HydrateRow(issue, model.StatusView{Value: model.StateOpen, Assignee: strings.TrimSpace(in.Assignee)}, nil)
	if err != nil {
		return model.Issue{}, err
	}
	parentID := strings.TrimSpace(in.ParentID)
	if err := s.withMutation(ctx, "create issue", func(ctx context.Context, tx *sql.Tx) error {
		if parentID != "" {
			if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE id = ?`, parentID).Scan(new(string)); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return NotFoundError{Entity: "issue", ID: parentID}
				}
				return fmt.Errorf("lookup parent issue %q: %w", parentID, err)
			}
		}
		prefix, err := issueid.NormalizeConfiguredPrefix(in.Prefix)
		if err != nil {
			return fmt.Errorf("normalize issue prefix: %w", err)
		}
		issue.ID, err = newIssueID(ctx, tx, prefix, issue.Topic, issue.Title, issue.Description, createdBy, issue.CreatedAt, parentID)
		if err != nil {
			return err
		}
		issue.Rank, err = nextRankAtBottom(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(
			id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL)`,
			issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), statusForStorage(issue), issue.Priority, issue.IssueType, issue.Topic,
			issue.AssigneeValue(), issue.Rank, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert issue: %w", err)
		}
		if parentID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, ?)`,
				issue.ID, parentID, issue.CreatedAt.Format(time.RFC3339Nano), createdBy); err != nil {
				return fmt.Errorf("insert parent relation: %w", err)
			}
		}
		if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, createdBy); err != nil {
			return err
		}
		if err := s.insertHistoryTx(ctx, tx, issue.ID, "created", "issue created", "", "open", createdBy); err != nil {
			return err
		}
		return smoothRanksIfNeededTx(ctx, tx, issue.Rank)
	}); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error) {
	query := `SELECT i.id, i.title, i.description, i.agent_prompt, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at FROM issues i`
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
		where = append(where, "(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR LOWER(COALESCE(i.agent_prompt, '')) LIKE ? OR LOWER(i.topic) LIKE ?)")
		like := "%" + trimmed + "%"
		args = append(args, like, like, like, like)
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

func parseStatusFilter(input []model.State) ([]model.State, error) {
	out := make([]model.State, 0, len(input))
	for _, raw := range input {
		state := model.DefaultOpen(string(raw))
		out = append(out, state)
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

	// [LAW:single-enforcer] Hydrate every related issue in one query
	// rather than running N+1 GetIssue calls. The map lets the relation
	// loop below stay a pure data-flow over already-hydrated rows.
	relatedIDs := collectRelatedIssueIDs(id, relations)
	relatedByID, err := s.getIssuesByIDs(ctx, relatedIDs)
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
				if dep, ok := relatedByID[rel.DstID]; ok {
					detail.DependsOn = append(detail.DependsOn, dep)
				}
			}
			if rel.DstID == id {
				if dependent, ok := relatedByID[rel.SrcID]; ok {
					detail.Blocks = append(detail.Blocks, dependent)
				}
			}
		case "parent-child":
			if rel.SrcID == id {
				if parent, ok := relatedByID[rel.DstID]; ok {
					detail.Parent = &parent
				}
			}
			if rel.DstID == id {
				if child, ok := relatedByID[rel.SrcID]; ok {
					detail.Children = append(detail.Children, child)
				}
			}
		case "related-to":
			otherID := rel.SrcID
			if otherID == id {
				otherID = rel.DstID
			}
			if related, ok := relatedByID[otherID]; ok {
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

// collectRelatedIssueIDs returns every distinct counterparty id referenced
// by relations, excluding the focal id itself.
func collectRelatedIssueIDs(focalID string, relations []model.Relation) []string {
	seen := make(map[string]struct{}, len(relations)*2)
	ids := make([]string, 0, len(relations))
	add := func(candidate string) {
		if candidate == "" || candidate == focalID {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		ids = append(ids, candidate)
	}
	for _, rel := range relations {
		add(rel.SrcID)
		add(rel.DstID)
	}
	return ids
}

// getIssuesByIDs batch-loads issues by id and returns a map keyed by id.
// Missing ids (deleted/archived/never-existed) are simply absent from the
// returned map; callers decide whether absence is an error or merely a hole
// to skip. Empty input returns an empty map without querying.
func (s *Store) getIssuesByIDs(ctx context.Context, ids []string) (map[string]model.Issue, error) {
	if len(ids) == 0 {
		return map[string]model.Issue{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at FROM issues WHERE id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch load issues: %w", err)
	}
	defer rows.Close()
	scanned := make([]issueRow, 0, len(ids))
	for rows.Next() {
		row, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scan batch-loaded issue: %w", err)
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch-loaded issues: %w", err)
	}
	hydrated, err := s.hydrateIssues(ctx, scanned)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Issue, len(hydrated))
	for _, issue := range hydrated {
		out[issue.ID] = issue
	}
	return out, nil
}

func (s *Store) GetIssue(ctx context.Context, id string) (model.Issue, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, created_at, updated_at, closed_at, archived_at, deleted_at FROM issues WHERE id = ?`, id)
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
	// [LAW:dataflow-not-control-flow] Empty input is a defined no-op; callers need not branch on whether fields were set.
	if in.IsEmpty() {
		return s.GetIssue(ctx, id)
	}
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
	if in.Prompt != nil {
		issue.Prompt = strings.TrimSpace(*in.Prompt)
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
	if err := s.withMutation(ctx, "update issue", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET
			title = ?, description = ?, agent_prompt = ?, status = ?, priority = ?, issue_type = ?, assignee = ?, updated_at = ?, closed_at = ?, archived_at = ?, deleted_at = ?
			WHERE id = ?`, issue.Title, issue.Description, nullableString(issue.Prompt), statusForStorage(issue), issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt, nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID); err != nil {
			return fmt.Errorf("update issue: %w", err)
		}
		if in.Labels != nil {
			if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, "links"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return model.Issue{}, err
	}
	return s.GetIssue(ctx, issue.ID)
}

// [LAW:dataflow-not-control-flow] ApplyUpdate is the single execution path for all lit update mutations.
// Variability lives in the input values: empty TargetStatus = no transitions; empty Fields = no field write.
// Empty TargetStatus must not be normalized — DefaultOpen("") would mutate the "no --status flag" signal
// into a real "open" target, which then fails the transition lookup for containers (StatusValue == "").
func (s *Store) ApplyUpdate(ctx context.Context, id string, in ApplyUpdateInput) (model.Issue, error) {
	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if strings.TrimSpace(in.TargetStatus) != "" {
		in.TargetStatus = string(model.DefaultOpen(in.TargetStatus))
	}
	actions, err := statusTransitionActionsForApplyUpdate(current.StatusValue(), in.TargetStatus)
	if err != nil {
		return model.Issue{}, err
	}
	reason := in.TransitionReason
	if reason == "" && len(actions) > 0 {
		reason = fmt.Sprintf("status update via lit update: %s -> %s", current.StatusValue(), in.TargetStatus)
	}
	for _, action := range actions {
		if _, err = s.TransitionIssue(ctx, TransitionIssueInput{
			IssueID:   id,
			Action:    action,
			Reason:    reason,
			CreatedBy: in.TransitionBy,
		}); err != nil {
			return model.Issue{}, err
		}
	}
	// UpdateIssue always does a fresh GetIssue internally; it is also a no-op if Fields is empty.
	return s.UpdateIssue(ctx, id, in.Fields)
}

func (s *Store) AddComment(ctx context.Context, in AddCommentInput) (model.Comment, error) {
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
	if err := s.withMutation(ctx, "add comment", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		return nil
	}); err != nil {
		return model.Comment{}, err
	}
	return comment, nil
}

func (s *Store) TransitionIssue(ctx context.Context, in TransitionIssueInput) (model.Issue, error) {
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
	if parsed, err := model.ParseAction(action); err == nil {
		return s.writeStatusTransition(ctx, issue, actor, reason, parsed)
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
	if err := s.withMutation(ctx, "transition issue", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, updated_at = ?, closed_at = ?, archived_at = ?, deleted_at = ? WHERE id = ?`,
			statusForStorage(issue), issue.UpdatedAt.Format(time.RFC3339Nano), nullableTime(issue.ClosedAtValue()), nullableTime(issue.ArchivedAt), nullableTime(issue.DeletedAt), issue.ID); err != nil {
			return fmt.Errorf("update issue lifecycle: %w", err)
		}
		return s.insertHistoryTx(ctx, tx, issue.ID, string(action), reason, fromStatus, toStatus, actor)
	}); err != nil {
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

func (s *Store) writeStatusTransition(ctx context.Context, issue model.Issue, actor string, reason string, action model.ActionName) (model.Issue, error) {
	if issue.DeletedAt != nil || issue.ArchivedAt != nil {
		return model.Issue{}, fmt.Errorf("cannot %s archived or deleted issue", action)
	}
	updated, err := issue.Apply(action, actor, reason)
	if err != nil {
		return model.Issue{}, err
	}
	fromStatus := issue.StatusValue()
	toStatus := updated.StatusValue()
	now := time.Now().UTC()
	var closedAt any
	if value := updated.ClosedAtValue(); value != nil {
		closedAt = value.Format(time.RFC3339Nano)
	}
	if err := s.withMutation(ctx, "transition issue", func(ctx context.Context, tx *sql.Tx) error {
		// [LAW:dataflow-not-control-flow] Status transitions always execute one guarded write; contention is modeled by affected row count.
		result, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, updated_at = ?, closed_at = ? WHERE id = ? AND status = ?`,
			toStatus, now.Format(time.RFC3339Nano), closedAt, issue.ID, fromStatus)
		if err != nil {
			return fmt.Errorf("update issue status: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read status transition result: %w", err)
		}
		if affected == 0 {
			currentStatus, lookupErr := currentStatusTx(ctx, tx, issue.ID)
			if lookupErr != nil {
				return lookupErr
			}
			return fmt.Errorf("%s conflict: issue status is %q", action, currentStatus)
		}
		return s.insertHistoryTx(ctx, tx, issue.ID, string(action), reason, fromStatus, toStatus, actor)
	}); err != nil {
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
		t, err := scanTime(createdAt)
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
	Prompt      string
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
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&issue.ID, &issue.Title, &issue.Description, &prompt, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &createdAt, &updatedAt, &closedAt, &archivedAt, &deletedAt); err != nil {
		return issueRow{}, err
	}
	issue.Prompt = prompt.String
	return parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, archivedAt, deletedAt)
}

func scanIssueWithParent(row issueScanner) (string, issueRow, error) {
	var parentID string
	var issue partialIssue
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&parentID, &issue.ID, &issue.Title, &issue.Description, &prompt, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &createdAt, &updatedAt, &closedAt, &archivedAt, &deletedAt); err != nil {
		return "", issueRow{}, err
	}
	issue.Prompt = prompt.String
	parsed, err := parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, archivedAt, deletedAt)
	return parentID, parsed, err
}

func parsedIssueRow(issue partialIssue, status sql.NullString, assignee string, createdAt string, updatedAt string, closedAt sql.NullString, archivedAt sql.NullString, deletedAt sql.NullString) (issueRow, error) {
	var err error
	issue.CreatedAt, err = scanTime(createdAt)
	if err != nil {
		return issueRow{}, err
	}
	issue.UpdatedAt, err = scanTime(updatedAt)
	if err != nil {
		return issueRow{}, err
	}
	// Container rows store NULL status; hydrateIssues ignores StatusView for them
	// and constructs the lifecycle via HydrateAllOf instead.
	// [LAW:single-enforcer] The decision "use StatusView vs derive from children"
	// lives in hydrateIssues; here we just carry the row data as it appears.
	statusView := model.StatusView{Value: model.State(status.String), Assignee: assignee}
	if closedAt.Valid {
		t, err := scanTime(closedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		statusView.ClosedAt = &t
	}
	if archivedAt.Valid {
		t, err := scanTime(archivedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		issue.ArchivedAt = &t
	}
	if deletedAt.Valid {
		t, err := scanTime(deletedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		issue.DeletedAt = &t
	}
	issue.Labels = []string{}
	return issueRow{Issue: issue, Status: statusView}, nil
}

// scanTime parses an RFC3339Nano timestamp returned by a SQL row scan.
// [LAW:single-enforcer] All RFC3339Nano-typed timestamp columns parse through
// here so the format binding lives in one place; changing the wire format is
// one edit, not 12.
func scanTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

// statusForStorage returns the value to persist in the issues.status column.
// The capability presence is the type-encoded answer to the container-vs-leaf
// question — leaves expose Status, containers do not — so this projection
// never asks IsContainer; it asks the lifecycle.
// [LAW:one-source-of-truth] Container state lives in the AllOf lifecycle, not
// the DB column. Writing NULL keeps the column from lying about what it owns.
func statusForStorage(issue model.Issue) sql.NullString {
	if status := issue.Capabilities().Status; status != nil {
		return sql.NullString{String: string(status.Value), Valid: true}
	}
	return sql.NullString{}
}

// statusForStorageRaw is the (issueType, status) entry point for write paths
// that haven't yet built a hydrated Issue (import / restore). It hydrates a
// minimal Issue through the canonical shape parser, then delegates the column
// projection to statusForStorage so both write entrypoints share one rule.
// [LAW:single-enforcer] One rule for "what goes in the status column" applies
// to every write path; statusForStorage is that rule and this routes through it.
func statusForStorageRaw(issueType string, status string) (sql.NullString, error) {
	view := model.StatusView{}
	if !model.IsContainerType(issueType) {
		state := model.DefaultOpen(status)
		view.Value = state
	}
	issue, err := model.HydrateRow(model.Issue{IssueType: issueType}, view, nil)
	if err != nil {
		return sql.NullString{}, err
	}
	return statusForStorage(issue), nil
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
			Prompt:      row.Issue.Prompt,
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
		// [LAW:single-enforcer] This store hydrator is the only read boundary
		// that turns row status plus child relations into model lifecycle state.
		// HydrateRow owns the container-vs-leaf dispatch.
		issue, err := model.HydrateRow(base, row.Status, childrenByEpicID[row.Issue.ID])
		if err != nil {
			return nil, err
		}
		// [LAW:single-enforcer] Hydrator post-condition: every returned Issue is
		// fully hydrated. Consumers receive a typed value and never need to
		// re-check IsHydrated() defensively. (subsumes va-001-hydration-b3z)
		if !issue.IsHydrated() {
			return nil, fmt.Errorf("hydrateIssues: produced unhydrated issue %s", issue.ID)
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
	rows, err := s.db.QueryContext(ctx, `SELECT r.dst_id, i.id, i.title, i.description, i.agent_prompt, i.status, i.priority, i.issue_type, i.topic, i.assignee, i.item_rank, i.created_at, i.updated_at, i.closed_at, i.archived_at, i.deleted_at
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
	if priority != model.PriorityNormal && priority != model.PriorityUrgent {
		return fmt.Errorf("priority must be %d (normal) or %d (urgent)", model.PriorityNormal, model.PriorityUrgent)
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.Format(time.RFC3339Nano)
}

// nullableString stores empty strings as SQL NULL so the agent_prompt column reads
// back as "" via sql.NullString.String regardless of whether the row predates
// the column add.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func ensureDoltDatabase(ctx context.Context, doltRootDir string, workspaceID string) (bool, error) {
	root := filepath.Clean(doltRootDir)
	created := !dirExists(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return false, fmt.Errorf("create dolt root dir: %w", err)
	}
	db, err := sql.Open(doltDriverName, buildDoltDSN(root, workspaceID, false))
	if err != nil {
		return false, fmt.Errorf("open dolt bootstrap: %w", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", doltDatabaseName)); err != nil {
		return false, fmt.Errorf("create dolt database: %w", err)
	}
	db, err = sql.Open(doltDriverName, buildDoltDSN(root, workspaceID, true))
	if err != nil {
		return false, fmt.Errorf("open dolt bootstrap database: %w", err)
	}
	defer db.Close()
	if err := ensureMasterDefaultBranch(ctx, db); err != nil {
		return false, err
	}
	return created, nil
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

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
