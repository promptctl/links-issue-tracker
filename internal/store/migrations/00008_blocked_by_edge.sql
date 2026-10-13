-- +goose Up
-- The dependency edge was stored as type 'blocks' with src the ticket that
-- waits and dst the ticket it waits for, so the row read "src blocks dst" while
-- meaning the opposite, and every layer above the table flipped the endpoints
-- to compensate. The edge is now 'blocked-by': "src blocked-by dst" says which
-- ticket waits, in the same words `lit dep add <id> --blocked-by <id>` and
-- `lit show`'s blocked_by group use. Only the name changes. Every row keeps
-- its src and dst, so every stored edge keeps its meaning. The CHECK comes off
-- before the rename and goes back on sealing the new name, so the table never
-- holds a type outside the set it declares. No row can already hold
-- 'blocked-by', so the rewritten primary keys cannot collide.
--
-- The CHECK is found by table, not by name: a store created before goose
-- declared it inline and unnamed, so the engine named it per store
-- (relations_chk_<suffix>). The one CHECK relations carries is dropped
-- whatever its name; none, or more than one, fails the PREPARE and the
-- migration with it. Re-added, it is relations_type_check on every store.
-- +goose StatementBegin
SET @drop_relations_check = (
  SELECT CONCAT('ALTER TABLE relations DROP CONSTRAINT `', tc.constraint_name, '`')
  FROM information_schema.table_constraints tc
  WHERE tc.table_schema = DATABASE() AND tc.table_name = 'relations' AND tc.constraint_type = 'CHECK'
);
-- +goose StatementEnd
-- +goose StatementBegin
PREPARE drop_relations_check FROM @drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
EXECUTE drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
DEALLOCATE PREPARE drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE relations SET type = 'blocked-by' WHERE type = 'blocks';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE relations ADD CONSTRAINT relations_type_check CHECK (type IN ('blocked-by','parent-child','related-to'));
-- +goose StatementEnd

-- +goose Down
-- Lossless: the rename is a bijection on the type column, so down restores
-- every 'blocked-by' row to 'blocks' with its src and dst untouched.
-- +goose StatementBegin
SET @drop_relations_check = (
  SELECT CONCAT('ALTER TABLE relations DROP CONSTRAINT `', tc.constraint_name, '`')
  FROM information_schema.table_constraints tc
  WHERE tc.table_schema = DATABASE() AND tc.table_name = 'relations' AND tc.constraint_type = 'CHECK'
);
-- +goose StatementEnd
-- +goose StatementBegin
PREPARE drop_relations_check FROM @drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
EXECUTE drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
DEALLOCATE PREPARE drop_relations_check;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE relations SET type = 'blocks' WHERE type = 'blocked-by';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE relations ADD CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'));
-- +goose StatementEnd
