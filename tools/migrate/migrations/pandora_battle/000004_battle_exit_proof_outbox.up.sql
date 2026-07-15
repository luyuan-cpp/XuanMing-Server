CREATE TABLE IF NOT EXISTS `battle_exit_proof_outbox` (
    `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `match_id`           BIGINT UNSIGNED NOT NULL,
    `player_id`          BIGINT UNSIGNED NOT NULL,
    `payload`            VARBINARY(2048) NOT NULL COMMENT 'battle_result internal immutable proof JSON',
    `prepared`           TINYINT UNSIGNED NOT NULL DEFAULT 0,
    `next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0,
    `attempt_count`      INT UNSIGNED NOT NULL DEFAULT 0,
    `superseded_at_ms`   BIGINT NOT NULL DEFAULT 0,
    `created_at_ms`      BIGINT NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_battle_exit_match_player` (`match_id`, `player_id`),
    KEY `idx_battle_exit_due` (`superseded_at_ms`, `next_attempt_at_ms`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Battle terminal/leave placement proof durable relay';
