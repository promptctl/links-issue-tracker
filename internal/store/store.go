package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/dolthub/driver"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
)

const (
	doltDriverName   = "dolt"
	doltDatabaseName = "links"
)

type Store struct {
	db                   *sql.DB
	workspaceID          string
	doltRootDir          string
	commitLockPath       string
	telemetryDir         string
	releaseWorkspaceLock func() error
}

type NotFoundError struct {
	Entity string
	ID     string
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Entity, e.ID)
}

// ValidationError is returned when a domain constraint (field value, type, range) is violated.
// [LAW:types-are-the-program] The type carries the classification so callers dispatch on type, not message text.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }

type SyncState struct {
	Path        string
	ContentHash string
}

// RankPlacement selects where a newly created issue lands in the rank order.
// Its zero value is RankTop, so the product default — fresh work surfaces at
// the top — is also the type default: a CreateIssueInput that says nothing
// about placement gets top.
type RankPlacement int

const (
	RankTop    RankPlacement = iota // sorts before all existing items (default)
	RankBottom                      // sorts after all existing items
)

type CreateIssueInput struct {
	Title       string
	Description string
	Prompt      string
	IssueType   string
	Topic       string
	ParentID    string
	Priority    int
	Assignee    string
	Lane        string
	Labels      []string
	// Placement decides where the new issue lands in the rank order. Zero value
	// (RankTop) surfaces fresh work at the top; callers that author an ordered
	// batch (e.g. preserving creation order) pass RankBottom.
	Placement RankPlacement
	// Prefix is the workspace's cosmetic ID prefix (e.g., "links" → "links-foo-abc1").
	// Sourced from workspace config at the call site. Not persisted as derived state.
	Prefix string
}

// UpdateIssueInput is the field-axis patch of a Change: only columns the field
// axis owns are representable, so a status write through the field path is
// unconstructible rather than guarded at runtime. [LAW:types-are-the-program]
type UpdateIssueInput struct {
	Title       *string
	Description *string
	Prompt      *string
	IssueType   *string
	Priority    *int
	Assignee    *string
	Lane        *string
	Labels      *[]string
	// Reason is optional free text recorded on the field-change event.
	Reason string
}

func (u UpdateIssueInput) IsEmpty() bool {
	return u.Title == nil && u.Description == nil && u.Prompt == nil && u.IssueType == nil &&
		u.Priority == nil && u.Assignee == nil && u.Lane == nil && u.Labels == nil
}

// Change is THE issue-record mutation input: an optional lifecycle action
// paired with a field patch and the transition's provenance. nil Action means
// no transition; empty Fields means no field mutations. The action variant
// carries exactly its payload (Start the assignee, Close the outcome), so the
// loose per-action parameters this seam used to thread are unrepresentable.
// Which axis the action drives — status machine or retention — is the sum's
// own structure (StatusAction vs not), never a caller-side mode.
// [LAW:types-are-the-program]
//
// Actor is THE actor for the whole change — one call, one author, recorded on
// both events it may produce. [LAW:one-source-of-truth] Reasons stay
// per-event: Reason belongs to the transition event, Fields.Reason to the
// field-change event, because a combined change records two events whose
// reasons are independently set — `lit update` deliberately synthesizes a
// transition reason while leaving the field reason as typed.
type Change struct {
	Action model.Action
	Fields UpdateIssueInput
	Actor  string
	Reason string
}

func (c Change) IsEmpty() bool {
	return c.Action == nil && c.Fields.IsEmpty()
}

// applyTransition runs every guard a status transition must pass before it
// writes — the archived/deleted refusal and the lifecycle Apply (which rejects
// every action on a container, whose state is derived from children) — and
// returns the post-transition issue, or the error the write would surface. It
// never mutates the store. [LAW:single-enforcer]
func applyTransition(issue model.Issue, action model.StatusAction) (model.Issue, error) {
	// [LAW:single-enforcer] What counts as out-of-the-flow is the typed Frozen
	// predicate beside the Retention sum, not a variant match owned here.
	if model.Frozen(issue.Retention()) {
		return model.Issue{}, fmt.Errorf("cannot %s archived or deleted issue", action.Name())
	}
	return issue.Apply(action)
}

type SortSpec struct {
	Field string
	Desc  bool
}

type ListIssuesFilter struct {
	Statuses          []model.State
	Resolutions       []model.Resolution
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

func Open(ctx context.Context, doltRootDir string, workspaceID string) (_ *Store, err error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Workspace shared lock is acquired BEFORE
	// EnsureDatabase because ensureDoltDatabase opens Dolt SQL connections
	// to create/initialize the database. Acquiring after would leave a
	// window in which `lit snapshots restore` could rotate the Dolt
	// directory while those bootstrap connections were live.
	release, err := acquireWorkspaceShared(ctx, doltRootDir)
	if err != nil {
		return nil, err
	}
	// [LAW:no-silent-failure] On any failure path before the Store owns
	// the lock, release the hold AND surface a release error via the named
	// return — a leaked shared hold would block subsequent restores with
	// workspace-busy from a vanished caller. success guards the happy path
	// so the lock survives in the returned Store.
	success := false
	defer func() {
		if success {
			return
		}
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if _, err = EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// [LAW:single-enforcer] Store-level commit lock is the single writer gate for all startup and runtime mutations.
	if err = s.withCommitLock(ctx, s.migrate); err != nil {
		if closeErr := s.db.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			err = errors.Join(err, closeErr)
		}
		s.releaseWorkspaceLock = nil
		return nil, err
	}
	success = true
	return s, nil
}

func OpenForRead(ctx context.Context, doltRootDir string, workspaceID string) (_ *Store, err error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	// [LAW:dataflow-not-control-flow] Acquire the workspace shared lock
	// BEFORE the existence stat so the stat cannot observe the transient
	// ENOENT that dbsnapshot.Restore opens between rotate-away and
	// install-snapshot. The lock blocks until restore releases its
	// exclusive hold, after which the stat sees a real result every time.
	release, err := acquireWorkspaceShared(ctx, doltRootDir)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if success {
			return
		}
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if _, statErr := os.Stat(doltRootDir); statErr != nil {
		// [LAW:no-silent-failure] Only ENOENT means "uninitialized";
		// every other stat error (EACCES, EIO, ELOOP, etc.) is its own
		// failure mode the operator needs to see, not a vague downstream
		// Dolt-connection error.
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("repository not initialized with lit — run 'lit init' first")
		}
		return nil, fmt.Errorf("stat database dir: %w", statErr)
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// Auto-migrate stale schemas so read paths don't fail on missing columns/tables.
	// Unlike Open, this does NOT call EnsureDatabase — the DB must already exist.
	if err = s.withCommitLock(ctx, s.migrate); err != nil {
		if closeErr := s.db.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			err = errors.Join(err, closeErr)
		}
		s.releaseWorkspaceLock = nil
		return nil, err
	}
	success = true
	return s, nil
}

