package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/dolthub/driver"

	"github.com/google/uuid"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

const (
	driverName         = "dolt"
	defaultBeadsDBName = "beads"
	defaultCommitName  = "links-beads"
	defaultCommitEmail = "links-beads@links.local"
)

type Summary struct {
	Issues    int `json:"issues"`
	Relations int `json:"relations"`
	Comments  int `json:"comments"`
	Labels    int `json:"labels"`
}

func Import(ctx context.Context, st *store.Store, beadsDBPath string) (Summary, error) {
	db, _, err := openDoltDatabase(ctx, beadsDBPath, false)
	if err != nil {
		return Summary{}, fmt.Errorf("open beads db: %w", err)
	}
	defer db.Close()
	summary := Summary{}
	rows, err := db.QueryContext(ctx, `SELECT id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at FROM issues WHERE deleted_at IS NULL`)
	if err != nil {
		return Summary{}, fmt.Errorf("query beads issues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var issue store.ImportIssue
		var createdAt, updatedAt string
		var closedAt sql.NullString
		if err := rows.Scan(&issue.ID, &issue.Title, &issue.Description, &issue.Status, &issue.Priority, &issue.IssueType, &issue.Assignee, &createdAt, &updatedAt, &closedAt); err != nil {
			return Summary{}, err
		}
		issue.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return Summary{}, err
		}
		issue.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return Summary{}, err
		}
		if closedAt.Valid {
			t, err := parseTime(closedAt.String)
			if err != nil {
				return Summary{}, err
			}
			issue.ClosedAt = &t
		}
		if err := st.ImportIssue(ctx, issue); err != nil {
			return Summary{}, err
		}
		summary.Issues++
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}

	relRows, err := db.QueryContext(ctx, `SELECT issue_id, depends_on_id, type, created_at, created_by FROM dependencies`)
	if err != nil {
		return Summary{}, fmt.Errorf("query beads dependencies: %w", err)
	}
	defer relRows.Close()
	for relRows.Next() {
		var rel store.ImportRelation
		var createdAt string
		if err := relRows.Scan(&rel.SrcID, &rel.DstID, &rel.Type, &createdAt, &rel.CreatedBy); err != nil {
			return Summary{}, err
		}
		if rel.Type != "blocks" && rel.Type != "parent-child" && rel.Type != "related-to" {
			continue
		}
		rel.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return Summary{}, err
		}
		if err := st.ImportRelation(ctx, rel); err != nil {
			return Summary{}, err
		}
		summary.Relations++
	}
	if err := relRows.Err(); err != nil {
		return Summary{}, err
	}

	commentRows, err := db.QueryContext(ctx, `SELECT issue_id, author, text, created_at FROM comments`)
	if err != nil {
		return Summary{}, fmt.Errorf("query beads comments: %w", err)
	}
	defer commentRows.Close()
	for commentRows.Next() {
		var issueID, author, text, createdAt string
		if err := commentRows.Scan(&issueID, &author, &text, &createdAt); err != nil {
			return Summary{}, err
		}
		t, err := parseTime(createdAt)
		if err != nil {
			return Summary{}, err
		}
		if err := st.ImportComment(ctx, store.ImportComment{
			ID:        "beads-" + uuid.NewString(),
			IssueID:   issueID,
			Body:      text,
			CreatedAt: t,
			CreatedBy: author,
		}); err != nil {
			return Summary{}, err
		}
		summary.Comments++
	}
	if err := commentRows.Err(); err != nil {
		return Summary{}, err
	}
	labelRows, err := db.QueryContext(ctx, `SELECT issue_id, label FROM labels`)
	if err == nil {
		defer labelRows.Close()
		for labelRows.Next() {
			var issueID, label string
			if err := labelRows.Scan(&issueID, &label); err != nil {
				return Summary{}, err
			}
			if err := st.ImportLabel(ctx, store.ImportLabel{
				IssueID:   issueID,
				Name:      label,
				CreatedAt: time.Now().UTC(),
				CreatedBy: "beads",
			}); err != nil {
				return Summary{}, err
			}
			summary.Labels++
		}
		if err := labelRows.Err(); err != nil {
			return Summary{}, err
		}
	}
	return summary, nil
}

