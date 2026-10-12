-- +goose Up
-- +goose StatementBegin
-- Containment: every ticket lives inside a container. From this version a
-- task, bug, feature or chore always has a parent and every parent is an epic,
-- so only an epic may be top-level. The write boundary refuses every write
-- that would break that (storage.RejectLeafWithoutParent and its siblings);
-- this migration brings the data already stored under the same rule, in two
-- moves. A child whose parent is not an epic moves to the nearest epic above
-- it. A leaf with no epic above it at all — a top-level leaf, or a child of
-- leaves with no epic over them — moves under one epic this migration creates
-- for the store, "Unfiled tickets", where it stays until an agent files it
-- where it belongs. A deleted ticket moves like a live one, since a delete is
-- reversible and a restored ticket needs its home; only live ones decide the
-- epic's rank. No column changes.
--
-- Nothing here is materialized in a temporary table, and that is deliberate:
-- the engine corrupts a temporary table that a modifying statement reads
-- through a join or a correlated EXISTS — every later join onto it returns
-- each row many times over while COUNT(*) still answers the true count — so
-- a DELETE keyed on such a table left the next INSERT writing one edge twenty-
-- six times on a real store, and a duplicate primary key was the only sign.
-- 00006 escaped by luck of statement order. The destinations are instead
-- computed inside the one INSERT that needs them, where the engine accepts a
-- recursive query, and the id the store-level epic gets is held in a session
-- variable, set once and read by name.
--
-- The epic's id is rendered the way `lit new` renders a top-level id,
-- <prefix>-<topic>-<hash>, so every reader of id shapes (`lit prefix set`'s
-- count of the prefixes a store's ids carry, the top-level population behind
-- hash lengths) reads it as one of the store's own. The prefix is the one most
-- of the store's top-level ids carry, read back from the ids the way
-- issueid.PrefixOf reads them — the id with its "-<topic>-<hash>" tail
-- removed, the tail a base36 hash of the length the generator mints — ties
-- broken by the lower prefix, and "lit" when no id is readable that way. The
-- hash is the first four hex digits of a SHA-256 over the prefix and the
-- topic: hex is base36, four is a length the generator mints, and nothing in
-- it varies by store, so two checkouts of one workspace that each run this
-- migration mint the same id, and the one that already received the epic
-- through sync mints nothing (@minted below).
SET @unfiled := (
  SELECT CONCAT(p.prefix, '-unfiled-', LEFT(SHA2(CONCAT(p.prefix, '-unfiled'), 256), 4))
  FROM (
    SELECT COALESCE((
      SELECT counted.prefix
      FROM (
        SELECT
          SUBSTRING(i.id, 1, CHAR_LENGTH(i.id) - CHAR_LENGTH(i.topic) - CHAR_LENGTH(SUBSTRING_INDEX(i.id, '-', -1)) - 2) AS prefix,
          COUNT(*) AS n
        FROM issues i
        WHERE i.id NOT LIKE '%.%'
          AND SUBSTRING_INDEX(i.id, '-', -1) REGEXP '^[0-9a-z]{3,8}$'
          AND CHAR_LENGTH(i.id) > CHAR_LENGTH(i.topic) + CHAR_LENGTH(SUBSTRING_INDEX(i.id, '-', -1)) + 2
          AND RIGHT(i.id, CHAR_LENGTH(i.topic) + CHAR_LENGTH(SUBSTRING_INDEX(i.id, '-', -1)) + 2) = CONCAT('-', i.topic, '-', SUBSTRING_INDEX(i.id, '-', -1))
        GROUP BY prefix
      ) counted
      ORDER BY counted.n DESC, counted.prefix ASC
      LIMIT 1
    ), 'lit') AS prefix
  ) p
);
-- +goose StatementEnd
-- +goose StatementBegin
-- The epic is minted only when the store does not hold it already and
-- something may need it: a leaf with no parent, or a child under a parent that
-- is not an epic. The second is a superset of need — such a child usually
-- climbs to an epic — so an epic minted for nothing is removed again at the
-- end, and only one this run minted, never one a peer sent.
SET @minted := NOT EXISTS (SELECT 1 FROM issues e WHERE e.id = @unfiled)
  AND (
    EXISTS (
      SELECT 1 FROM issues i
      WHERE i.issue_type <> 'epic'
        AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = i.id AND r.type = 'parent-child')
    )
    OR EXISTS (
      SELECT 1 FROM relations r JOIN issues p ON p.id = r.dst_id
      WHERE r.type = 'parent-child' AND p.issue_type <> 'epic'
    )
  );
