-- +goose Up
-- One row per apply of the project config: the whole text of the committed
-- .lit/config.toml, and who applied it and when. The newest apply is the config
-- every clone runs on; with no row, lit runs on its defaults. An apply is never
-- edited or removed, and it names no issue, so no purge reaches it. Its stamp
-- is spelled as v12 spells every stored timestamp, and the same CHECK holds it
-- to that spelling.
-- +goose StatementBegin
CREATE TABLE config_applies (
  id VARCHAR(191) NOT NULL,
  content TEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (id),
  CONSTRAINT config_applies_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19))
);
-- +goose StatementEnd

-- A lit below v13 writes on the checkout's file, not the applied config, so
-- the rules the config in force sets would not hold on its writes.
-- +goose StatementBegin
INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', '13')
ON DUPLICATE KEY UPDATE meta_value = '13';
-- +goose StatementEnd

-- +goose Down
-- The applies are stashed before the drop; the next Up's restore
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
DELETE FROM schema_stash WHERE table_name = 'config_applies' AND column_name = '*';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'config_applies' AND column_name = '*#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'config_applies', CAST(t.id AS CHAR), '*', JSON_ARRAY(SUBSTRING(t.id, 1), SUBSTRING(t.content, 1), SUBSTRING(t.created_at, 1), SUBSTRING(t.created_by, 1)) FROM config_applies t;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE config_applies;
-- +goose StatementEnd
-- v12 declared the minimum writer schema this one raised.
-- +goose StatementBegin
INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', '12')
ON DUPLICATE KEY UPDATE meta_value = '12';
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'config_applies', s.row_key, '*#image', JSON_ARRAY(NULL) FROM schema_stash s WHERE s.table_name = 'config_applies' AND s.column_name = '*';
-- +goose StatementEnd
