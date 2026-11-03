-- +goose Up
-- work_plan is the ticket's plan: how its work will be done, written before
-- the work starts so a reviewer can judge it against the laws. The empty text
-- is a ticket with no plan. Each edit is recorded in the history like any
-- field's, and those recorded texts are the plan's versions. The column is not
-- named plan because PLAN is a reserved word in the SQL dialect.
-- +goose StatementBegin
ALTER TABLE issues ADD COLUMN work_plan text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- One row per law review of a ticket's plan: the verdict, the laws it cites,
-- who gave it and when. plan_digest is the hex SHA-256 of the plan text the
-- review judged. It names the text rather than its version number because the
-- numbers follow the history's order, and a sync that merges in an older edit
-- renumbers the versions after it. laws is the cited law tokens, sorted and
-- joined by commas; a token holds no comma. A review is never edited, and it
-- cascades away with its issue on a purge.
-- +goose StatementBegin
CREATE TABLE plan_reviews (
  id VARCHAR(191) NOT NULL,
  issue_id VARCHAR(191) NOT NULL,
  plan_digest VARCHAR(64) NOT NULL,
  verdict VARCHAR(16) NOT NULL,
  laws TEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_plan_reviews_issue_created (issue_id, created_at),
  CONSTRAINT plan_reviews_issue FOREIGN KEY (issue_id) REFERENCES issues (id) ON DELETE CASCADE,
  CONSTRAINT plan_reviews_verdict_check CHECK (verdict IN ('pass','fail'))
);
-- +goose StatementEnd

-- +goose Down
-- Plans and plan reviews are stashed before the drop; the next Up's
-- restore (migrations.Restores) writes them back.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS schema_stash (
  table_name VARCHAR(64) NOT NULL,
  row_key VARCHAR(767) NOT NULL,
  column_name VARCHAR(64) NOT NULL,
  value JSON NOT NULL,
  PRIMARY KEY (table_name, row_key, column_name)
);
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', CAST(t.id AS CHAR), 'work_plan', JSON_ARRAY(SUBSTRING(t.work_plan, 1)) FROM issues t;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS schema_stash (
  table_name VARCHAR(64) NOT NULL,
  row_key VARCHAR(767) NOT NULL,
  column_name VARCHAR(64) NOT NULL,
  value JSON NOT NULL,
  PRIMARY KEY (table_name, row_key, column_name)
);
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'plan_reviews', CAST(t.id AS CHAR), '*', JSON_ARRAY(SUBSTRING(t.id, 1), SUBSTRING(t.issue_id, 1), SUBSTRING(t.plan_digest, 1), SUBSTRING(t.verdict, 1), SUBSTRING(t.laws, 1), SUBSTRING(t.created_at, 1), SUBSTRING(t.created_by, 1)) FROM plan_reviews t;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE plan_reviews;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN work_plan;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', s.row_key, 'work_plan#image', JSON_ARRAY(EXISTS (SELECT 1 FROM issues u WHERE CAST(u.id AS CHAR) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'work_plan';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'plan_reviews', s.row_key, '*#image', JSON_ARRAY(NULL) FROM schema_stash s WHERE s.table_name = 'plan_reviews' AND s.column_name = '*';
-- +goose StatementEnd
