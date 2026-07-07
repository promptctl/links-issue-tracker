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
-- LOSS CONTRACT: down re-materializes one related-to edge per recorded
-- redirect and then drops the column, so the redirect-vs-manual-peer
-- distinction is lost again by construction — the pre-upgrade reader renders
-- the re-materialized edge exactly as it rendered the original one. Edge
-- created_at is approximated by the close timestamp (falling back to the
-- row's updated_at) and created_by by 'unknown'; the original edge's exact
-- stamps are not preserved. INSERT IGNORE tolerates two known-benign row
-- classes: a manual edge already linking the same pair (the edge the
-- redirect needs already exists), and an FK-gap row — redirect_target has
-- no FK to issues, so a redirect whose canonical row was since hard-deleted
-- cannot re-materialize as an edge (relations' FK would reject it) and is
-- skipped. The pre-column reader never rendered a redirect for a vanished
-- counterpart either, so the skip is parity, not new loss — but it IS a
-- silent skip, accepted here only because a migration has no per-row
-- reporting channel and both classes are enumerated above.
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
