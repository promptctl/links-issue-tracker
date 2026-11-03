-- +goose Up
-- One row per purged issue: the id and the instant the issue was created, which
-- together name the ticket, and when and by whom it was purged — never the
-- content. A purge deletes the issue's row, and the ON DELETE CASCADE on every
-- child table takes its comments, labels, history and edges with it; this row
-- is what remains, so that a merge drops the ticket from every side, a restore
-- does not bring it back, and id minting never hands its id out again. Neither
-- key column carries a foreign key: the issue it names no longer exists, by
-- definition.
-- +goose StatementBegin
CREATE TABLE purges (
  issue_id VARCHAR(191) NOT NULL,
  issue_created_at VARCHAR(64) NOT NULL,
  purged_at VARCHAR(64) NOT NULL,
  purged_by TEXT NOT NULL,
  PRIMARY KEY (issue_id, issue_created_at)
);
-- +goose StatementEnd

-- +goose Down
-- The purge records are stashed before the drop; the next Up's restore
-- (migrations.Restores) re-inserts them.
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
DELETE FROM schema_stash WHERE table_name = 'purges' AND column_name = '*';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'purges' AND column_name = '*#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'purges', CONCAT_WS(CHAR(31), CAST(t.issue_id AS CHAR), CAST(t.issue_created_at AS CHAR)), '*', JSON_ARRAY(SUBSTRING(t.issue_id, 1), SUBSTRING(t.issue_created_at, 1), SUBSTRING(t.purged_at, 1), SUBSTRING(t.purged_by, 1)) FROM purges t;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE purges;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'purges', s.row_key, '*#image', JSON_ARRAY(NULL) FROM schema_stash s WHERE s.table_name = 'purges' AND s.column_name = '*';
-- +goose StatementEnd