func EnsureDatabase(ctx context.Context, doltRootDir string, workspaceID string) (bool, error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return false, err
	}
	return ensureDoltDatabase(ctx, doltRootDir, workspaceID)
}

func validateOpenArgs(doltRootDir string, workspaceID string) error {
	if _, err := validateDoltRootDir(doltRootDir); err != nil {
		return err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("workspace id is required")
	}
	return nil
}

// validateDoltRootDir validates a Dolt root path argument and returns its cleaned,
// canonical form. [LAW:single-enforcer][LAW:one-source-of-truth] One definition of
// "a usable root path", reached by every exported entry point that takes one:
// Open/OpenForRead/OpenSync/DumpRaw via validateOpenArgs, and
// Recover/PromoteCandidate/HealWorkspace directly. It rejects empty input (so a
// path never silently degrades into cwd-relative scratch, lock, and backup
// artifacts) and normalizes the rest, so equivalent paths that differ only by a
// trailing separator derive the SAME lock path, backup names, staging parent, and
// rename target — making "write the backup at one spelling, scan for it at
// another" unrepresentable.
func validateDoltRootDir(doltRootDir string) (string, error) {
	if strings.TrimSpace(doltRootDir) == "" {
		return "", errors.New("dolt root dir is required")
	}
	return filepath.Clean(doltRootDir), nil
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
		err = nil
	}
	// [LAW:dataflow-not-control-flow] Workspace lock release runs on every
	// Close, regardless of whether the DB closed cleanly — the shared hold
	// must end with the Store's lifetime so a subsequent restore is not
	// pinned by a dead Store. errors.Join keeps both failures observable
	// when db.Close and release both fail; a leaked workspace lock would
	// silently block every future restore with workspace-busy.
	if s.releaseWorkspaceLock != nil {
		release := s.releaseWorkspaceLock
		s.releaseWorkspaceLock = nil
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
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
		commitLockPath: commitLockPathForDolt(doltRootDir),
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
		Lane:        strings.TrimSpace(in.Lane),
		Labels:      labels,
		Assignee:    strings.TrimSpace(in.Assignee),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	issue, err = model.HydrateRow(issue, model.StatusView{Value: model.StateOpen}, nil)
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
		issue.Rank, err = nextRankForPlacement(ctx, tx, in.Placement)
		if err != nil {
			return err
		}
		archivedCol, deletedCol := retentionColumns(issue)
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(
			id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, closed_at, archived_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
			issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), statusForStorage(issue), issue.Priority, issue.IssueType, issue.Topic,
			issue.AssigneeValue(), issue.Rank, issue.Lane, issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), archivedCol, deletedCol); err != nil {
			return fmt.Errorf("insert issue: %w", err)
		}
		if parentID != "" {
			// [LAW:one-source-of-truth] Build the edge as a value and route through
			// insertRelationTx; the relations INSERT statement lives only there.
			parentEdge := model.Relation{
				SrcID:     issue.ID,
				DstID:     parentID,
				Type:      model.RelParentChild,
				CreatedAt: issue.CreatedAt,
				CreatedBy: createdBy,
			}
			if err := insertRelationTx(ctx, tx, parentEdge); err != nil {
				return err
			}
		}
		if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, createdBy); err != nil {
			return err
		}
		// CreateIssue's "created" event records the initial status as a single
		// field-change row. Containers don't carry status so no row for them.
		createChanges := []model.FieldChange{}
		if !model.IsContainerType(issue.IssueType) {
			createChanges = append(createChanges, model.FieldChange{Field: "status", From: "", To: "open"})
		}
		if err := s.recordEvent(ctx, tx, issue.ID, "created", "issue created", createdBy, createChanges); err != nil {
			return err
		}
		return smoothRanksIfNeededTx(ctx, tx, issue.Rank)
	}); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) ListIssues(ctx context.Context, filter ListIssuesFilter) ([]model.Issue, error) {
	query := `SELECT ` + issueColumnsQualified + ` FROM issues i`
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
	return capLimit(filterByResolution(filterByState(hydrated, allowedStates), filter.Resolutions), filter.Limit), nil
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

// filterByResolution keeps only issues whose recorded close resolution is in
// allowed; an empty allow-list passes everything through. Resolution lives in
// the lifecycle, so the filter reads the derived ResolutionValue() rather than
// the DB column — the same post-hydration discipline as filterByState. An issue
// with no resolution (open, in_progress, or a `done`/legacy close) matches no
// non-empty allow-list. [LAW:single-enforcer]
func filterByResolution(issues []model.Issue, allowed []model.Resolution) []model.Issue {
	if len(allowed) == 0 {
		return issues
	}
	allow := make(map[model.Resolution]struct{}, len(allowed))
	for _, r := range allowed {
		allow[r] = struct{}{}
	}
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		resolution := issue.ResolutionValue()
		if resolution == nil {
			continue
		}
		if _, ok := allow[*resolution]; ok {
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
	events, err := s.listEvents(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}

	// [LAW:single-enforcer] Hydrate every related issue in one query
	// rather than running N+1 GetIssue calls. The map lets the relation
	// loop below stay a pure data-flow over already-hydrated rows. The
	// redirect target joins the same batch — hydrating it costs no extra
	// query.
	relatedIDs := collectRelatedIssueIDs(id, relations)
	if target := issue.RedirectTargetValue(); target != nil && !slices.Contains(relatedIDs, *target) {
		relatedIDs = append(relatedIDs, *target)
	}
	relatedByID, err := s.getIssuesByIDs(ctx, relatedIDs)
	if err != nil {
		return model.IssueDetail{}, err
	}

	// [LAW:one-source-of-truth] Structural edges (parent/child/blocks) are
	// bucketed by the same helper the batch accessor uses, so the blocks
	// convention has one definition. Related is GetIssueDetail's own concern.
	structural := bucketRelations(id, relations, relatedByID)
	// Siblings are the parent's other children. The set exists only when the
	// issue has a parent; an only child yields the empty slice and the renderer
	// omits the group. [LAW:one-source-of-truth] derived from the same
	// rank-ordered children query other consumers read, minus self.
	siblings := []model.Issue{}
	if structural.Parent != nil {
		parentChildren, err := s.ListChildren(ctx, structural.Parent.ID)
		if err != nil {
			return model.IssueDetail{}, err
		}
		siblings = siblingsOf(id, parentChildren)
	}
	// The redirect target hydrates from the issue's own column — related-to
	// edges mean exactly one thing (manual peer links), so there is nothing to
	// re-derive. A target whose row has vanished hydrates as absent, matching
	// getIssuesByIDs' hole-skipping contract.
	var redirectTarget *model.Issue
	if target := issue.RedirectTargetValue(); target != nil {
		if hydrated, ok := relatedByID[*target]; ok {
			redirectTarget = &hydrated
		}
	}
	related := relatedFrom(id, relations, relatedByID)
	detail := model.IssueDetail{
		Issue:          issue,
		Relations:      relations,
		Comments:       comments,
		Events:         events,
		Children:       structural.Children,
		Siblings:       siblings,
		DependsOn:      structural.DependsOn,
		Blocks:         structural.Blocks,
		Parent:         structural.Parent,
		Related:        related,
		RedirectTarget: redirectTarget,
	}
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
	query := fmt.Sprintf(`SELECT `+issueColumnsBare+` FROM issues WHERE id IN (%s)`, strings.Join(placeholders, ","))
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
	row := s.db.QueryRowContext(ctx, `SELECT `+issueColumnsBare+` FROM issues WHERE id = ?`, id)
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

// fieldWrite is a fully-planned field mutation, ready to execute against a tx.
// planFieldUpdate computes it (pure: validation + in-memory mutation + the
// change-row diff); applyFieldsTx performs the writes. The plan/apply split is
// what lets Apply run a field write inside the same transaction as a
// status transition, so a combined update lands as one Dolt commit instead of
// two. [LAW:decomposition] [LAW:effects-at-boundaries]
type fieldWrite struct {
	issue         model.Issue
	replaceLabels bool
	actor         string
	reason        string
	changes       []model.FieldChange
}

// planFieldUpdate validates in against baseline and computes the post-write
// issue plus its field-change rows, without touching the store. baseline is
// Apply's read of the row, or the post-action issue when the change also
// carries a lifecycle action — so a start's new assignee is the diff's prior,
// not a second assignee change row. [LAW:effects-at-boundaries] A pure
// function of (baseline, in, actor): no clock, no IO. The UpdatedAt stamp and
// every write are deferred to applyFieldsTx.
func planFieldUpdate(baseline model.Issue, in UpdateIssueInput, actor string) (fieldWrite, error) {
	issue := baseline
	priorTitle := issue.Title
	priorDescription := issue.Description
	priorIssueType := issue.IssueType
	priorPriority := issue.Priority
	priorAssignee := issue.AssigneeValue()
	priorLane := issue.Lane
	priorLabels := strings.Join(issue.Labels, ",")
	if in.Title != nil {
		issue.Title = strings.TrimSpace(*in.Title)
		if issue.Title == "" {
			return fieldWrite{}, errors.New("title cannot be empty")
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
			return fieldWrite{}, err
		}
		// [LAW:single-enforcer] Container vs leaf is encoded in the lifecycle
		// expression at hydration time. Switching across that boundary would
		// orphan the lifecycle: epic → leaf would leave AllOf attached to a
		// row whose schema requires a leaf status, and leaf → epic would
		// silently drop the leaf's status/closed_at. Refuse here
		// instead of patching it up downstream with an invented default.
		if model.IsContainerType(issue.IssueType) != model.IsContainerType(issueType) {
			return fieldWrite{}, fmt.Errorf("cannot change issue_type between container (%v) and leaf types: lifecycle capability would change", model.ContainerIssueTypes)
		}
		issue.IssueType = issueType
	}
	if in.Priority != nil {
		if err := validatePriority(*in.Priority); err != nil {
			return fieldWrite{}, err
		}
		issue.Priority = *in.Priority
	}
	if in.Assignee != nil {
		// [LAW:decomposition] Assignee is an issue-level field independent of the
		// lifecycle; reassigning is a plain field write, not a status mutation.
		issue.Assignee = strings.TrimSpace(*in.Assignee)
	}
	if in.Lane != nil {
		issue.Lane = strings.TrimSpace(*in.Lane)
	}
	if in.Labels != nil {
		labels, err := canonicalizeLabels(*in.Labels)
		if err != nil {
			return fieldWrite{}, err
		}
		issue.Labels = labels
	}
	// [LAW:dataflow-not-control-flow] Every field write emits one event with a
	// field-change row per actually-changed field.
	var changes []model.FieldChange
	if priorTitle != issue.Title {
		changes = append(changes, model.FieldChange{Field: "title", From: priorTitle, To: issue.Title})
	}
	if priorDescription != issue.Description {
		changes = append(changes, model.FieldChange{Field: "description", From: priorDescription, To: issue.Description})
	}
	if priorIssueType != issue.IssueType {
		changes = append(changes, model.FieldChange{Field: "issue_type", From: priorIssueType, To: issue.IssueType})
	}
	if priorPriority != issue.Priority {
		changes = append(changes, model.FieldChange{Field: "priority", From: strconv.Itoa(priorPriority), To: strconv.Itoa(issue.Priority)})
	}
	newAssignee := issue.AssigneeValue()
	if priorAssignee != newAssignee {
		changes = append(changes, model.FieldChange{Field: "assignee", From: priorAssignee, To: newAssignee})
	}
	if priorLane != issue.Lane {
		changes = append(changes, model.FieldChange{Field: "lane", From: priorLane, To: issue.Lane})
	}
	newLabels := strings.Join(issue.Labels, ",")
	if priorLabels != newLabels {
		changes = append(changes, model.FieldChange{Field: "labels", From: priorLabels, To: newLabels})
	}
	return fieldWrite{issue: issue, replaceLabels: in.Labels != nil, actor: actor, reason: in.Reason, changes: changes}, nil
}

// applyFieldsTx writes a planned field mutation against tx: the issue UPDATE,
// the label replacement (when requested), and the change event (when any field
// moved). It owns only writes — every decision was made in planFieldUpdate — so
// it composes into any transaction a caller already holds. [LAW:single-enforcer]
// The UPDATE sets only the columns the field axis owns: the lifecycle columns
// (status, closed_at, archived_at, deleted_at) belong to the transition and
// retention writes, so a stale field plan cannot clobber a concurrent
// lifecycle change — the cross-axis write is unwritable, not guarded.
// [LAW:dataflow-not-control-flow]
func (s *Store) applyFieldsTx(ctx context.Context, tx *sql.Tx, w fieldWrite) error {
	issue := w.issue
	// [LAW:effects-at-boundaries] The clock is read here, at the write boundary,
	// not in planFieldUpdate — so the plan stays a pure function of its inputs.
	issue.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET
		title = ?, description = ?, agent_prompt = ?, priority = ?, issue_type = ?, assignee = ?, lane = ?, updated_at = ?
		WHERE id = ?`, issue.Title, issue.Description, nullableString(issue.Prompt), issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.Lane, issue.UpdatedAt.Format(time.RFC3339Nano), issue.ID); err != nil {
		return fmt.Errorf("update issue: %w", err)
	}
	if w.replaceLabels {
		if err := s.replaceLabelsTx(ctx, tx, issue.ID, issue.Labels, w.actor); err != nil {
			return err
		}
	}
	if len(w.changes) > 0 {
		if err := s.recordEvent(ctx, tx, issue.ID, "", w.reason, w.actor, w.changes); err != nil {
			return err
		}
	}
	return nil
}

// [LAW:dataflow-not-control-flow] Apply is the single execution path for issue-record changes.
// Variability lives in the Change value: nil Action = no transition; empty Fields = no field write.
// [LAW:types-are-the-program] Every target state is reachable by exactly one action variant;
// no compound chains, no from-state preconditions. The 3x3 minus diagonal collapses to one call per change.
// Same-state transitions flow through planStatusTransition unconditionally; the leaf decides what they
// mean. A pure no-op (same state, same resulting assignee) records nothing — history reflects actual
// mutations. A same-state start with a new assignee is the canonical agent-reclaim path and records
// the assignee change with the calling Actor, which is the audit substrate for "who interacted with
// this ticket" history queries.
func (s *Store) Apply(ctx context.Context, id string, c Change) (model.Issue, error) {
	current, err := s.GetIssue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	// [LAW:no-ambient-temporal-coupling] The combined transition+field update has
	// exactly one atomicity owner: this method plans both writes (pure: reads,
	// validation, in-memory mutation) and then performs them inside a SINGLE
	// withMutation, so they share one SQL transaction and one Dolt commit. A
	// validation error or crash between the two writes can no longer leave a
	// status-moved-but-fields-unwritten row — the torn state is unrepresentable.
	// [LAW:one-source-of-truth] One change has one author: the actor is
	// normalized once here and recorded on every event the change produces.
	actor := strings.TrimSpace(c.Actor)
	if actor == "" {
		actor = "unknown"
	}
	baseline := current
	var lw lifecycleWrite
	if c.Action != nil {
		lw, err = s.planLifecycleAction(ctx, current, actor, strings.TrimSpace(c.Reason), c.Action)
		if err != nil {
			return model.Issue{}, err
		}
		// The field write starts from the post-action issue so a start's new
		// assignee is restated, not overwritten, and the diff sees it as prior.
		baseline = lw.postIssue()
	}
	var fw fieldWrite
	hasFields := !c.Fields.IsEmpty()
	if hasFields {
		fw, err = planFieldUpdate(baseline, c.Fields, actor)
		if err != nil {
			return model.Issue{}, err
		}
	}
	needsActionWrite := lw != nil && !lw.isNoop()
	if needsActionWrite || hasFields {
		if err := s.withMutation(ctx, "apply update", func(ctx context.Context, tx *sql.Tx) error {
			if needsActionWrite {
				if err := lw.applyTx(ctx, s, tx); err != nil {
					return err
				}
			}
			if hasFields {
				if err := s.applyFieldsTx(ctx, tx, fw); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return model.Issue{}, err
		}
	}
	return s.GetIssue(ctx, id)
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

func (s *Store) DeleteComment(ctx context.Context, commentID string) (model.Comment, error) {
	id := strings.TrimSpace(commentID)
	if id == "" {
		return model.Comment{}, errors.New("comment id is required")
	}
	var deleted model.Comment
	// [LAW:single-enforcer] Existence is verified by the SELECT inside the same mutation that deletes,
	// so the row proven present is the row removed — no TOCTOU gap, no separate guard.
	if err := s.withMutation(ctx, "delete comment", func(ctx context.Context, tx *sql.Tx) error {
		var createdAt string
		row := tx.QueryRowContext(ctx, `SELECT id, issue_id, body, created_at, created_by FROM comments WHERE id = ?`, id)
		if err := row.Scan(&deleted.ID, &deleted.IssueID, &deleted.Body, &createdAt, &deleted.CreatedBy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// [LAW:one-type-per-behavior] Typed not-found error matches GetIssue / relation removals,
				// so callers detect not-found via errors.As instead of string-matching the message.
				return NotFoundError{Entity: "comment", ID: id}
			}
			return fmt.Errorf("read comment: %w", err)
		}
		t, err := scanTime(createdAt)
		if err != nil {
			return err
		}
		deleted.CreatedAt = t
		if _, err := tx.ExecContext(ctx, `DELETE FROM comments WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete comment: %w", err)
		}
		return nil
	}); err != nil {
		return model.Comment{}, err
	}
	return deleted, nil
}

