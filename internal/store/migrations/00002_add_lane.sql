-- +goose Up
-- +goose StatementBegin
-- lane partitions an epic's children into parallel rank-ordered sub-sequences:
-- children sharing a lane are sequenced by rank; different lanes run in
-- parallel. The empty-string default is one lane value like any other — every
-- keyless child shares it, reproducing fully-sequential epic behavior. lane is
-- meaningful only within an epic (outside an epic, explicit `blocks` edges are
-- the ordering signal); the gate predicate enforces that scoping, not the column.
ALTER TABLE issues ADD COLUMN lane text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down drops lane and does NOT restore per-child lane
-- assignments. The loss is benign by construction — with no lane column every
-- child falls back to the shared default lane, i.e. the fully-sequential epic
-- gate, which is the conservative behavior. Operators needing exact lane
-- values must restore from a pre-upgrade dbsnapshot.
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN lane;
-- +goose StatementEnd
