package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// timestampColumn is one column the stamp CHECKs of migration v12 guard. scope
// restricts it to the rows that hold a timestamp ("" for all of them): a change
// row's from/to value is a timestamp only for the fields
// model.TimestampChangeFields names.
type timestampColumn struct {
	table, column, scope string
}

// checkName is the name v12 gives the column's CHECK, and names it in the
// refusal of an unspelled value.
func (c timestampColumn) checkName() string {
	return c.table + "_" + c.column + "_stamp"
}

// rows is the predicate selecting the column's timestamps, and nothing else.
func (c timestampColumn) rows() string {
	scope := "TRUE"
	if c.scope != "" {
		scope = c.scope
	}
	return "(" + scope + ") AND " + c.column + " IS NOT NULL"
}

// timestampColumns is every column the stored spelling binds. The migration
// repeats it in SQL, so a test pins each entry to its CHECK.
// [LAW:one-source-of-truth]
func timestampColumns() []timestampColumn {
	changeFields := make([]string, 0, 3)
	for _, field := range model.TimestampChangeFields() {
		changeFields = append(changeFields, "'"+field+"'")
	}
	scope := "field IN (" + strings.Join(changeFields, ",") + ")"
	return []timestampColumn{
		{"issues", "created_at", ""},
		{"issues", "updated_at", ""},
		{"issues", "closed_at", ""},
		{"issues", "archived_at", ""},
		{"issues", "deleted_at", ""},
		{"comments", "created_at", ""},
		{"issue_events", "created_at", ""},
		{"labels", "created_at", ""},
		{"relations", "created_at", ""},
		{"plan_reviews", "created_at", ""},
		{"purges", "issue_created_at", ""},
		{"purges", "purged_at", ""},
		{"issue_event_changes", "from_value", scope},
		{"issue_event_changes", "to_value", scope},
	}
}

// probeTimestampSpelling reads what v12 does: every timestamp not already in
// the stored spelling is one it respells, and an offset spelling is the one
// thing it destroys, since the instant is kept in UTC and the offset is not.
func probeTimestampSpelling(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var respelled, offsets int
	for _, c := range timestampColumns() {
		var inColumn, withOffset int
		if err := db.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COALESCE(SUM(CHAR_LENGTH(%[1]s) <> %[3]d OR RIGHT(%[1]s,1) <> 'Z'),0), COALESCE(SUM(RIGHT(%[1]s,1) <> 'Z'),0) FROM %[2]s WHERE %[4]s`,
			c.column, c.table, len(model.StampLayout), c.rows())).Scan(&inColumn, &withOffset); err != nil {
			return nil, fmt.Errorf("count %s.%s spellings: %w", c.table, c.column, err)
		}
		respelled += inColumn
		offsets += withOffset
	}
	return map[string]any{"migration.v12_stamps_respelled": respelled, "migration.v12_offsets_to_utc": offsets}, nil
}
