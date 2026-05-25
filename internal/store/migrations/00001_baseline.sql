-- FROZEN FILE — DO NOT EDIT. Active from this commit forward.
--
-- 00001_baseline.sql is the immutable definition of schema v1: the converged
-- shape every prior reconcile helper collectively produced. Once shipped, the
-- bytes of this file ARE the meaning of "v1" for every binary that embeds
-- them. Editing the file after release silently changes that meaning, which
-- bricks every workspace last-touched before the edit shipped (see PR #143 /
-- PR #145 — the recovery for the 2026-05-21 retcon incident that stranded
-- unreal-3d-maps and cc-nerf-buster).
--
-- Structural changes go in a NEW numbered migration file (00002_*.sql,
-- 00003_*.sql, ...). The frozen-file gate (TestBaselineFileIsFrozen in
-- baseline_frozen_test.go) enforces this; updating the pinned hash to make
-- the test pass is NOT the correct response — open a new migration file
-- instead. This applies even to comment-only or whitespace edits: the
-- binary's identity is tied to the file's bytes, and the gate cannot
-- distinguish "harmless" edits from structural ones (that distinction is
-- exactly what failed in the original incident).
--
-- A fresh workspace applies this; a pre-goose workspace already at this
-- shape is adopted (stamped v1) without re-running it. CHECK constraints
-- carry explicit deterministic names so SHOW CREATE TABLE is stable across
-- applies (the drift canary depends on it). Priority bounds mirror
-- model.PriorityNormal (0) and model.PriorityUrgent (1).
--
-- [LAW:one-source-of-truth] This file is the canonical definition of "what
-- v1 was when v1 shipped." A mutable baseline is not a single source — it's
-- a moving target, and what schema a binary's v1 produces depends on which
-- commit of the file the binary was built from.
-- [LAW:single-enforcer] The frozen-file test is the single enforcer of this
-- discipline. Reviewer attention and documentation discipline both failed
-- for the original incident; neither is the enforcer.
-- [LAW:types-are-the-program] The strongest theorem about a migration
-- registry is "each version_id maps to exactly one schema shape." A
-- rewritten baseline weakens it to "version_id maps to schema shape AT a
-- specific binary build," which the integer cannot encode. The gate keeps
-- the strong theorem true at the boundary.

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
	CONSTRAINT issues_status_check CHECK ((issue_type IN ('epic') AND status IS NULL) OR (issue_type NOT IN ('epic') AND status IS NOT NULL AND status IN ('open','in_progress','closed'))),
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
CREATE TABLE issue_events (
	id VARCHAR(191) PRIMARY KEY,
	issue_id VARCHAR(191) NOT NULL,
	action VARCHAR(64) NULL,
	reason TEXT NOT NULL,
	actor TEXT NOT NULL,
	created_at VARCHAR(64) NOT NULL,
	FOREIGN KEY (issue_id) REFERENCES issues(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE issue_event_changes (
	event_id VARCHAR(191) NOT NULL,
	field VARCHAR(64) NOT NULL,
	from_value TEXT NULL,
	to_value TEXT NULL,
	PRIMARY KEY (event_id, field),
	FOREIGN KEY (event_id) REFERENCES issue_events(id) ON DELETE CASCADE
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_issues_status_priority ON issues(status, priority, updated_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_issues_rank ON issues(item_rank(191));
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_relations_src_type ON relations(src_id, type);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_relations_dst_type ON relations(dst_id, type);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_comments_issue_created ON comments(issue_id, created_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_labels_issue ON labels(issue_id, label);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_labels_name ON labels(label, issue_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_issue_events_issue_created ON issue_events(issue_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS issue_event_changes;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS issue_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS labels;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS comments;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS relations;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS issues;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS meta;
-- +goose StatementEnd
