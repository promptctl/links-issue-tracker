-- schema_snapshot.sql — GENERATED, DO NOT HAND-EDIT.
--
-- Canonical SHOW CREATE TABLE of every application table after a fresh
-- workspace applies the goose registry (00001_baseline.sql + any later
-- migrations). The drift canary regenerates this and byte-compares; a mismatch
-- means the schema reached the database through something other than a
-- migration, or a migration changed shape without this snapshot being refreshed.
--
-- Regenerate:
--   go test ./internal/store -run TestSchemaSnapshotMatchesConvergedSchema -update-schema-snapshot
--
-- goose_db_version is intentionally excluded: it is goose's bookkeeping table,
-- not application schema, and its AUTO_INCREMENT counter is not deterministic.

CREATE TABLE `comments` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `body` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_comments_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `comments_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `comments_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `issue_event_changes` (
  `event_id` varchar(191) NOT NULL,
  `field` varchar(64) NOT NULL,
  `from_value` text,
  `to_value` text,
  PRIMARY KEY (`event_id`,`field`),
  CONSTRAINT `issue_event_changes_ibfk_1` FOREIGN KEY (`event_id`) REFERENCES `issue_events` (`id`) ON DELETE CASCADE,
  CONSTRAINT `issue_event_changes_from_value_stamp` CHECK ((((NOT((`field` IN ('closed_at', 'archived_at', 'deleted_at')))) OR `from_value` IS NULL) OR (regexp_like(`from_value`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`from_value`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`from_value`, 1, 19))))),
  CONSTRAINT `issue_event_changes_to_value_stamp` CHECK ((((NOT((`field` IN ('closed_at', 'archived_at', 'deleted_at')))) OR `to_value` IS NULL) OR (regexp_like(`to_value`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`to_value`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`to_value`, 1, 19)))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `issue_events` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `action` varchar(64),
  `reason` text NOT NULL,
  `actor` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `stream_id` varchar(64),
  `workspace_id` varchar(191),
  PRIMARY KEY (`id`),
  KEY `idx_issue_events_issue_created` (`issue_id`,`created_at`),
  KEY `issue_id` (`issue_id`),
  CONSTRAINT `issue_events_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `issue_events_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19))))
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
  `lane` text NOT NULL DEFAULT '',
  `resolution` varchar(32),
  `redirect_target` varchar(191),
  `follows_up` varchar(191),
  `work_plan` text NOT NULL DEFAULT '',
  PRIMARY KEY (`id`),
  KEY `idx_issues_rank` (`item_rank`(191)),
  KEY `idx_issues_status_priority` (`status`,`priority`,`updated_at`),
  CONSTRAINT `issues_status_check` CHECK ((((`issue_type` IN ('epic')) AND `status` IS NULL) OR (((NOT((`issue_type` IN ('epic')))) AND (NOT(`status` IS NULL))) AND (`status` IN ('open', 'in_progress', 'closed'))))),
  CONSTRAINT `issues_priority_check` CHECK (((`priority` >= 0) AND (`priority` <= 1))),
  CONSTRAINT `issues_type_check` CHECK ((`issue_type` IN ('task', 'feature', 'bug', 'chore', 'epic'))),
  CONSTRAINT `issues_resolution_check` CHECK ((`resolution` IS NULL OR (`resolution` IN ('duplicate', 'superseded', 'obsolete', 'wontfix')))),
  CONSTRAINT `issues_redirect_target_check` CHECK ((`redirect_target` IS NULL OR (`resolution` IN ('duplicate', 'superseded')))),
  CONSTRAINT `issues_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19)))),
  CONSTRAINT `issues_updated_at_stamp` CHECK ((regexp_like(`updated_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`updated_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`updated_at`, 1, 19)))),
  CONSTRAINT `issues_closed_at_stamp` CHECK ((`closed_at` IS NULL OR (regexp_like(`closed_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`closed_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`closed_at`, 1, 19))))),
  CONSTRAINT `issues_archived_at_stamp` CHECK ((`archived_at` IS NULL OR (regexp_like(`archived_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`archived_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`archived_at`, 1, 19))))),
  CONSTRAINT `issues_deleted_at_stamp` CHECK ((`deleted_at` IS NULL OR (regexp_like(`deleted_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`deleted_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`deleted_at`, 1, 19)))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `labels` (
  `issue_id` varchar(191) NOT NULL,
  `label` varchar(191) NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`issue_id`,`label`),
  KEY `idx_labels_issue` (`issue_id`,`label`),
  KEY `idx_labels_name` (`label`,`issue_id`),
  CONSTRAINT `labels_ibfk_1` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `labels_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `meta` (
  `meta_key` varchar(191) NOT NULL,
  `meta_value` text NOT NULL,
  PRIMARY KEY (`meta_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `migration_quarantine` (
  `version` bigint NOT NULL,
  `name` text NOT NULL,
  `error_text` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  PRIMARY KEY (`version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `plan_reviews` (
  `id` varchar(191) NOT NULL,
  `issue_id` varchar(191) NOT NULL,
  `plan_digest` varchar(64) NOT NULL,
  `verdict` varchar(16) NOT NULL,
  `laws` text NOT NULL,
  `created_at` varchar(64) NOT NULL,
  `created_by` text NOT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_plan_reviews_issue_created` (`issue_id`,`created_at`),
  CONSTRAINT `plan_reviews_issue` FOREIGN KEY (`issue_id`) REFERENCES `issues` (`id`) ON DELETE CASCADE,
  CONSTRAINT `plan_reviews_verdict_check` CHECK ((`verdict` IN ('pass', 'fail'))),
  CONSTRAINT `plan_reviews_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;

CREATE TABLE `purges` (
  `issue_id` varchar(191) NOT NULL,
  `issue_created_at` varchar(64) NOT NULL,
  `purged_at` varchar(64) NOT NULL,
  `purged_by` text NOT NULL,
  PRIMARY KEY (`issue_id`,`issue_created_at`),
  CONSTRAINT `purges_issue_created_at_stamp` CHECK ((regexp_like(`issue_created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`issue_created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`issue_created_at`, 1, 19)))),
  CONSTRAINT `purges_purged_at_stamp` CHECK ((regexp_like(`purged_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`purged_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`purged_at`, 1, 19))))
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
  CONSTRAINT `relations_type_check` CHECK ((`type` IN ('blocked-by', 'parent-child', 'related-to'))),
  CONSTRAINT `relations_created_at_stamp` CHECK ((regexp_like(`created_at`,'^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9][.][0-9]{9}Z\\z') AND (date_format(str_to_date(coalesce(SUBSTRING(`created_at`, 1, 19),'-'),'%Y-%m-%dT%H:%i:%s'), '%Y-%m-%dT%H:%i:%s') = SUBSTRING(`created_at`, 1, 19))))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;
