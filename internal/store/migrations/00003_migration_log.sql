-- migration_log is a write-only observability table recording every migration
-- apply attempt. It is never read by the runner to gate behavior —
-- [LAW:one-source-of-truth] goose_db_version is the authority on "applied".
-- migration_log exists solely for operator and CI inspection.
--
-- Rows survive failures; deleting on failure would erase the most useful
-- record. A `running` row that has no matching `success`/`failure` row
-- indicates a process crash mid-migration.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE migration_log (
    id      BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    version BIGINT          NOT NULL,
    name    TEXT            NOT NULL,
    started_at  DATETIME(3)  NOT NULL,
    finished_at DATETIME(3),
    duration_ms BIGINT,
    status      VARCHAR(10)  NOT NULL,
    error_text  TEXT,
    rows_affected BIGINT,
    PRIMARY KEY (id)
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS migration_log;
