package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

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

// writeLabelsTx moves an issue's label rows from the set before to the set
// after: a name only after holds is inserted, authored by author at at; a name
// only before holds is deleted; a name both hold keeps its row, and with it who
// added the label and when. A write rewrites only the rows of the names it
// adds or removes, so it never re-authors a label it did not touch, and two
// writers adding different labels write disjoint rows.
func writeLabelsTx(ctx context.Context, tx *sql.Tx, issueID string, before, after []string, author string, at time.Time) error {
	for _, name := range before {
		if slices.Contains(after, name) {
			continue
		}
		// [LAW:single-enforcer] one labels DELETE, shared with the replay's delta.
		if _, err := deleteLabelTx(ctx, tx, labelKey{issueID: issueID, name: name}); err != nil {
			return err
		}
	}
	for _, name := range after {
		if slices.Contains(before, name) {
			continue
		}
		if err := insertLabelTx(ctx, tx, model.Label{IssueID: issueID, Name: name, CreatedAt: at, CreatedBy: storage.Actor(author)}); err != nil {
			return err
		}
	}
	return nil
}

// canonicalizeLabels delegates to the domain's canonical label-set form.
// [LAW:single-enforcer] The definition lives in model.CanonicalizeLabels; this
// wrapper only keeps the store's callsites short.
func canonicalizeLabels(labels []string) ([]string, error) {
	return model.CanonicalizeLabels(labels)
}