// lifecycleWrite is the planned lifecycle mutation Apply executes — a sealed
// two-variant sum mirroring the action sum's axis split: transitionWrite for
// StatusActions, retentionWrite for the retention actions. Apply holds exactly
// one plan whichever axis the action drives, so the axis is a value flowing
// through one seam, not a second code path. [LAW:dataflow-not-control-flow]
type lifecycleWrite interface {
	// applyTx performs the planned writes against tx; it owns only writes, so
	// it composes into the transaction Apply already holds.
	applyTx(ctx context.Context, s *Store, tx *sql.Tx) error
	// postIssue is the post-action issue a following field write baselines on.
	postIssue() model.Issue
	// isNoop reports that the plan owes no write.
	isNoop() bool
}

// planLifecycleAction plans any lifecycle action by its axis: StatusActions
// travel the status state machine, everything else in the sealed sum is a
// retention action and travels the Retain transition table. The dispatch is
// the sum's own structure — a retention action reaching the status machine, or
// vice versa, is unrepresentable. [LAW:types-are-the-program]
func (s *Store) planLifecycleAction(ctx context.Context, issue model.Issue, actor string, reason string, action model.Action) (lifecycleWrite, error) {
	if statusAction, ok := action.(model.StatusAction); ok {
		w, err := s.planStatusTransition(ctx, issue, actor, reason, statusAction)
		if err != nil {
			return nil, err
		}
		return w, nil
	}
	w, err := planRetentionTransition(issue, actor, reason, action)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// transitionWrite is a fully-planned status transition, ready to execute
// against a tx. planStatusTransition is the read+validate+compute half — it runs
// the applyTransition guards, the assignee rule, the redirect-target validation
// (which reads the DB to confirm the target exists), and the change-row diff,
// but performs no writes. applyTransitionTx is the write half: the single
// guarded UPDATE plus the event. The split is read/compute vs
// write, not pure vs impure — planStatusTransition does read the clock and the
// store; what it defers is every mutation. The plan also carries `post` — the
// post-transition issue — which Apply uses as the baseline for a following
// field write. A noop plan records that the target state and assignee already
// hold, so no write is owed. [LAW:decomposition]
type transitionWrite struct {
	issueID           string
	fromStatus        string
	toStatus          string
	postAssignee      string
	now               time.Time
	closedAtArg       any
	resolutionArg     any
	redirectTargetArg any
	action            model.ActionName
	reason            string
	actor             string
	changes           []model.FieldChange
	post              model.Issue
	noop              bool
}

func (w transitionWrite) applyTx(ctx context.Context, s *Store, tx *sql.Tx) error {
	return s.applyTransitionTx(ctx, tx, w)
}
func (w transitionWrite) postIssue() model.Issue { return w.post }
func (w transitionWrite) isNoop() bool           { return w.noop }

func (s *Store) planStatusTransition(ctx context.Context, issue model.Issue, actor string, reason string, action model.StatusAction) (transitionWrite, error) {
	updated, err := applyTransition(issue, action)
	if err != nil {
		return transitionWrite{}, err
	}
	priorAssignee := issue.AssigneeValue()
	// Only Start rewrites the assignee — it is the variant that carries the new
	// owner. Every other transition preserves ownership: assignee is an
	// issue-level field orthogonal to the status state machine, untouched by
	// changing state. [LAW:types-are-the-program] The payload comes from the
	// variant, not a loose parameter every other action had to ignore.
	postAssignee := priorAssignee
	if start, ok := action.(model.Start); ok {
		postAssignee = strings.TrimSpace(start.Assignee)
	}
	updated.Assignee = postAssignee
	fromStatus := issue.StatusValue()
	toStatus := updated.StatusValue()
	// [LAW:one-source-of-truth] History records actual mutations only. A call
	// whose target state and resulting assignee both match the current row is
	// the documented leaf-state Apply no-op: no write, no event. The claim
	// audit substrate survives because a reclaim (same state, new assignee)
	// falls through and records the assignee change with the calling actor.
	if toStatus == fromStatus && postAssignee == priorAssignee {
		return transitionWrite{noop: true, post: issue}, nil
	}
	now := time.Now().UTC()
	var closedAtArg any
	if value := updated.ClosedAtValue(); value != nil {
		closedAtArg = value.Format(time.RFC3339Nano)
	}
	// The close outcome traveled through the state machine into the closed leaf,
	// so the post-transition resolution is read back off the updated issue — no
	// re-attachment, and a non-closed target structurally carries none, so a
	// resolution can never linger on a non-closed row. [LAW:types-are-the-program]
	postResolution := updated.ResolutionValue()
	var resolutionArg any
	if postResolution != nil {
		resolutionArg = string(*postResolution)
	}
	// The redirect target traveled through the state machine into the closed
	// leaf exactly like the resolution — only the redirecting outcomes carry
	// one, structurally — so the plan reads it back off the post-transition
	// issue and validates it the way AddRelation validates endpoints: it must
	// exist and cannot be the issue itself. Validating before the transaction
	// mirrors the target-existence read AddRelation does outside its mutation.
	// [LAW:one-source-of-truth] The leaf is the single carrier; there is no
	// separate outcome re-extraction to drift from it.
	postRedirect := updated.RedirectTargetValue()
	if err := s.validateRedirectTarget(ctx, issue.ID, postResolution, postRedirect); err != nil {
		return transitionWrite{}, err
	}
	var redirectTargetArg any
	if postRedirect != nil {
		redirectTargetArg = *postRedirect
	}
	// [LAW:one-source-of-truth] Change rows mirror the columns that actually
	// moved. A same-state reclaim records only the assignee row — the legacy
	// from==to status row was a schema lie.
	var changes []model.FieldChange
	if fromStatus != toStatus {
		changes = append(changes, model.FieldChange{Field: "status", From: fromStatus, To: toStatus})
	}
	priorClosedAt := issue.ClosedAtValue()
	newClosedAt := updated.ClosedAtValue()
	if !timesEqual(priorClosedAt, newClosedAt) {
		changes = append(changes, model.FieldChange{Field: "closed_at", From: formatNullableTime(priorClosedAt), To: formatNullableTime(newClosedAt)})
	}
	priorResolution := issue.ResolutionValue()
	if !resolutionsEqual(priorResolution, postResolution) {
		changes = append(changes, model.FieldChange{Field: "resolution", From: formatNullableResolution(priorResolution), To: formatNullableResolution(postResolution)})
	}
	priorRedirect := issue.RedirectTargetValue()
	if !stringPointersEqual(priorRedirect, postRedirect) {
		changes = append(changes, model.FieldChange{Field: "redirect_target", From: formatNullableString(priorRedirect), To: formatNullableString(postRedirect)})
	}
	if priorAssignee != postAssignee {
		changes = append(changes, model.FieldChange{Field: "assignee", From: priorAssignee, To: postAssignee})
	}
	updated.UpdatedAt = now
	// updated already carries the leaf the write will persist — the outcome
	// traveled through the machine, so there is no post-hoc rehydration to
	// re-attach a resolution. [LAW:one-source-of-truth]
	return transitionWrite{
		issueID:           issue.ID,
		fromStatus:        fromStatus,
		toStatus:          toStatus,
		postAssignee:      postAssignee,
		now:               now,
		closedAtArg:       closedAtArg,
		resolutionArg:     resolutionArg,
		redirectTargetArg: redirectTargetArg,
		action:            action.Name(),
		reason:            reason,
		actor:             actor,
		changes:           changes,
		post:              updated,
	}, nil
}

// applyTransitionTx writes a planned status transition against tx: the guarded
// status UPDATE and the change event. It owns only writes, so it composes into
// any transaction a caller already holds — which is how Apply folds a
// transition and a field write into one commit. [LAW:single-enforcer]
// The redirect target rides the same UPDATE as status/closed_at/resolution, so
// a reopen clears the whole close payload atomically — a stale redirect on a
// live issue is unwritable, not guarded against. [LAW:one-source-of-truth]
func (s *Store) applyTransitionTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	// [LAW:dataflow-not-control-flow] Status transitions always execute one guarded write; contention is modeled by affected row count.
	result, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, assignee = ?, updated_at = ?, closed_at = ?, resolution = ?, redirect_target = ? WHERE id = ? AND status = ?`,
		w.toStatus, w.postAssignee, w.now.Format(time.RFC3339Nano), w.closedAtArg, w.resolutionArg, w.redirectTargetArg, w.issueID, w.fromStatus)
	if err != nil {
		return fmt.Errorf("update issue status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read status transition result: %w", err)
	}
	if affected == 0 {
		currentStatus, lookupErr := currentStatusTx(ctx, tx, w.issueID)
		if lookupErr != nil {
			return lookupErr
		}
		return fmt.Errorf("%s conflict: issue status is %q", w.action, currentStatus)
	}
	return s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.changes)
}

// retentionWrite is a fully-planned retention transition, the retention-axis
// variant of lifecycleWrite. planRetentionTransition is the compute half: the
// Retain transition table derives the post-state (or the rejection), and the
// change-row diff is taken on the wire projection of the Retention value so
// the history format is the same encoding the columns store. applyTx is the
// write half: one guarded UPDATE plus the event. The prior column pair rides
// along as the compare-and-swap guard, so a plan computed against a stale
// snapshot fails loudly instead of last-write-wins. [LAW:no-silent-failure]
type retentionWrite struct {
	issueID string
	now     time.Time
	// priorArchived/priorDeleted are the CAS guard: the column pair the plan
	// read; the UPDATE matches them or affects no row. nextArchived/nextDeleted
	// are the pair Retain produced. All four are retentionColumns projections.
	priorArchived any
	priorDeleted  any
	nextArchived  any
	nextDeleted   any
	action        model.ActionName
	reason        string
	actor         string
	changes       []model.FieldChange
	post          model.Issue
}

func (w retentionWrite) postIssue() model.Issue { return w.post }

// isNoop is always false: the Retain table has no same-state success cell —
// re-archiving an archived issue is a rejection, not a silent no-op — so every
// planned retention transition owes a write.
func (w retentionWrite) isNoop() bool { return false }

// planRetentionTransition plans a retention action against issue. The whole
// retention state machine — legal moves, rejection reasons, the
// delete-on-archived stamp drop — is the pure Retain transition table; this
// plan supplies the clock and projects the result into column values.
// [LAW:single-enforcer] The action arrives as the sealed sum's variant and
// adapts to Retain via its Name(); the four non-StatusAction variants are
// exactly the four retention actions, so Retain's unsupported-action row is
// unreachable from this seam.
func planRetentionTransition(issue model.Issue, actor string, reason string, action model.Action) (retentionWrite, error) {
	now := time.Now().UTC()
	priorArchivedAt, priorDeletedAt := model.RetentionTimestamps(issue.Retention())
	priorArchived, priorDeleted := retentionColumns(issue)
	next, err := model.Retain(issue.Retention(), action.Name(), now)
	if err != nil {
		return retentionWrite{}, err
	}
	post := issue
	post.SetRetention(next)
	post.UpdatedAt = now
	nextArchivedAt, nextDeletedAt := model.RetentionTimestamps(next)
	nextArchived, nextDeleted := retentionColumns(post)
	var changes []model.FieldChange
	if !timesEqual(priorArchivedAt, nextArchivedAt) {
		changes = append(changes, model.FieldChange{Field: "archived_at", From: formatNullableTime(priorArchivedAt), To: formatNullableTime(nextArchivedAt)})
	}
	if !timesEqual(priorDeletedAt, nextDeletedAt) {
		changes = append(changes, model.FieldChange{Field: "deleted_at", From: formatNullableTime(priorDeletedAt), To: formatNullableTime(nextDeletedAt)})
	}
	return retentionWrite{
		issueID:       issue.ID,
		now:           now,
		priorArchived: priorArchived,
		priorDeleted:  priorDeleted,
		nextArchived:  nextArchived,
		nextDeleted:   nextDeleted,
		action:        action.Name(),
		reason:        reason,
		actor:         actor,
		changes:       changes,
		post:          post,
	}, nil
}

// applyTx writes a planned retention transition against tx. The UPDATE sets
// only the columns the retention axis owns — updated_at plus the pair — so a
// concurrent status change can never be clobbered by a stale restatement, and
// it is guarded on the pair it planned from with null-safe equality (the
// columns are the exact RFC3339Nano strings retentionColumns writes), the same
// affected-rows contention discipline the status write uses.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
func (w retentionWrite) applyTx(ctx context.Context, s *Store, tx *sql.Tx) error {
	result, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at = ?, archived_at = ?, deleted_at = ? WHERE id = ? AND archived_at <=> ? AND deleted_at <=> ?`,
		w.now.Format(time.RFC3339Nano), w.nextArchived, w.nextDeleted, w.issueID, w.priorArchived, w.priorDeleted)
	if err != nil {
		return fmt.Errorf("update issue retention: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read retention transition result: %w", err)
	}
	if affected == 0 {
		current, lookupErr := currentRetentionTx(ctx, tx, w.issueID)
		if lookupErr != nil {
			return lookupErr
		}
		return fmt.Errorf("%s conflict: issue retention is %q", w.action, retentionWord(current))
	}
	return s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.changes)
}

