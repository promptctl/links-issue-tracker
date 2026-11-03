-- +goose Up
-- +goose StatementBegin
-- resolution is the sealed close reason — why a ticket was closed without being
-- finished: duplicate/superseded (redirect to a canonical ticket), obsolete (the
-- need is gone), wontfix (a standing decision). It is the payload of the closed
-- lifecycle state and is NULL on every non-closed row and on a `done` close (a
-- success carries no why-not). The Go type makes a resolution on a non-closed
-- state unrepresentable; this column is its persisted projection. The CHECK seals
-- the value set at the DB boundary, mirroring relations_type_check — the parse
-- boundary (ParseResolution) is the primary gate, this is defense in depth.
ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_resolution_check CHECK (resolution IS NULL OR resolution IN ('duplicate','superseded','obsolete','wontfix'));
-- +goose StatementEnd

-- +goose Down
-- Resolution is stashed before the drop; the next Up's restore
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
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'resolution';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'resolution#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', CAST(t.id AS CHAR), 'resolution', JSON_ARRAY(SUBSTRING(t.resolution, 1)) FROM issues t;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_resolution_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN resolution;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', s.row_key, 'resolution#image', JSON_ARRAY((SELECT JSON_ARRAY(SUBSTRING(u.status, 1), SUBSTRING(u.closed_at, 1)) FROM issues u WHERE CAST(u.id AS CHAR) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'resolution';
-- +goose StatementEnd
