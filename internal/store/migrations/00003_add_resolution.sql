-- +goose Up
-- +goose StatementBegin
-- resolution is the sealed close reason — why a ticket was closed without being
-- finished: duplicate/superseded (redirect to a canonical ticket), obsolete (the
-- need is gone), wontfix (a standing decision). It is the payload of the closed
-- lifecycle state and is NULL on every non-closed row and on a `done` close (a
-- success carries no why-not). The Go type makes a resolution on a non-closed
-- state unrepresentable; this column is its persisted projection. The CHECK seals
-- the value set at the DB boundary, mirroring relations_type_check — the parse
-- boundary (ParseResolution) is the primary gate, this is defense in depth.
ALTER TABLE issues ADD COLUMN resolution VARCHAR(32) NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues ADD CONSTRAINT issues_resolution_check CHECK (resolution IS NULL OR resolution IN ('duplicate','superseded','obsolete','wontfix'));
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down drops resolution and does NOT preserve recorded close
-- reasons. The loss is benign by construction — with no resolution column every
-- closed ticket falls back to "closed with no recorded resolution", the same
-- state a legacy or `done` close already occupies. Operators needing the exact
-- resolutions must restore from a pre-upgrade dbsnapshot.
-- +goose StatementBegin
ALTER TABLE issues DROP CONSTRAINT issues_resolution_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN resolution;
-- +goose StatementEnd
