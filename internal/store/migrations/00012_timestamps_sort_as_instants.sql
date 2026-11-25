-- Every stored timestamp is spelled one way: UTC RFC3339 with exactly nine
-- fractional digits, e.g. 2026-11-24T10:00:07.500000000Z. Before this the
-- store held time.RFC3339Nano text, which trims trailing zeros, so comparing
-- two stamps as text did not compare their instants: '...:07Z' sorted after
-- '...:07.5Z'. A fixed width makes text order time order, so a SQL ORDER BY or
-- range over a timestamp column is correct. The one writer is
-- model.StampText; each column below gets a CHECK that admits only the shape,
-- so a writer that spells an instant any other way, an older release included,
-- is refused by the store, never stored. The shape is the stamp with its
-- calendar ranges (month 01-12, hour 00-23, minute and second 00-59), a day
-- that exists in its month (the engine would turn February 30th into March 2nd,
-- a different instant, so the date must survive a round trip unchanged), and
-- its end of text, so a value with a trailing newline is no stamp either.
--
-- Up rewrites every value in place, one UPDATE per column, then adds the
-- CHECKs. A value that fits neither the canonical 'Z' spelling lit's own
-- writers produce nor an RFC3339 offset spelling an import could carry is not
-- rewritten, and the CHECK that follows refuses it, which fails this migration
-- and resets the store to its checkpoint. So does a value naming a day that does
-- not exist. IFNULL leaves a value whose conversion has no result as it was,
-- never as NULL, for the CHECK to refuse. A value with an offset keeps its
-- instant and is spelled in UTC: the contract's stamps are UTC (storage.Clock),
-- so a non-UTC spelling is outside the domain of stores this Down returns, as
-- design-docs/schema-reversibility.md records. migrationProbes counts the
-- values rewritten and the offsets normalized, so the open that ran this says
-- what it did.
--
-- Down is lossless for every store in that domain. For a canonical value the
-- rewrite is a bijection: dropping the fraction's trailing zeros, and the
-- fraction itself when all nine are zero, gives back exactly what time.
-- RFC3339Nano wrote, a whole second included. The CHECKs come off first.
--
-- issue_event_changes carries timestamps only for the three fields
-- model.TimestampFields names; its other values are text a person wrote
-- and are not touched. A timestamp field's absent value is NULL, the one
-- spelling its writer (insertEventTx) produces, so an empty value is set to
-- NULL here and the CHECK admits NULL alone.
--
-- migration_quarantine is not here: it is lit's bookkeeping, created outside
-- goose (ensureQuarantineTable), not a column of the store's data, and its
-- writer spells its stamp with model.StampText.
--
-- An older release writing trimmed text into a store this migration reached is
-- refused by the CHECKs, by constraint name. The minimum writer schema below
-- makes a release that reads that row refuse the store up front instead; a
-- release built before the row existed (migrations/README.md says which) does
-- not read it, and meets the CHECK mid-command.