-- +goose StatementEnd
-- +goose StatementBegin
-- A moved ticket was a lane of one — a top-level leaf, or a child of a leaf,
-- is its own lane — and stays one: its lane becomes its own id. Left empty, it
-- would join its new epic's default lane and stand in sequence behind every
-- earlier sibling there, so one move would make unrelated work wait on
-- unrelated work. The moved set is read off the edges as they stand now, so
-- this runs before any edge moves.
UPDATE issues
SET lane = id
WHERE (
    issue_type <> 'epic'
    AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = issues.id AND r.type = 'parent-child')
  )
  OR (
    EXISTS (
      SELECT 1 FROM relations r JOIN issues p ON p.id = r.dst_id
      WHERE r.src_id = issues.id AND r.type = 'parent-child' AND p.issue_type <> 'epic'
    )
    AND NOT EXISTS (
      SELECT 1 FROM relations held JOIN issues e ON e.id = held.dst_id
      WHERE held.src_id = issues.id AND held.type = 'parent-child' AND e.issue_type = 'epic'
    )
  );
-- +goose StatementEnd
-- +goose StatementBegin
-- The unfiled epic. Its rank is the rank the highest-ranked live top-level
-- leaf held in the top-level order, so the group sits where the best of its
-- members sat; the members keep their own keys, which now order them among
-- each other inside the epic. No top-level leaf with a key means an unranked
-- epic. created_at is the date this migration was authored rather than the
-- moment it ran, for the same reason the id is fixed: two checkouts must mint
-- one row, not two rows that disagree in a cell.
INSERT INTO issues (
  id, title, description, agent_prompt, status, priority, issue_type, topic, assignee,
  item_rank, lane, created_at, updated_at, closed_at, archived_at, deleted_at,
  resolution, redirect_target, follows_up
)
SELECT
  @unfiled,
  'Unfiled tickets',
  'Tickets that had no epic when every ticket came to live inside one (schema v7): top-level tasks, bugs, features and chores, and children of tickets that are not epics with no epic above them. Each belongs to some effort; move it there with `lit parent set --child <id> --parent <epic-id>`. Every ticket here is its own lane, as it was at the top level, so they are worked in parallel rather than in sequence. This epic is finished when it is empty.',
  NULL, NULL, 0, 'epic', 'unfiled', '',
  COALESCE((
    SELECT MIN(i.item_rank) FROM issues i
    WHERE i.issue_type <> 'epic' AND i.item_rank <> '' AND i.deleted_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = i.id AND r.type = 'parent-child')
  ), ''),
  '', '2026-10-12T00:00:00Z', '2026-10-12T00:00:00Z', NULL, NULL, NULL,
  NULL, NULL, NULL
