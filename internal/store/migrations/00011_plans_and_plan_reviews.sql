-- +goose Up
-- work_plan is the ticket's plan: how its work will be done, written before
-- the work starts so a reviewer can judge it against the laws. The empty text
-- is a ticket with no plan. Each edit is recorded in the history like any
-- field's, and those recorded texts are the plan's versions. The column is not
-- named plan because PLAN is a reserved word in the SQL dialect.
-- +goose StatementBegin
ALTER TABLE issues ADD COLUMN work_plan text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- One row per law review of a ticket's plan: the verdict, the laws it cites,
-- who gave it and when. plan_digest is the hex SHA-256 of the plan text the
-- review judged. It names the text rather than its version number because the
-- numbers follow the history's order, and a sync that merges in an older edit
-- renumbers the versions after it. laws is the cited law tokens, sorted and
-- joined by commas; a token holds no comma. A review is never edited, and it
-- cascades away with its issue on a purge.
-- +goose StatementBegin
CREATE TABLE plan_reviews (
  id VARCHAR(191) NOT NULL,
  issue_id VARCHAR(191) NOT NULL,
  plan_digest VARCHAR(64) NOT NULL,
  verdict VARCHAR(16) NOT NULL,
  laws TEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (id),
  KEY idx_plan_reviews_issue_created (issue_id, created_at),
  CONSTRAINT plan_reviews_issue FOREIGN KEY (issue_id) REFERENCES issues (id) ON DELETE CASCADE,
  CONSTRAINT plan_reviews_verdict_check CHECK (verdict IN ('pass','fail'))
);
-- +goose StatementEnd

-- +goose Down
-- LOSS CONTRACT: this Down drops every plan and every plan review. A store
-- stepped back below this version holds no record that a ticket was planned or
-- that a plan passed review. Operators who need them must restore from a
-- pre-downgrade dbsnapshot.
-- +goose StatementBegin
DROP TABLE plan_reviews;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE issues DROP COLUMN work_plan;
-- +goose StatementEnd