-- +goose Up
-- +goose StatementBegin
UPDATE issues SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET updated_at = IFNULL(CONCAT(IF(RIGHT(updated_at,1)='Z',SUBSTRING(updated_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(updated_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(updated_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(updated_at,21,CHAR_LENGTH(updated_at)-20-IF(RIGHT(updated_at,1)='Z',1,6)),9,'0'),'Z'),updated_at) WHERE updated_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(updated_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(updated_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET closed_at = IFNULL(CONCAT(IF(RIGHT(closed_at,1)='Z',SUBSTRING(closed_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(closed_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(closed_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(closed_at,21,CHAR_LENGTH(closed_at)-20-IF(RIGHT(closed_at,1)='Z',1,6)),9,'0'),'Z'),closed_at) WHERE closed_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(closed_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(closed_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET archived_at = IFNULL(CONCAT(IF(RIGHT(archived_at,1)='Z',SUBSTRING(archived_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(archived_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(archived_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(archived_at,21,CHAR_LENGTH(archived_at)-20-IF(RIGHT(archived_at,1)='Z',1,6)),9,'0'),'Z'),archived_at) WHERE archived_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(archived_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(archived_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET deleted_at = IFNULL(CONCAT(IF(RIGHT(deleted_at,1)='Z',SUBSTRING(deleted_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(deleted_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(deleted_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(deleted_at,21,CHAR_LENGTH(deleted_at)-20-IF(RIGHT(deleted_at,1)='Z',1,6)),9,'0'),'Z'),deleted_at) WHERE deleted_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(deleted_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(deleted_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE comments SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_events SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE labels SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE relations SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE plan_reviews SET created_at = IFNULL(CONCAT(IF(RIGHT(created_at,1)='Z',SUBSTRING(created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(created_at,21,CHAR_LENGTH(created_at)-20-IF(RIGHT(created_at,1)='Z',1,6)),9,'0'),'Z'),created_at) WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE purges SET issue_created_at = IFNULL(CONCAT(IF(RIGHT(issue_created_at,1)='Z',SUBSTRING(issue_created_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(issue_created_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(issue_created_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(issue_created_at,21,CHAR_LENGTH(issue_created_at)-20-IF(RIGHT(issue_created_at,1)='Z',1,6)),9,'0'),'Z'),issue_created_at) WHERE issue_created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(issue_created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(issue_created_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE purges SET purged_at = IFNULL(CONCAT(IF(RIGHT(purged_at,1)='Z',SUBSTRING(purged_at,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(purged_at,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(purged_at,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(purged_at,21,CHAR_LENGTH(purged_at)-20-IF(RIGHT(purged_at,1)='Z',1,6)),9,'0'),'Z'),purged_at) WHERE purged_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(purged_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(purged_at,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET from_value = IFNULL(CONCAT(IF(RIGHT(from_value,1)='Z',SUBSTRING(from_value,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(from_value,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(from_value,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(from_value,21,CHAR_LENGTH(from_value)-20-IF(RIGHT(from_value,1)='Z',1,6)),9,'0'),'Z'),from_value) WHERE field IN ('closed_at','archived_at','deleted_at') AND from_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(from_value,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(from_value,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET to_value = IFNULL(CONCAT(IF(RIGHT(to_value,1)='Z',SUBSTRING(to_value,1,19),DATE_FORMAT(CONVERT_TZ(STR_TO_DATE(SUBSTRING(to_value,1,19),'%Y-%m-%dT%H:%i:%s'),RIGHT(to_value,6),'+00:00'),'%Y-%m-%dT%H:%i:%s')),'.',RPAD(SUBSTRING(to_value,21,CHAR_LENGTH(to_value)-20-IF(RIGHT(to_value,1)='Z',1,6)),9,'0'),'Z'),to_value) WHERE field IN ('closed_at','archived_at','deleted_at') AND to_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]([.][0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(to_value,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(to_value,1,19);
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET from_value = NULL WHERE field IN ('closed_at','archived_at','deleted_at') AND from_value = '';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET to_value = NULL WHERE field IN ('closed_at','archived_at','deleted_at') AND to_value = '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_updated_at_stamp CHECK (updated_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(updated_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(updated_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_closed_at_stamp CHECK (closed_at IS NULL OR closed_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(closed_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(closed_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_archived_at_stamp CHECK (archived_at IS NULL OR archived_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(archived_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(archived_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_deleted_at_stamp CHECK (deleted_at IS NULL OR deleted_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(deleted_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(deleted_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE comments ADD CONSTRAINT comments_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events ADD CONSTRAINT issue_events_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE labels ADD CONSTRAINT labels_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE relations ADD CONSTRAINT relations_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE plan_reviews ADD CONSTRAINT plan_reviews_created_at_stamp CHECK (created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE purges ADD CONSTRAINT purges_issue_created_at_stamp CHECK (issue_created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(issue_created_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(issue_created_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE purges ADD CONSTRAINT purges_purged_at_stamp CHECK (purged_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(purged_at,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(purged_at,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_event_changes ADD CONSTRAINT issue_event_changes_from_value_stamp CHECK (field NOT IN ('closed_at','archived_at','deleted_at') OR from_value IS NULL OR from_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(from_value,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(from_value,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_event_changes ADD CONSTRAINT issue_event_changes_to_value_stamp CHECK (field NOT IN ('closed_at','archived_at','deleted_at') OR to_value IS NULL OR to_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z' AND DATE_FORMAT(STR_TO_DATE(COALESCE(SUBSTRING(to_value,1,19),'-'),'%Y-%m-%dT%H:%i:%s'),'%Y-%m-%dT%H:%i:%s')=SUBSTRING(to_value,1,19));
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO meta (meta_key, meta_value) VALUES ('min_writer_schema', '12') ON DUPLICATE KEY UPDATE meta_value = '12';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM meta WHERE meta_key = 'min_writer_schema';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_updated_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_closed_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_archived_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_deleted_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE comments DROP CONSTRAINT comments_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events DROP CONSTRAINT issue_events_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE labels DROP CONSTRAINT labels_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE relations DROP CONSTRAINT relations_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE plan_reviews DROP CONSTRAINT plan_reviews_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE purges DROP CONSTRAINT purges_issue_created_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE purges DROP CONSTRAINT purges_purged_at_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_event_changes DROP CONSTRAINT issue_event_changes_from_value_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_event_changes DROP CONSTRAINT issue_event_changes_to_value_stamp;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET updated_at = CONCAT(LEFT(updated_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(updated_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(updated_at,21,9)))),'Z') WHERE updated_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET closed_at = CONCAT(LEFT(closed_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(closed_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(closed_at,21,9)))),'Z') WHERE closed_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET archived_at = CONCAT(LEFT(archived_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(archived_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(archived_at,21,9)))),'Z') WHERE archived_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issues SET deleted_at = CONCAT(LEFT(deleted_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(deleted_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(deleted_at,21,9)))),'Z') WHERE deleted_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE comments SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_events SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE labels SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE relations SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE plan_reviews SET created_at = CONCAT(LEFT(created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(created_at,21,9)))),'Z') WHERE created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE purges SET issue_created_at = CONCAT(LEFT(issue_created_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(issue_created_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(issue_created_at,21,9)))),'Z') WHERE issue_created_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE purges SET purged_at = CONCAT(LEFT(purged_at,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(purged_at,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(purged_at,21,9)))),'Z') WHERE purged_at REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET from_value = CONCAT(LEFT(from_value,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(from_value,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(from_value,21,9)))),'Z') WHERE field IN ('closed_at','archived_at','deleted_at') AND from_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE issue_event_changes SET to_value = CONCAT(LEFT(to_value,19),IF(TRIM(TRAILING '0' FROM SUBSTRING(to_value,21,9))='','',CONCAT('.',TRIM(TRAILING '0' FROM SUBSTRING(to_value,21,9)))),'Z') WHERE field IN ('closed_at','archived_at','deleted_at') AND to_value REGEXP '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z';
-- +goose StatementEnd
