-- +goose Up
-- +goose StatementBegin
-- redirect_target is the canonical ticket a duplicate/superseded close
-- redirects to — the persisted payload of the redirecting resolutions, stored
-- beside resolution so one row (and one guarded UPDATE) owns the whole close
-- payload. It is NULL on every non-redirecting row. Before this column the
-- redirect was written as a generic related-to edge and re-derived at read
-- time by a lossy heuristic; the column makes the redirect a stored fact and
-- returns related-to to meaning exactly one thing: a manual peer link.
ALTER TABLE issues ADD COLUMN redirect_target VARCHAR(191) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
-- The CHECK is one-directional on purpose: a redirecting resolution with an
-- unknown target stays representable (legacy closes and backfill-ambiguous
-- rows below genuinely occupy that state); the write boundary requires a
-- target for every new redirecting close. Like issues_resolution_check this
-- is defense in depth behind the Go-side single validation path.
ALTER TABLE issues ADD CONSTRAINT issues_redirect_target_check CHECK (redirect_target IS NULL OR resolution IN ('duplicate','superseded'));
-- +goose StatementEnd
-- +goose StatementBegin
-- Backfill: a redirecting close used to write exactly one related-to edge, so
-- for rows where exactly one such edge is incident the counterpart is provably
-- the recorded redirect and moves into the column. Rows with any other edge
-- count are left NULL with their edges intact — the pre-column reader could
-- not distinguish the redirect there either, so this is exact rendering
-- parity, not new loss.
UPDATE issues i
SET redirect_target = (
  SELECT IF(r.src_id = i.id, r.dst_id, r.src_id)
  FROM relations r
  WHERE r.type = 'related-to' AND (r.src_id = i.id OR r.dst_id = i.id)
)
WHERE i.resolution IN ('duplicate','superseded')
  AND (
    SELECT COUNT(*)
    FROM relations r2
    WHERE r2.type = 'related-to' AND (r2.src_id = i.id OR r2.dst_id = i.id)
  ) = 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- The backfilled edges were machine-written bookkeeping for the redirect now
-- stored on the row; deleting them is what returns related-to to pure manual
-- peer links. Only edges whose counterpart moved into redirect_target are
-- deleted — ambiguous rows keep theirs.
DELETE r FROM relations r
JOIN issues i ON i.redirect_target IS NOT NULL
  AND ((r.src_id = i.id AND r.dst_id = i.redirect_target)
    OR (r.dst_id = i.id AND r.src_id = i.redirect_target))
WHERE r.type = 'related-to';
-- +goose StatementEnd

-- +goose Down
-- The redirects and the related-to edges are stashed before the Down
-- re-materializes one edge per redirect for the reader below; the next Up's
-- restore (migrations.Restores) writes the redirects back and returns any
-- manual peer edge its backfill read as a redirect.
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
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'redirect_target';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'redirect_target#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', CAST(t.id AS CHAR), 'redirect_target', JSON_ARRAY(SUBSTRING(t.redirect_target, 1)) FROM issues t;
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
DELETE FROM schema_stash WHERE table_name = 'relations' AND column_name = '*related-to';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'relations' AND column_name = '*related-to#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'relations', CONCAT_WS(CHAR(31), CAST(t.src_id AS CHAR), CAST(t.dst_id AS CHAR), CAST(t.type AS CHAR)), '*related-to', JSON_ARRAY(SUBSTRING(t.src_id, 1), SUBSTRING(t.dst_id, 1), SUBSTRING(t.type, 1), SUBSTRING(t.created_at, 1), SUBSTRING(t.created_by, 1)) FROM relations t WHERE t.type = 'related-to';
-- +goose StatementEnd
-- +goose StatementBegin
INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)
SELECT LEAST(i.id, i.redirect_target), GREATEST(i.id, i.redirect_target), 'related-to', COALESCE(i.closed_at, i.updated_at), 'unknown'
FROM issues i
WHERE i.redirect_target IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_redirect_target_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN redirect_target;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', s.row_key, 'redirect_target#image', JSON_ARRAY((SELECT JSON_ARRAY(SUBSTRING(u.status, 1), SUBSTRING(u.closed_at, 1), SUBSTRING(u.resolution, 1)) FROM issues u WHERE CAST(u.id AS CHAR) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'redirect_target';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'relations', s.row_key, '*related-to#image', JSON_ARRAY((SELECT JSON_ARRAY(SUBSTRING(u.src_id, 1), SUBSTRING(u.dst_id, 1), SUBSTRING(u.type, 1), SUBSTRING(u.created_at, 1), SUBSTRING(u.created_by, 1)) FROM relations u WHERE CONCAT_WS(CHAR(31), CAST(u.src_id AS CHAR), CAST(u.dst_id AS CHAR), CAST(u.type AS CHAR)) = s.row_key)) FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '*related-to';
-- +goose StatementEnd