FROM (SELECT 1) one
WHERE @minted;
-- +goose StatementEnd
-- +goose StatementBegin
-- The move. For every child under a parent that is not an epic, the climb
-- starts at that parent and walks up while the ancestor is still not an epic,
-- cut at a depth no real hierarchy approaches so a loop of leaves ends rather
-- than runs. The deepest row is where the walk stopped: an epic, which is the
-- destination, or a leaf with no parent of its own or the depth cut, which
-- leaves the child with no epic above it, so it goes unfiled. Restored data
-- admits shapes the write boundary refuses, and the climb yields one
-- destination per child under all of them: a child with two leaf parents
-- forks the climb, and the deepest fork's lowest id wins, the lower-id rule
-- every listing applies; a child that already holds an epic parent beside a
-- leaf parent is home and is not climbed, so the DELETE below leaves it its
-- epic alone; and an epic in a loop through a leaf climbs back to itself,
-- which is no edge at all, so it comes out at the top level, where an epic
-- may live. Every leaf with no parent at all goes unfiled too. The new edge
-- is stamped with the child's own created_at and 'links', the stamps `lit new
-- --parent` writes on the edge it creates, and is not written again where the
-- child already holds it.
INSERT INTO relations (src_id, dst_id, type, created_at, created_by)
WITH RECURSIVE climb (child, anc, depth) AS (
  SELECT r.src_id, r.dst_id, 0
  FROM relations r
  JOIN issues p ON p.id = r.dst_id
  WHERE r.type = 'parent-child' AND p.issue_type <> 'epic'
    AND NOT EXISTS (
      SELECT 1 FROM relations held JOIN issues e ON e.id = held.dst_id
      WHERE held.src_id = r.src_id AND held.type = 'parent-child' AND e.issue_type = 'epic'
    )
  UNION ALL
  SELECT climb.child, up.dst_id, climb.depth + 1
  FROM climb
  JOIN issues a ON a.id = climb.anc
  JOIN relations up ON up.src_id = a.id AND up.type = 'parent-child'
  WHERE a.issue_type <> 'epic' AND climb.depth < 64
),
dest (child, anc) AS (
  SELECT c1.child, MIN(c1.anc)
  FROM climb c1
  WHERE c1.depth = (SELECT MAX(c2.depth) FROM climb c2 WHERE c2.child = c1.child)
  GROUP BY c1.child
),
moved (child, dst) AS (
  SELECT d.child, COALESCE((SELECT e.id FROM issues e WHERE e.id = d.anc AND e.issue_type = 'epic'), @unfiled)
  FROM dest d
  UNION ALL
  SELECT i.id, @unfiled
  FROM issues i
  WHERE i.issue_type <> 'epic'
    AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = i.id AND r.type = 'parent-child')
)
SELECT m.child, m.dst, 'parent-child', i.created_at, 'links'
FROM moved m
JOIN issues i ON i.id = m.child
WHERE m.child <> m.dst
  AND NOT EXISTS (
  SELECT 1 FROM relations held
  WHERE held.src_id = m.child AND held.dst_id = m.dst AND held.type = 'parent-child'
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Every edge onto a parent that is not an epic goes: the old edge of each
-- child the move above re-homed, and a restored second parent that is itself
-- a leaf. After this, every parent is an epic.
DELETE r FROM relations r
JOIN issues p ON p.id = r.dst_id
WHERE r.type = 'parent-child' AND p.issue_type <> 'epic';
-- +goose StatementEnd
-- +goose StatementBegin
-- An epic this run minted that nothing landed in is not kept.
DELETE FROM issues
WHERE id = @unfiled AND @minted
  AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.dst_id = issues.id AND r.type = 'parent-child');
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: down removes the unfiled epic and lets its children — the
-- leaves this Up homed there and any filed under it since — go back to the
-- top level, where the pre-upgrade reader wrote them. A child that Up moved
-- from a parent that is not an epic to the epic above it stays where Up put
-- it: nothing records which parent it had, and the epic above is a parent
-- the pre-upgrade reader accepts. A lane spelled as the ticket's own id is
-- reset to the default lane, since that is the spelling Up wrote; a lane an
-- agent set to the id by hand is reset with it. Comments, labels and events
-- on the unfiled epic itself go with its row. The epic is recognized by its
-- type, topic and title as Up wrote them; one renamed since is an ordinary
-- epic and stays.
-- +goose StatementBegin
UPDATE issues
SET lane = ''
WHERE lane = id;
-- +goose StatementEnd
-- +goose StatementBegin
DELETE r FROM relations r
JOIN issues e ON e.id = r.dst_id
WHERE r.type = 'parent-child'
  AND e.issue_type = 'epic' AND e.topic = 'unfiled' AND e.title = 'Unfiled tickets';
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM issues
WHERE issue_type = 'epic' AND topic = 'unfiled' AND title = 'Unfiled tickets';
-- +goose StatementEnd
