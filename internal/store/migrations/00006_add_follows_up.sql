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
-- Its destination is the nearest ancestor that is not itself a closed
-- non-container — walked, not read one hop up, because a follow-up filed on a
-- follow-up sits two closed leaves deep and one hop would only re-nest it under
-- the next closed leaf. The walk ends at the first ancestor that is a container
-- or still open (the new parent), or at a closed leaf with no parent of its own
-- (no parent at all). The destinations are materialized in a temporary table
-- because the engine accepts a recursive query feeding an INSERT and refuses
-- one feeding an UPDATE; the table lives on this transaction's connection and
-- is dropped below. Rank keys are frame-local ties broken by id, so a moved
-- row needs no new key: it sorts among its new siblings by the key it has,
-- exactly as `lit parent set` leaves it.
CREATE TEMPORARY TABLE followup_dest (
  child VARCHAR(191) PRIMARY KEY,
  anc VARCHAR(191) NOT NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO followup_dest (child, anc)
WITH RECURSIVE climb (child, anc, depth) AS (
  SELECT r0.src_id, r0.dst_id, 0
  FROM relations r0
  JOIN issues c0 ON c0.id = r0.src_id
  WHERE r0.type = 'parent-child' AND c0.follows_up IS NOT NULL
  UNION ALL
  SELECT climb.child, up.dst_id, climb.depth + 1
  FROM climb
  JOIN issues a ON a.id = climb.anc
  JOIN relations up ON up.src_id = a.id AND up.type = 'parent-child'
  WHERE a.status = 'closed' AND a.issue_type <> 'epic'
)
SELECT c1.child, c1.anc
FROM climb c1
WHERE c1.depth = (SELECT MAX(c2.depth) FROM climb c2 WHERE c2.child = c1.child);
-- +goose StatementEnd
-- +goose StatementBegin
-- The walk stopped on a container or an open ticket: that is the new parent.
-- Single-table, as step one is: the engine checks relations_type_check against
-- the joined row when issues is joined in, and refuses.
UPDATE relations r
SET r.dst_id = (SELECT d.anc FROM followup_dest d WHERE d.child = r.src_id)
WHERE r.type = 'parent-child'
  AND r.src_id IN (
    SELECT d.child FROM followup_dest d
    JOIN issues a ON a.id = d.anc
    WHERE NOT (a.status = 'closed' AND a.issue_type <> 'epic')
  );
-- +goose StatementEnd
-- +goose StatementBegin
-- The walk stopped on a closed leaf with no parent: the follow-up has no epic
-- to join and becomes a top-level ticket, which is where its followed-up ticket
-- already lives. After the UPDATE above, the only follow-ups still parented to
-- a closed non-container are exactly these.
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
-- to exist, as relations' FK would), and one that already sits under that
-- ticket is left as it is (INSERT IGNORE on the existing edge). Edge created_at
-- is the follow-up's own created_at and created_by is 'links', the stamps
-- `lit followup` wrote on the original edge.
-- +goose StatementBegin
UPDATE relations r
SET r.dst_id = (SELECT i.follows_up FROM issues i WHERE i.id = r.src_id)
WHERE r.type = 'parent-child'
  AND r.src_id IN (
    SELECT i.id FROM issues i
    JOIN issues f ON f.id = i.follows_up
    WHERE i.follows_up IS NOT NULL
  );
-- +goose StatementEnd
-- +goose StatementBegin
INSERT IGNORE INTO relations(src_id, dst_id, type, created_at, created_by)
SELECT i.id, i.follows_up, 'parent-child', i.created_at, 'links'
FROM issues i
JOIN issues f ON f.id = i.follows_up
LEFT JOIN relations r ON r.src_id = i.id AND r.type = 'parent-child'
WHERE i.follows_up IS NOT NULL AND r.src_id IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN follows_up;
-- +goose StatementEnd
