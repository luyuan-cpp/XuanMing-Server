CREATE TABLE IF NOT EXISTS `match_release_outbox` (
    `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `match_id`           BIGINT UNSIGNED NOT NULL,
    `payload`            VARBINARY(1024) NOT NULL COMMENT 'match.v1.MatchReleaseStorageRecord proto bytes',
    `next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0,
    `attempt_count`      INT UNSIGNED NOT NULL DEFAULT 0,
    `created_at_ms`      BIGINT NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_match_release_match` (`match_id`),
    KEY `idx_match_release_due` (`next_attempt_at_ms`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Battle 结算后可靠释放 match/ticket/player claim';
