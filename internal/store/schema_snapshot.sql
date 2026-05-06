-- AUTO-GENERATED: do not hand-edit.
-- Regenerate with: LIT_REGEN_SCHEMA_SNAPSHOT=1 go test ./internal/store -run TestSchemaSnapshotMatches
-- This file is the canonical converged schema after all registered goose migrations apply.
-- CI fails if a migration body changes the resulting schema and this file is not updated in the same commit.

CREATE TABLE `meta` (
  `meta_key` varchar(191) NOT NULL,
  `meta_value` text NOT NULL,
  PRIMARY KEY (`meta_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `issues` (
  `id` varchar(191) NOT NULL,
  `title` text NOT NULL,
  `description` text NOT NULL,
  `agent_prompt` text,
  `status` varchar(32),
  `priority` int NOT NULL,
  `issue_type` varchar(32) NOT NULL,
  `topic` varchar(191) NOT NULL,
  `assignee` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `updated_at` varchar(64) NOT NULL,
  `closed_at` varchar(64),
  `archived_at` varchar(64),
  `deleted_at` varchar(64),
  `item_rank` text NOT NULL DEFAULT '',
  PRIMARY KEY (`id`),
  KEY `idx_issues_rank` (`item_rank`(191)),
  KEY `idx_issues_status_priority` (`status`,`priority`,`updated_at`),
  CONSTRAINT `issues_status_check` CHECK ((((`issue_type` IN ('epic')) AND `status` IS NULL) OR (((NOT((`issue_type` IN ('epic')))) AND (NOT(`status` IS NULL))) AND (`status` IN ('open', 'in_progress', 'closed'))))),
  CONSTRAINT `issues_priority_check` CHECK (((`priority` >= 0) AND (`priority` <= 1))),
  CONSTRAINT `issues_type_check` CHECK ((`issue_type` IN ('task', 'feature', 'bug', 'chore', 'epic')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `relations` (
  `src_id` varchar(191) NOT NULL,
  `dst_id` varchar(191) NOT NULL,
  `type` varchar(32) NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`src_id`,`dst_id`,`type`),
  KEY `dst_id` (`dst_id`),
  KEY `idx_relations_dst_type` (`dst_id`,`type`),
  KEY `idx_relations_src_type` (`src_id`,`type`),
  CONSTRAINT `relations_ibfk_1` FOREIGN KEY (`src_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `relations_ibfk_2` FOREIGN KEY (`dst_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `relations_type_check` CHECK ((`type` IN ('blocks', 'parent-child', 'related-to')))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `comments` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `body` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_comments_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `comments_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `labels` (
  `issue_id` varchar(191) NOT NULL,
  `label` varchar(191) NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`issue_id`,`label`),
  KEY `idx_labels_issue` (`issue_id`,`label`),
  KEY `idx_labels_name` (`label`,`issue_id`),
  CONSTRAINT `labels_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `issue_history` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `action` text NOT NULL,
  `reason` text NOT NULL,
  `from_status` text NOT NULL,
  `to_status` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_issue_history_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `issue_history_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

