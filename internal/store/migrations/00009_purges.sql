-- +goose Up
-- One row per purged issue: the id, when, and by whom — never the content. A
-- purge deletes the issue's row, and the ON DELETE CASCADE on every child table
-- takes its comments, labels, history and edges with it; this row is what
-- remains, so that a merge drops the id from every side, a restore does not
-- bring it back, and id minting never hands it out again. issue_id carries no
-- foreign key: the issue it names no longer exists, by definition.
-- +goose StatementBegin
CREATE TABLE purges (
  issue_id VARCHAR(191) NOT NULL,
  purged_at VARCHAR(64) NOT NULL,
  purged_by TEXT NOT NULL,
  PRIMARY KEY (issue_id)
);
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down drops the purge records. The purged issues stay
-- gone — their rows were deleted by the purge, not by this migration — but a
-- store stepped back below this version no longer knows which ids were
-- purged, so a sync with a store that still holds one of them can bring it
-- back, and minting can hand a purged id out again. Operators who need the
-- records must restore from a pre-downgrade dbsnapshot.
-- +goose StatementBegin
DROP TABLE purges;
-- +goose StatementEnd
