-- +goose Up
-- +goose StatementBegin
-- is_top_level_id says this row's id has no dot, the shape every reader of an
-- id keys on. It is a predicate, so it is not an identifier despite the suffix,
-- and it is not "is a top-level issue": that is the frame of issues no container
-- holds (storage.TopLevel), and a dotless id can sit inside a container. The
-- engine derives it from id and no writer holds it. The index lets a create size
-- its id without scanning issues. STORED, because a VIRTUAL column makes every
-- plain scan of issues compute it.
ALTER TABLE issues ADD COLUMN is_top_level_id TINYINT AS (IF(id LIKE '%.%', 0, 1)) STORED NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_issues_is_top_level_id ON issues (is_top_level_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_issues_is_top_level_id ON issues;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN is_top_level_id;
-- +goose StatementEnd
