-- migration_quarantine tracks goose migration versions that have been marked
-- bad (either auto-marked when the runner caught a failure and reverted, or
-- manually marked via `lit doctor --reset-to-pre-migration`). The runner
-- reads this table on every Open and passes the contents to
-- goose.WithExcludeVersions so a quarantined migration is never re-applied.
--
-- [LAW:one-source-of-truth] goose_db_version remains the authority on what
-- has been applied; this table is the authority on what should never be
-- applied (or re-applied) again. Together they form the runner's complete
-- decision input.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE migration_quarantine (
    version_id BIGINT PRIMARY KEY,
    reason TEXT NOT NULL,
    quarantined_at DATETIME NOT NULL
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS migration_quarantine;
