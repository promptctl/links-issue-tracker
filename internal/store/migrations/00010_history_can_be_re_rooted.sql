-- +goose Up
-- A store at this version or above can be re-rooted by a push past the history
-- bound, and every lit that supports it follows a re-rooted remote at every
-- sync entry point. A lit whose support stops below it meets a re-rooted remote
-- as unrelated histories, and it refuses to write to a remote whose head
-- records a version above its own (guardCommitSchemaAhead). So once the shared
-- remote carries this version, no lit that cannot follow a re-root can push
-- over one, force-push it, combine with it or take a side against it.
--
-- It is also the first version past v9, the newest any lit that wrote
-- meta.producer_binary_version supports. lit v0.1.0.1 through v0.14.0.1 read
-- that row and, on a workspace too new for them, print `lit upgrade --to
-- <row>`; past v9 that target refuses the workspace too, so the row goes.
-- +goose StatementBegin
DELETE FROM meta WHERE meta_key = 'producer_binary_version';
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down does not restore meta.producer_binary_version. No
-- lit since v0.18.0 writes or reads it, and the version it named is not
-- recoverable from the store.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
