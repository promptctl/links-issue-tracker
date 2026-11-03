-- +goose Up
-- +goose StatementBegin
-- follows_up is the ticket this one was filed as a follow-up of: the stored
-- fact behind `lit followup --on`. Before this column the only record of that
-- link was the parent-child edge `lit followup` wrote onto the ticket it
-- followed up, which nested new work inside finished work and turned a closed
-- ticket into an accidental container. A follow-up is the next piece of the
-- same effort, so it belongs beside the ticket it follows up — same parent —
-- and the link itself lives here. It is NULL on every ticket that was not
-- filed as a follow-up, written once at creation, and never edited. No foreign
-- key, as with redirect_target: the followed-up ticket may later be purged and
-- the fact that this one followed it up stays true.
ALTER TABLE issues ADD COLUMN follows_up VARCHAR(191) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
-- Backfill, step one: every child of a closed non-container is read as a
-- follow-up of that parent. `lit followup` is how children land under a closed
-- task, bug, feature or chore, but not the only way — `lit new --parent` and
-- `lit parent set` accept a leaf too — and the rows hold nothing that tells the
-- two apart. Every such child is finished work's child by the same accident,
-- so the move applies to all of them, and the link records the closed leaf as
-- the ticket the child came out of, the only reading the data supports.
-- The parent is recorded before any edge moves, so the link names the
-- ticket the follow-up was actually filed on, not wherever the climb below
-- lands it. A single-table UPDATE with correlated subqueries, not a join onto
-- a second issues alias: the engine checks issues_status_check against the
-- joined row and refuses the statement outright when the alias it reads is
-- the epic's. A child restored data gives two parents is read under the
-- lower parent id, the rule every listing already applies to it.
UPDATE issues c
SET c.follows_up = (
  SELECT r.dst_id
  FROM relations r
  JOIN issues p ON p.id = r.dst_id
  WHERE r.src_id = c.id AND r.type = 'parent-child'
    AND p.status = 'closed' AND p.issue_type <> 'epic'
  ORDER BY r.dst_id
  LIMIT 1
)
WHERE EXISTS (
  SELECT 1
  FROM relations r2
  JOIN issues p2 ON p2.id = r2.dst_id
  WHERE r2.src_id = c.id AND r2.type = 'parent-child'
    AND p2.status = 'closed' AND p2.issue_type <> 'epic'
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Backfill, step two: each such child moves beside the ticket it followed up.
-- Its destination is the nearest ancestor of that ticket that is not itself a
-- closed non-container — walked from the recorded follows_up, not read one hop
-- up, because a follow-up filed on a follow-up sits two closed leaves deep and
-- one hop would only re-nest it under the next closed leaf. The walk ends at
-- the first ancestor that is a container or still open (the new parent), or at
-- a closed leaf with no parent of its own (no parent at all). The destinations
-- are materialized in a temporary table because the engine accepts a recursive
-- query feeding an INSERT and refuses one feeding an UPDATE; the table lives on
-- this transaction's connection and is dropped below. Rank keys are frame-local
-- ties broken by id, so a moved row needs no new key: it sorts among its new
-- siblings by the key it has, exactly as `lit parent set` leaves it.
--
-- Restored data admits shapes the write boundary refuses (relations.go,
-- loadParentEdges), and the table holds exactly one row per child under all
-- of them: an ancestor with two parents forks the climb, and the deepest
-- fork's lowest id wins — the lower-id rule every listing applies to a
-- two-parent child; a cycle of closed leaves never reaches an open ancestor,
-- so the climb is cut at a depth no real hierarchy approaches and the child
-- is read as having no destination. The table lists only the children that
-- move: a climb ending on a parentless closed leaf and a child that already
-- holds the destination edge are both left to the DELETE below. src is the
-- recorded follows_up, carried here so the UPDATE names the one edge it moves
-- from this table alone: the engine skips rows when the moved edge is matched
-- by equality against a correlated scalar subquery on issues instead.
CREATE TEMPORARY TABLE followup_dest (
  child VARCHAR(191) PRIMARY KEY,
  src VARCHAR(191) NOT NULL,
  anc VARCHAR(191) NOT NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO followup_dest (child, src, anc)
WITH RECURSIVE climb (child, anc, depth) AS (
  SELECT c0.id, c0.follows_up, 0
  FROM issues c0
  WHERE c0.follows_up IS NOT NULL
  UNION ALL
  SELECT climb.child, up.dst_id, climb.depth + 1
  FROM climb
  JOIN issues a ON a.id = climb.anc
  JOIN relations up ON up.src_id = a.id AND up.type = 'parent-child'
  WHERE a.status = 'closed' AND a.issue_type <> 'epic' AND climb.depth < 64
),
dest (child, anc) AS (
  SELECT c1.child, MIN(c1.anc)
  FROM climb c1
  WHERE c1.depth = (SELECT MAX(c2.depth) FROM climb c2 WHERE c2.child = c1.child)
  GROUP BY c1.child
)
SELECT d.child, (SELECT c.follows_up FROM issues c WHERE c.id = d.child), d.anc
FROM dest d
WHERE NOT EXISTS (
    SELECT 1 FROM issues a
    WHERE a.id = d.anc AND a.status = 'closed' AND a.issue_type <> 'epic'
  )
  AND NOT EXISTS (
    SELECT 1 FROM relations held
    WHERE held.src_id = d.child AND held.dst_id = d.anc AND held.type = 'parent-child'
  );
-- +goose StatementEnd
-- +goose StatementBegin
-- The walk stopped on a container or an open ticket: that is the new parent.
-- Only the edge to the recorded follows_up moves — a two-parent child keeps its
-- other edge for the DELETE below to judge — so no rewrite can land on a row
-- that already exists. Single-table, as step one is: the engine checks
-- relations_type_check against the joined row when issues is joined in, and
-- refuses.
UPDATE relations r
SET r.dst_id = (SELECT d.anc FROM followup_dest d WHERE d.child = r.src_id)
WHERE r.type = 'parent-child'
  AND r.dst_id IN (SELECT d.src FROM followup_dest d WHERE d.child = r.src_id);
-- +goose StatementEnd
-- +goose StatementBegin
-- Every edge a follow-up still holds onto a closed non-container goes: the walk
-- stopped on a closed leaf with no parent, so the follow-up has no epic to join
-- and becomes top-level, where its followed-up ticket already lives; or the
-- child already held its destination edge and the one to the closed leaf is
-- the duplicate; or it is a restored second parent that is itself finished
-- work. After the UPDATE above these are the only such edges left.
DELETE FROM relations
WHERE type = 'parent-child'
  AND src_id IN (SELECT c.id FROM issues c WHERE c.follows_up IS NOT NULL)
  AND dst_id IN (SELECT p.id FROM issues p WHERE p.status = 'closed' AND p.issue_type <> 'epic');
-- +goose StatementEnd
-- +goose StatementBegin
DROP TEMPORARY TABLE followup_dest;
-- +goose StatementEnd

-- +goose Down
-- The follow-up marks and the follow-ups' parent edges are stashed before
-- the Down re-nests each follow-up under the ticket it follows up for the
-- reader below; the next Up's restore (migrations.Restores) writes the marks
-- back and makes each follow-up's parent edges the stashed ones again.
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
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'follows_up';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'issues' AND column_name = 'follows_up#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', CAST(t.id AS CHAR), 'follows_up', JSON_ARRAY(SUBSTRING(t.follows_up, 1)) FROM issues t;
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
DELETE FROM schema_stash WHERE table_name = 'relations' AND column_name = '*parent-child';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM schema_stash WHERE table_name = 'relations' AND column_name = '*parent-child#image';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'relations', CONCAT_WS(CHAR(31), CAST(t.src_id AS CHAR), CAST(t.dst_id AS CHAR), CAST(t.type AS CHAR)), '*parent-child', JSON_ARRAY(SUBSTRING(t.src_id, 1), SUBSTRING(t.dst_id, 1), SUBSTRING(t.type, 1), SUBSTRING(t.created_at, 1), SUBSTRING(t.created_by, 1)) FROM relations t WHERE t.type = 'parent-child' AND t.src_id IN (SELECT i.id FROM issues i WHERE i.follows_up IS NOT NULL);
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM relations
WHERE type = 'parent-child'
  AND src_id IN (
    SELECT i.id FROM issues i
    JOIN issues f ON f.id = i.follows_up
    WHERE i.follows_up IS NOT NULL
  );
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO relations(src_id, dst_id, type, created_at, created_by)
SELECT i.id, i.follows_up, 'parent-child', i.created_at, 'links'
FROM issues i
JOIN issues f ON f.id = i.follows_up
WHERE i.follows_up IS NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN follows_up;
-- +goose StatementEnd

-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'issues', s.row_key, 'follows_up#image', JSON_ARRAY((SELECT GROUP_CONCAT(CONCAT_WS(CHAR(31), CONCAT_WS(CHAR(31), CAST(u.src_id AS CHAR), CAST(u.dst_id AS CHAR), CAST(u.type AS CHAR), CAST(u.created_at AS CHAR), CAST(u.created_by AS CHAR)), CAST(o.status AS CHAR), CAST(o.issue_type AS CHAR)) ORDER BY u.src_id, u.dst_id SEPARATOR ',') FROM relations u LEFT JOIN issues o ON o.id = IF(u.src_id = s.row_key, u.dst_id, u.src_id) WHERE u.type = 'parent-child' AND u.src_id = s.row_key)) FROM schema_stash s WHERE s.table_name = 'issues' AND s.column_name = 'follows_up';
-- +goose StatementEnd
-- +goose StatementBegin
REPLACE INTO schema_stash (table_name, row_key, column_name, value) SELECT 'relations', IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]'))), '*parent-child#image', JSON_ARRAY((SELECT GROUP_CONCAT(CONCAT_WS(CHAR(31), CONCAT_WS(CHAR(31), CAST(u.src_id AS CHAR), CAST(u.dst_id AS CHAR), CAST(u.type AS CHAR), CAST(u.created_at AS CHAR), CAST(u.created_by AS CHAR)), CAST(o.status AS CHAR), CAST(o.issue_type AS CHAR)) ORDER BY u.src_id, u.dst_id SEPARATOR ',') FROM relations u LEFT JOIN issues o ON o.id = IF(u.src_id = IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]'))), u.dst_id, u.src_id) WHERE u.type = 'parent-child' AND u.src_id = IF(JSON_TYPE(JSON_EXTRACT(s.value, '$[0]')) = 'NULL', NULL, JSON_UNQUOTE(JSON_EXTRACT(s.value, '$[0]'))))) FROM schema_stash s WHERE s.table_name = 'relations' AND s.column_name = '*parent-child';
-- +goose StatementEnd
