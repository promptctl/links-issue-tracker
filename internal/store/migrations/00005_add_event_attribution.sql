-- +goose Up
-- +goose StatementBegin
-- The attribution pair: which checkout produced this work event. stream_id is
-- the producing checkout's opaque per-worktree token, workspace_id the opaque
-- per-store id it was produced in; together they distinguish two worktrees of
-- one repository across every clone the database reaches. Claims are DERIVED
-- from these stamps at read time — no claim table follows this column.
--
-- NULL is the load-bearing value, not an oversight: events written before this
-- migration carry no attribution and never will (attribution is historical
-- fact, never backfilled), so a freshly upgraded repository derives zero claims
-- and selects work exactly as it did before. Cold start by construction.
-- Nothing user-, host-, or path-shaped may ever enter these columns; the
-- database syncs to shared remotes.
ALTER TABLE issue_events ADD COLUMN stream_id VARCHAR(64) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events ADD COLUMN workspace_id VARCHAR(191) NULL;
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down drops both columns and does NOT restore the
-- attribution collected while the workspace was on this version. The loss is
-- benign by construction — an unattributed event is the shape every event had
-- before this migration, and the claim predicate reads unattributed history as
-- zero claims, which is the conservative behavior the whole design falls back
-- to. Operators needing the stamps must restore from a pre-downgrade dbsnapshot.
-- +goose StatementBegin
ALTER TABLE issue_events DROP COLUMN stream_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issue_events DROP COLUMN workspace_id;
-- +goose StatementEnd