// validateRedirectTarget is the close-validation floor for the redirect
// target the post-transition leaf carries. A NEW redirecting close must carry
// its target (the CLI requires --of; this is the store's integrity floor for
// any other caller — the leaf type alone admits a target-less redirecting
// close because legacy rows genuinely occupy that state). A present target is
// validated the way AddRelation validates endpoints: it must exist and cannot
// be the issue itself, so the redirect points at a real, distinct ticket.
// [LAW:single-enforcer] The one validation site for every close path.
func (s *Store) validateRedirectTarget(ctx context.Context, closingID string, resolution *model.Resolution, target *string) error {
	if target == nil {
		if resolution != nil && resolution.RedirectsToCanonical() {
			return fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)
		}
		return nil
	}
	if *target == closingID {
		return fmt.Errorf("cannot redirect %s to itself", closingID)
	}
	if _, err := s.GetIssue(ctx, *target); err != nil {
		return err
	}
	return nil
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

// currentRetentionTx reads the live retention state of issueID inside tx, for
// naming the winner when a retention CAS fails. The pair decodes through the
// single RetentionFromTimestamps boundary like every other read.
func currentRetentionTx(ctx context.Context, tx *sql.Tx, issueID string) (model.Retention, error) {
	var archivedAt, deletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT archived_at, deleted_at FROM issues WHERE id = ?`, issueID).Scan(&archivedAt, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundError{Entity: "issue", ID: issueID}
		}
		return nil, fmt.Errorf("read issue retention: %w", err)
	}
	archived, err := scanNullableTime(archivedAt)
	if err != nil {
		return nil, err
	}
	deleted, err := scanNullableTime(deletedAt)
	if err != nil {
		return nil, err
	}
	return model.RetentionFromTimestamps(archived, deleted), nil
}

// retentionWord renders a Retention state for human-facing conflict messages.
func retentionWord(r model.Retention) string {
	switch r.(type) {
	case model.Live:
		return "live"
	case model.Archived:
		return "archived"
	case model.Deleted:
		return "deleted"
	default:
		// [LAW:no-silent-failure] Same refusal as RetentionTimestamps: an
		// impostor named either way would mislabel the conflict.
		panic(fmt.Sprintf("illegal Retention value %T", r))
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

func (s *Store) ensureMetaValue(ctx context.Context, guard *snapshotGuard, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if current == value {
		return false, nil
	}
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("ensure meta %s: %w", key, err)
	}
	if err := s.setMeta(ctx, nil, key, value); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ensureMetaDefault(ctx context.Context, guard *snapshotGuard, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) != "" {
		return false, nil
	}
	if _, err := guard.ensure(); err != nil {
		return false, fmt.Errorf("ensure meta %s default: %w", key, err)
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

// recordEvent writes one issue_events row plus N issue_event_changes rows.
// [LAW:single-enforcer] Single insertion point for issue history. Every
// mutation site computes its field-change diff and routes through here.
func (s *Store) recordEvent(ctx context.Context, tx *sql.Tx, issueID, action, reason, actor string, changes []model.FieldChange) error {
	event := model.IssueEvent{
		ID:        "evt-" + uuid.NewString(),
		IssueID:   issueID,
		Action:    strings.TrimSpace(action),
		Reason:    strings.TrimSpace(reason),
		Actor:     strings.TrimSpace(actor),
		CreatedAt: time.Now().UTC(),
		Changes:   changes,
	}
	if event.Actor == "" {
		event.Actor = "unknown"
	}
	var actionArg any
	if event.Action != "" {
		actionArg = event.Action
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert issue event: %w", err)
	}
	for _, change := range changes {
		field := strings.TrimSpace(change.Field)
		if field == "" {
			return fmt.Errorf("issue event %s: field name cannot be empty", event.ID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES (?, ?, ?, ?)`,
			event.ID, field, nullableString(change.From), nullableString(change.To)); err != nil {
			return fmt.Errorf("insert issue event change %s.%s: %w", event.ID, field, err)
		}
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

