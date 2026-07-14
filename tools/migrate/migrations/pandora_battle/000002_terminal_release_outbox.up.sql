-- Battle 正常结算终态回收事务出箱(Model-B)。
-- additive-only：旧 battle_result 副本不知道本表，仍可与新副本滚动共存；
-- 新副本在 authority_mode=redis 时启动前会机械校验本表结构。
CREATE TABLE IF NOT EXISTS `terminal_release_outbox` (
    `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `match_id`          BIGINT UNSIGNED NOT NULL,
    `allocation_id`     CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `ds_pod_name`       VARCHAR(253) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `gameserver_uid`    VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `instance_epoch`    INT UNSIGNED NOT NULL,
    `auth_gen`          BIGINT UNSIGNED NOT NULL,
    `auth_jti`          VARCHAR(256) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `auth_exp_ms`       BIGINT NOT NULL,
    `auth_kid`          VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `auth_token_sha256` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `auth_writer_epoch` INT UNSIGNED NOT NULL,
    `authorized_at_ms`  BIGINT NOT NULL,
    `release_after_ms`  BIGINT NOT NULL,
    `released_at_ms`    BIGINT NOT NULL DEFAULT 0,
    `created_at_ms`     BIGINT NOT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_terminal_release_match` (`match_id`),
    KEY `idx_terminal_release_due` (`release_after_ms`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Battle Model-B 正常结算终态回收事务出箱';
