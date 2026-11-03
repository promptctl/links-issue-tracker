-- +goose Up
-- +goose StatementBegin
-- The attribution pair: which checkout produced this work event. stream_id is
-- the producing checkout's opaque per-worktree token, workspace_id the opaque
-- per-store id it was produced in; together they distinguish two worktrees of
-- one repository across every clone the database reaches. Claims are DERIVED
-- from these stamps at read time — no claim table follows this column.
--
-- NULL is the load-bearing value, not an oversight: events written before this
-- migration carry no attribution and never will (attribution is historical
-- fact, never backfilled), so a freshly upgraded repository derives zero claims
-- and selects work exactly as it did before. Cold start by construction.
-- Nothing user-, host-, or path-shaped may ever enter these columns; the
-- database syncs to shared remotes.
ALTER TABLE issue_events ADD COLUMN stream_id VARCHAR(64) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events ADD COLUMN workspace_id VARCHAR(191) NULL;
-- +goose StatementEnd

-- +goose Down
-- Both stamps are stashed before the drop; the next Up's restore
-- (migrations.Restores) writes them back.
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
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issue_events', CAST(t.id AS CHAR), 'stream_id', JSON_ARRAY(SUBSTRING(t.stream_id, 1)) FROM issue_events t;
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
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issue_events', CAST(t.id AS CHAR), 'workspace_id', JSON_ARRAY(SUBSTRING(t.workspace_id, 1)) FROM issue_events t;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events DROP COLUMN stream_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events DROP COLUMN workspace_id;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issue_events', s.row_key, 'stream_id#image', JSON_ARRAY(EXISTS (SELECT 1 FROM issue_events u WHERE CAST(u.id AS CHAR) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issue_events' AND s.column_name = 'stream_id';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issue_events', s.row_key, 'workspace_id#image', JSON_ARRAY(EXISTS (SELECT 1 FROM issue_events u WHERE CAST(u.id AS CHAR) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issue_events' AND s.column_name = 'workspace_id';
-- +goose StatementEnd
