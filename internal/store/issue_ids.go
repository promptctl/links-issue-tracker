package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
)

// newIssueID mints the id for a new issue. Top-level and child ids differ only
// in the namespace they hang under and the population that sets their hash
// length; the minting rule itself is one function in issueid, reached the same
// way from both. [LAW:one-type-per-behavior]
func newIssueID(ctx context.Context, tx *sql.Tx, prefix string, topic string, title string, description string, createdBy string, createdAt time.Time, parentID string) (string, error) {
	content := issueid.Content{
		Topic:       topic,
		Title:       title,
		Description: description,
		Creator:     createdBy,
		CreatedAt:   createdAt,
	}
	namespace, population, err := idSpace(ctx, tx, prefix, topic, parentID)
	if err != nil {
		return "", err
	}
	return issueid.Mint(namespace, content, population, func(candidate string) (bool, error) {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, candidate).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	})
}

// idSpace resolves which id-space a new issue is minted into and how populated
// that space already is. A parent names its own child space; the absence of one
// names the workspace's top-level space.
//
// The population is a starting hash length, never a position: nothing is
// counted to derive the id itself, which is the whole point — a count taken
// over the LOCAL rows is a claim about every row that exists anywhere, and two
// disconnected stores holding the same rows make that claim identically.
// [LAW:one-source-of-truth]
func idSpace(ctx context.Context, tx *sql.Tx, prefix, topic, parentID string) (issueid.Namespace, int, error) {
	if strings.TrimSpace(parentID) == "" {
		population, err := countTopLevelIssues(ctx, tx)
		if err != nil {
			return "", 0, err
		}
		return issueid.TopLevelNamespace(prefix, topic), population, nil
	}
	population, err := countChildren(ctx, tx, parentID)
	if err != nil {
		return "", 0, err
	}
	return issueid.ChildNamespace(parentID), population, nil
}

// countChildren counts the direct children already recorded under parentID.
// Parentage is read from the relations table, the only place it lives — an id
// prefix scan would also sweep up grandchildren and would take the id shape as
// evidence of structure it does not own. [LAW:one-source-of-truth]
func countChildren(ctx context.Context, tx *sql.Tx, parentID string) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations WHERE dst_id = ? AND type = 'parent-child'`, parentID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count children of %s: %w", parentID, err)
	}
	return count, nil
}

// countTopLevelIssues counts the ids in the workspace's top-level space.
// [LAW:dataflow-not-control-flow] Adaptive length is a pure function of the
// population. The prefix never gates the count: every issue in a workspace
// shares one generation-time prefix, and even after a rename the collision
// space we care about is "all top-level IDs in this DB" — counting across
// prefixes is conservative (slightly longer hashes) and never wrong.
func countTopLevelIssues(ctx context.Context, tx *sql.Tx) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id NOT LIKE ?`, "%.%").Scan(&count); err != nil {
		return 0, fmt.Errorf("count top-level issues: %w", err)
	}
	return count, nil
}
