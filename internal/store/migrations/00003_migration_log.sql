-- migration_log is a write-only observability table recording every migration
-- apply attempt. It is never read by the runner to gate behavior —
-- [LAW:one-source-of-truth] goose_db_version is the authority on "applied".
-- migration_log exists solely for operator and CI inspection.
--
-- Rows are written only after a migration completes (success after
-- ApplyVersion, failure after the auto-revert reset). The status column
-- carries 'success' or 'failure'; there is no in-flight 'running' state
-- today. Absence of a row for a given version means the migration never
-- ran on this workspace, not that the process crashed mid-way.

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
