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
-- Backfill, step one: every child of a closed non-container is a follow-up of
-- that parent. A closed task, bug, feature or chore holds children only because
-- `lit followup` put them there; nothing else in the product files work under a
-- leaf. The parent is recorded before any edge moves, so the link names the
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
-- LOSS CONTRACT: down re-nests every follow-up under the ticket it follows up
-- and then drops the column, so the pre-upgrade reader sees the graph it used
-- to write — a follow-up as a child of the ticket it followed up — and loses the
-- follow-up's place among its epic's children. Backfilled rows return to where
-- they were; follow-ups filed after the upgrade move under their followed-up
-- ticket for the first time. A follow-up whose followed-up ticket has since
-- been purged keeps its current parent (the join on issues requires the target
-- to exist, as relations' FK would). Every parent-child edge a re-nested
-- follow-up held is replaced by the one edge to its followed-up ticket, so a
-- restored second parent is lost with the column. Edge created_at is the
-- follow-up's own created_at and created_by is 'links', the stamps
-- `lit followup` wrote on the original edge.
--
-- The column is the only record of which children of an OPEN ticket or of an
-- epic were follow-ups: a later Up backfills from closed non-containers alone,
-- so a follow-up filed on an open ticket or on an epic comes back from a
-- downgrade-then-upgrade as an ordinary child of that ticket, with no
-- follows_up and no move beside it.
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