func (s *Store) listEvents(ctx context.Context, issueID string) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, "e.issue_id = ?", issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue events: %w", err)
	}
	return events, nil
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

func (s *Store) listAllEvents(ctx context.Context) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("list all issue events: %w", err)
	}
	return events, nil
}

// queryEvents fetches issue_events joined with issue_event_changes in a single
// LEFT JOIN query, collapsing the per-change rows back into IssueEvent.Changes
// slices. whereClause is an optional SQL fragment applied after the JOIN (e.g.
// "e.issue_id = ?"); pass "" for an unfiltered scan. [LAW:dataflow-not-control-flow]
func (s *Store) queryEvents(ctx context.Context, whereClause string, args ...any) ([]model.IssueEvent, error) {
	q := `SELECT e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, c.field, c.from_value, c.to_value
		FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id`
	if whereClause != "" {
		q += " WHERE " + whereClause
	}
	q += " ORDER BY e.created_at ASC, e.id ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Ordered list of events; idx maps event ID to its position in out so each
	// change row can be appended to the right event without a second query.
	out := []model.IssueEvent{}
	idx := map[string]int{}
	for rows.Next() {
		var evtID, evtIssueID, evtReason, evtActor, evtCreatedAt string
		var action, cField, cFrom, cTo sql.NullString
		if err := rows.Scan(&evtID, &evtIssueID, &action, &evtReason, &evtActor, &evtCreatedAt, &cField, &cFrom, &cTo); err != nil {
			return nil, err
		}
		i, seen := idx[evtID]
		if !seen {
			t, err := scanTime(evtCreatedAt)
			if err != nil {
				return nil, err
			}
			event := model.IssueEvent{
				ID:        evtID,
				IssueID:   evtIssueID,
				Reason:    evtReason,
				Actor:     evtActor,
				CreatedAt: t,
				Changes:   []model.FieldChange{},
			}
			if action.Valid {
				event.Action = action.String
			}
			i = len(out)
			idx[evtID] = i
			out = append(out, event)
		}
		if cField.Valid {
			change := model.FieldChange{Field: cField.String}
			if cFrom.Valid {
				change.From = cFrom.String
			}
			if cTo.Valid {
				change.To = cTo.String
			}
			out[i].Changes = append(out[i].Changes, change)
		}
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
	Assignee    string
	Rank        string
	Lane        string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Retention   model.Retention
}

// nextRankForPlacement resolves a new issue's rank from its requested
// placement. The single dispatch point on RankPlacement: the two edge helpers
// stay branch-free, and the runtime variability (which edge the caller chose)
// lives in this one exhaustive match.
func nextRankForPlacement(ctx context.Context, tx *sql.Tx, p RankPlacement) (string, error) {
	switch p {
	case RankTop:
		return nextRankAtTop(ctx, tx)
	case RankBottom:
		return nextRankAtBottom(ctx, tx)
	default:
		return "", fmt.Errorf("unknown rank placement: %d", p)
	}
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

// nextRankAtTop returns a rank that sorts before all existing items.
// Called within a transaction to ensure consistency.
func nextRankAtTop(ctx context.Context, tx *sql.Tx) (string, error) {
	var firstRank sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' ORDER BY item_rank ASC LIMIT 1").Scan(&firstRank)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query first rank: %w", err)
	}
	if !firstRank.Valid || firstRank.String == "" {
		return rank.Initial(), nil
	}
	return rank.Before(firstRank.String), nil
}

// issueColumns is the single authoritative ordered projection of the issues
// table. Every read site selects exactly these columns in this order, and the
// positional scanners below (scanIssue, scanIssueWithParent) consume them by
// position — each scanner's Scan() argument order MUST match this slice. A
// column added, removed, or reordered is one edit here that flows to every read
// site through the derived projections, leaving only the two adjacent scanners
// to move with it. [LAW:one-source-of-truth] [FRAMING:representation]
var issueColumns = []string{
	"id", "title", "description", "agent_prompt", "status", "priority",
	"issue_type", "topic", "assignee", "item_rank", "lane", "created_at",
	"updated_at", "closed_at", "resolution", "redirect_target", "archived_at", "deleted_at",
}

// issueProjection renders issueColumns as a SELECT list. A non-empty alias
// qualifies every column (alias "i" yields "i.id, i.title, ..."); the empty
// alias yields the bare form for single-table reads.
func issueProjection(alias string) string {
	qualifier := ""
	if alias != "" {
		qualifier = alias + "."
	}
	cols := make([]string, len(issueColumns))
	for i, c := range issueColumns {
		cols[i] = qualifier + c
	}
	return strings.Join(cols, ", ")
}

// Derived once from issueColumns: the bare form for single-table reads and the
// "i."-qualified form for reads that join the relations table.
var (
	issueColumnsBare      = issueProjection("")
	issueColumnsQualified = issueProjection("i")
)

// scanIssue and scanIssueWithParent consume a row positionally; the Scan()
// argument order below is the other half of issueColumns' positional contract
// and must stay in lockstep with that slice.
func scanIssue(row issueScanner) (issueRow, error) {
	var issue partialIssue
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, resolution, redirectTarget, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&issue.ID, &issue.Title, &issue.Description, &prompt, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &issue.Lane, &createdAt, &updatedAt, &closedAt, &resolution, &redirectTarget, &archivedAt, &deletedAt); err != nil {
		return issueRow{}, err
	}
	issue.Prompt = prompt.String
	return parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, resolution, redirectTarget, archivedAt, deletedAt)
}

