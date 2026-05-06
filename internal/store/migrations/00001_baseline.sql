-- Baseline schema for the lit issue tracker.
--
-- Goose's first migration captures the converged shape that pre-goose
-- workspaces reached via probe-gated reconciliation in schema.go. Fresh
-- workspaces created on or after the goose-layer landing run only this
-- file; pre-goose workspaces are stamped at version 1 by
-- adoptPreGooseWorkspace and never run this body.
--
-- [LAW:one-source-of-truth] goose_db_version is the authority for "applied".

-- +goose Up
-- +goose StatementBegin
CREATE TABLE meta (
    meta_key VARCHAR(191) PRIMARY KEY,
    meta_value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE issues (
    id VARCHAR(191) PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    agent_prompt TEXT NULL,
    status VARCHAR(32) NULL,
    priority INT NOT NULL,
    issue_type VARCHAR(32) NOT NULL,
    topic VARCHAR(191) NOT NULL,
    assignee TEXT NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    updated_at VARCHAR(64) NOT NULL,
    closed_at VARCHAR(64) NULL,
    archived_at VARCHAR(64) NULL,
    deleted_at VARCHAR(64) NULL,
    item_rank TEXT NOT NULL DEFAULT '',
    CONSTRAINT issues_status_check CHECK (
        (issue_type IN ('epic') AND status IS NULL)
        OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))
    ),
    CONSTRAINT issues_priority_check CHECK (priority >= 0 AND priority <= 1),
    CONSTRAINT issues_type_check CHECK (issue_type IN ('task','feature','bug','chore','epic'))
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE relations (
    src_id VARCHAR(191) NOT NULL,
    dst_id VARCHAR(191) NOT NULL,
    type VARCHAR(32) NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (src_id, dst_id, type),
    FOREIGN KEY (src_id) REFERENCES issues(id) ON DELETE CASCADE,
    FOREIGN KEY (dst_id) REFERENCES issues(id) ON DELETE CASCADE,
    CONSTRAINT relations_type_check CHECK (type IN ('blocks','parent-child','related-to'))
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE comments (
    id VARCHAR(191) PRIMARY KEY,
    issue_id VARCHAR(191) NOT NULL,
    body TEXT NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    created_by TEXT NOT NULL,
    FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE labels (
    issue_id VARCHAR(191) NOT NULL,
    label VARCHAR(191) NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (issue_id, label),
    FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE issue_history (
    id VARCHAR(191) PRIMARY KEY,
    issue_id VARCHAR(191) NOT NULL,
    action TEXT NOT NULL,
    reason TEXT NOT NULL,
    from_status TEXT NOT NULL,
    to_status TEXT NOT NULL,
    created_at VARCHAR(64) NOT NULL,
    created_by TEXT NOT NULL,
    FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
);
-- +goose StatementEnd

CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);
CREATE INDEX idx_issues_rank ON issues(item_rank(191));
CREATE INDEX idx_relations_src_type ON relations(src_id, type);
CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);
CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);
CREATE INDEX idx_labels_issue ON labels(issue_id, label);
CREATE INDEX idx_labels_name ON labels(label, issue_id);
CREATE INDEX idx_issue_history_issue_created ON issue_history(issue_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS issue_history;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS relations;
DROP TABLE IF EXISTS issues;
DROP TABLE IF EXISTS meta;