func Export(ctx context.Context, st *store.Store, beadsDBPath string) (Summary, error) {
	db, dbName, err := openDoltDatabase(ctx, beadsDBPath, true)
	if err != nil {
		return Summary{}, fmt.Errorf("open beads export db: %w", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE issues (
			id VARCHAR(191) PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			design TEXT NOT NULL DEFAULT '',
			acceptance_criteria TEXT NOT NULL DEFAULT '',
			notes TEXT NOT NULL DEFAULT '',
			status VARCHAR(32) NOT NULL DEFAULT 'open',
			priority INTEGER NOT NULL DEFAULT 2,
			issue_type VARCHAR(32) NOT NULL DEFAULT 'task',
			assignee TEXT,
			estimated_minutes INTEGER,
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT DEFAULT '',
			updated_at VARCHAR(64) NOT NULL,
			closed_at VARCHAR(64),
			closed_by_session TEXT DEFAULT '',
			external_ref TEXT,
			compaction_level INTEGER DEFAULT 0,
			compacted_at VARCHAR(64),
			compacted_at_commit TEXT,
			original_size INTEGER,
			deleted_at VARCHAR(64),
			deleted_by TEXT DEFAULT '',
			delete_reason TEXT DEFAULT '',
			original_type TEXT DEFAULT '',
			sender TEXT DEFAULT '',
			ephemeral INTEGER DEFAULT 0,
			pinned INTEGER DEFAULT 0,
			is_template INTEGER DEFAULT 0,
			mol_type TEXT DEFAULT '',
			event_kind TEXT DEFAULT '',
			actor TEXT DEFAULT '',
			target TEXT DEFAULT '',
			payload TEXT DEFAULT '', source_repo TEXT DEFAULT '.', close_reason TEXT DEFAULT '', await_type TEXT, await_id TEXT, timeout_ns INTEGER, waiters TEXT, hook_bead TEXT DEFAULT '', role_bead TEXT DEFAULT '', agent_state TEXT DEFAULT '', last_activity VARCHAR(64), role_type TEXT DEFAULT '', rig TEXT DEFAULT '', due_at VARCHAR(64), defer_until VARCHAR(64), owner TEXT DEFAULT '', crystallizes INTEGER DEFAULT 0, work_type TEXT DEFAULT 'mutex', source_system TEXT DEFAULT '', quality_score REAL
		);`,
		`CREATE TABLE dependencies (
			issue_id VARCHAR(191) NOT NULL,
			depends_on_id VARCHAR(191) NOT NULL,
			type VARCHAR(32) NOT NULL DEFAULT 'blocks',
			created_at VARCHAR(64) NOT NULL,
			created_by TEXT NOT NULL,
			metadata TEXT,
			thread_id TEXT,
			PRIMARY KEY (issue_id, depends_on_id, type)
		);`,
		`CREATE TABLE comments (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			issue_id VARCHAR(191) NOT NULL,
			author TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at VARCHAR(64) NOT NULL
		);`,
		`CREATE TABLE labels (
			issue_id VARCHAR(191) NOT NULL,
			label VARCHAR(191) NOT NULL,
			PRIMARY KEY (issue_id, label)
		);`,
	} {
		if err := execIgnoreAlreadyExists(ctx, db, stmt); err != nil {
			return Summary{}, fmt.Errorf("prepare export db: %w", err)
		}
	}
	export, err := st.Export(ctx)
	if err != nil {
		return Summary{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Summary{}, err
	}
	defer tx.Rollback()
	for _, table := range []string{"issues", "dependencies", "comments", "labels"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return Summary{}, err
		}
	}
	summary := Summary{}
	for _, issue := range export.Issues {
		var closedAt any
		if issue.ClosedAt != nil {
			closedAt = issue.ClosedAt.Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, assignee, created_at, updated_at, closed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, issue.ID, issue.Title, issue.Description, issue.Status, issue.Priority, issue.IssueType, nullIfEmpty(issue.Assignee), issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt); err != nil {
			return Summary{}, fmt.Errorf("export issue %s: %w", issue.ID, err)
		}
		summary.Issues++
	}
	for _, rel := range export.Relations {
		if rel.Type != "blocks" && rel.Type != "parent-child" && rel.Type != "related-to" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dependencies(issue_id, depends_on_id, type, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, rel.SrcID, rel.DstID, rel.Type, rel.CreatedAt.Format(time.RFC3339Nano), nullString(rel.CreatedBy, "links")); err != nil {
			return Summary{}, fmt.Errorf("export relation %s->%s: %w", rel.SrcID, rel.DstID, err)
		}
		summary.Relations++
	}
	for _, comment := range export.Comments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO comments(issue_id, author, text, created_at) VALUES (?, ?, ?, ?)`, comment.IssueID, nullString(comment.CreatedBy, "links"), comment.Body, comment.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return Summary{}, fmt.Errorf("export comment %s: %w", comment.ID, err)
		}
		summary.Comments++
	}
	for _, label := range export.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label) VALUES (?, ?)`, label.IssueID, label.Name); err != nil {
			return Summary{}, fmt.Errorf("export label %s:%s: %w", label.IssueID, label.Name, err)
		}
		summary.Labels++
	}
	if err := tx.Commit(); err != nil {
		return Summary{}, err
	}
	if err := commitWorkingSet(ctx, db, fmt.Sprintf("beads export (%s)", dbName)); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func openDoltDatabase(ctx context.Context, inputPath string, create bool) (*sql.DB, string, error) {
	rootDir, dbName, err := resolveDoltTarget(inputPath)
	if err != nil {
		return nil, "", err
	}
	if create {
		if err := os.MkdirAll(rootDir, 0o755); err != nil {
			return nil, "", fmt.Errorf("create beads root dir: %w", err)
		}
		bootstrap, err := sql.Open(driverName, buildDoltDSN(rootDir, ""))
		if err != nil {
			return nil, "", fmt.Errorf("open beads bootstrap: %w", err)
		}
		if _, err := bootstrap.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", dbName)); err != nil {
			_ = bootstrap.Close()
			return nil, "", fmt.Errorf("create beads database: %w", err)
		}
		_ = bootstrap.Close()
	}
	db, err := sql.Open(driverName, buildDoltDSN(rootDir, dbName))
	if err != nil {
		return nil, "", fmt.Errorf("open beads database: %w", err)
	}
	return db, dbName, nil
}

func resolveDoltTarget(inputPath string) (string, string, error) {
	clean := filepath.Clean(strings.TrimSpace(inputPath))
	if clean == "" || clean == "." {
		return "", "", errors.New("beads path is required")
	}
	base := filepath.Base(clean)
	ext := filepath.Ext(base)
	isHiddenDirName := strings.HasPrefix(base, ".") && strings.Count(base, ".") == 1
	if ext != "" && !isHiddenDirName {
		rootDir := filepath.Dir(clean)
		dbName := normalizeDatabaseName(strings.TrimSuffix(base, ext))
		if dbName == "" {
			dbName = defaultBeadsDBName
		}
		return rootDir, dbName, nil
	}
	if stat, err := os.Stat(filepath.Join(clean, ".dolt")); err == nil && stat.IsDir() {
		return filepath.Dir(clean), normalizeDatabaseName(base), nil
	}
	return clean, defaultBeadsDBName, nil
}

func buildDoltDSN(rootDir, dbName string) string {
	query := url.Values{}
	query.Set("commitname", defaultCommitName)
	query.Set("commitemail", defaultCommitEmail)
	if strings.TrimSpace(dbName) != "" {
		query.Set("database", normalizeDatabaseName(dbName))
	}
	return "file://" + filepath.ToSlash(filepath.Clean(rootDir)) + "?" + query.Encode()
}

func normalizeDatabaseName(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	if normalized == "" {
		return ""
	}
	reInvalid := regexp.MustCompile(`[^a-z0-9_]`)
	normalized = reInvalid.ReplaceAllString(normalized, "_")
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		return ""
	}
	return normalized
}

func execIgnoreAlreadyExists(ctx context.Context, db *sql.DB, stmt string) error {
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		return err
	}
	return nil
}

func commitWorkingSet(ctx context.Context, db *sql.DB, message string) error {
	var commitHash string
	if err := db.QueryRowContext(ctx, `CALL DOLT_COMMIT('-Am', ?)`, message).Scan(&commitHash); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("commit beads working set: %w", err)
	}
	return nil
}

func parseTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05.999999999-07:00"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}

func nullIfEmpty(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullString(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func FromExport(export model.Export) Summary {
	return Summary{Issues: len(export.Issues), Relations: len(export.Relations), Comments: len(export.Comments), Labels: len(export.Labels)}
}
