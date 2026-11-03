-- +goose Up
-- +goose StatementBegin
-- lane partitions an epic's children into parallel rank-ordered sub-sequences:
-- children sharing a lane are sequenced by rank; different lanes run in
-- parallel. The empty-string default is one lane value like any other — every
-- keyless child shares it, reproducing fully-sequential epic behavior. lane is
-- meaningful only within an epic (outside an epic, explicit `blocks` edges are
-- the ordering signal); the gate predicate enforces that scoping, not the column.
ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- Lane is stashed before the drop; the next Up's restore
-- (migrations.Restores) writes it back.
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
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'lane';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'lane#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', CAST(t.id AS CHAR), 'lane', JSON_ARRAY(SUBSTRING(t.lane, 1)) FROM issues t;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN lane;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', s.row_key, 'lane#image', JSON_ARRAY((SELECT JSON_ARRAY(SUBSTRING(u.id, 1)) FROM issues u WHERE u.id = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'lane';
-- +goose StatementEnd