func scanIssueWithParent(row issueScanner) (string, issueRow, error) {
	var parentID string
	var issue partialIssue
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, resolution, redirectTarget, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&parentID, &issue.ID, &issue.Title, &issue.Description, &prompt, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &issue.Lane, &createdAt, &updatedAt, &closedAt, &resolution, &redirectTarget, &archivedAt, &deletedAt); err != nil {
		return "", issueRow{}, err
	}
	issue.Prompt = prompt.String
	parsed, err := parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, resolution, redirectTarget, archivedAt, deletedAt)
	return parentID, parsed, err
}

func parsedIssueRow(issue partialIssue, status sql.NullString, assignee string, createdAt string, updatedAt string, closedAt sql.NullString, resolution sql.NullString, redirectTarget sql.NullString, archivedAt sql.NullString, deletedAt sql.NullString) (issueRow, error) {
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
	issue.Assignee = assignee
	statusView := model.StatusView{Value: model.State(status.String)}
	if closedAt.Valid {
		t, err := scanTime(closedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		statusView.ClosedAt = &t
	}
	if resolution.Valid {
		// Raw conversion on a read boundary: the bytes were sealed by
		// ParseResolution at write time and the DB CHECK rejects anything else, so
		// this salvage path conserves them without re-parsing. NewStatus drops the
		// resolution for any non-closed state, so a stray value cannot leak onto a
		// non-closed leaf. [LAW:types-are-the-program]
		r := model.Resolution(resolution.String)
		statusView.Resolution = &r
	}
	if redirectTarget.Valid {
		// Same salvage contract as resolution: the value was validated at the
		// write boundary and the DB CHECK pins it to a redirecting resolution;
		// NewStatus drops it for any non-redirecting pairing.
		t := redirectTarget.String
		statusView.RedirectTarget = &t
	}
	var archivedTime, deletedTime *time.Time
	if archivedAt.Valid {
		t, err := scanTime(archivedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		archivedTime = &t
	}
	if deletedAt.Valid {
		t, err := scanTime(deletedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		deletedTime = &t
	}
	issue.Retention = model.RetentionFromTimestamps(archivedTime, deletedTime)
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

// scanNullableTime parses a nullable RFC3339Nano timestamp column: SQL NULL is
// the absent state (nil), anything else must parse.
func scanNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	t, err := scanTime(value.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
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
			Assignee:    row.Issue.Assignee,
			Rank:        row.Issue.Rank,
			Lane:        row.Issue.Lane,
			Labels:      labelsByID[row.Issue.ID],
			CreatedAt:   row.Issue.CreatedAt,
			UpdatedAt:   row.Issue.UpdatedAt,
		}
		base.SetRetention(row.Issue.Retention)
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
	rows, err := s.db.QueryContext(ctx, `SELECT r.dst_id, `+issueColumnsQualified+`
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
	// [LAW:dataflow-not-control-flow] Hydrate every epic's children in a single
	// pass rather than once per epic. A parallel parentID slice carries the epic
	// each child row belongs to, so the per-recursion-level query count is fixed
	// regardless of how many epics are open instead of scaling as one label query
	// plus one child-relation query per epic. hydrateIssues preserves input order
	// and the SELECT groups children by epic (dst_id) then item_rank, so
	// re-bucketing the hydrated result by parentID reproduces the identical
	// per-epic, rank-ordered grouping the per-epic loop produced.
	childRows := make([]issueRow, 0)
	parentIDs := make([]string, 0)
	for rows.Next() {
		parentID, child, err := scanIssueWithParent(rows)
		if err != nil {
			return nil, err
		}
		childRows = append(childRows, child)
		parentIDs = append(parentIDs, parentID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hydrated, err := s.hydrateIssues(ctx, childRows)
	if err != nil {
		return nil, err
	}
	for i, issue := range hydrated {
		out[parentIDs[i]] = append(out[parentIDs[i]], issue)
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
		return "", ValidationError{Message: "issue type must be task, feature, bug, chore, or epic"}
	}
	return trimmed, nil
}

// canonicalPriority is the single authority on the priority domain: it maps any
// raw int onto the canonical {normal, urgent} set and is idempotent, so its
// fixed points ARE the legal priorities. validatePriority (live writes) and the
// import boundary (legacy restores) are both defined in terms of it, so they
// cannot disagree about what a legal priority is — extending the domain is a
// one-function edit here. [LAW:one-source-of-truth] [LAW:single-enforcer]
func canonicalPriority(priority int) int {
	if priority == model.PriorityUrgent {
		return model.PriorityUrgent
	}
	return model.PriorityNormal
}

// validatePriority rejects exactly the values canonicalPriority would rewrite —
// i.e. anything that is not already canonical. [LAW:single-enforcer] The live
// write path rejects where the import path coerces, but both read "what is a
// legal priority" from canonicalPriority, so the two resolutions stay in lockstep.
func validatePriority(priority int) error {
	if canonicalPriority(priority) != priority {
		return ValidationError{Message: fmt.Sprintf("priority must be %d (normal) or %d (urgent)", model.PriorityNormal, model.PriorityUrgent)}
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.Format(time.RFC3339Nano)
}

// retentionColumns projects an issue's Retention into the two physical
// archived_at/deleted_at column values. Every SQL statement that writes the
// pair binds this function's outputs, and its input cannot represent
// archived-and-deleted, so the both-set row is unwritable from this codebase.
// [LAW:single-enforcer] The one feeder of the two columns.
func retentionColumns(issue model.Issue) (archivedAt, deletedAt any) {
	a, d := model.RetentionTimestamps(issue.Retention())
	return nullableTime(a), nullableTime(d)
}

// nullableResolution stores a nil resolution as SQL NULL so a closed-without-
// resolution row (a `done` close, legacy, or merged) reads back as the absent
// state, never the empty string.
func nullableResolution(value *model.Resolution) any {
	if value == nil {
		return nil
	}
	return string(*value)
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

// formatNullableTime renders a *time.Time for storage in a FieldChange's
// from/to value: nil → "" (SQL NULL via nullableString), non-nil → RFC3339Nano.
func formatNullableTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

// timesEqual compares two *time.Time pointers by value (both nil is equal,
// nil vs non-nil is not, two non-nil compared with .Equal).
func timesEqual(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}

func formatNullableResolution(value *model.Resolution) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

// resolutionsEqual compares two *model.Resolution pointers by value, the same
// nil-aware contract as timesEqual so the event-change diff treats "no
// resolution" symmetrically on both sides.
func resolutionsEqual(a, b *model.Resolution) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// formatNullableString renders an optional string for a FieldChange's from/to
// value: nil → "" (SQL NULL via nullableString), matching formatNullableTime.
func formatNullableString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// stringPointersEqual compares two *string by value, the same nil-aware
// contract as timesEqual/resolutionsEqual.
func stringPointersEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// nullableStringPtr stores a nil optional string as SQL NULL, the pointer
// counterpart of nullableResolution.
func nullableStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
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
