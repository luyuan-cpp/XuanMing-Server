-- 000005_battle_progress — 战斗中实时进度通道(实时成长,2026-07-20 realtime-progression.md)。
--
-- 纯 additive(不变量 §16/§17 不停服):两张新表,旧副本不引用,新旧共存安全。
-- 语义见 deploy/mysql-init/05-battle-outbox.sql 同名表注释。

CREATE TABLE IF NOT EXISTS `battle_progress_stream` (
    `match_id`         BIGINT UNSIGNED NOT NULL,
    `last_applied_seq` BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `final_seq`        BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'DS 结算上报的对账水位(0=未走实时通道)',
    `settled_at_ms`    BIGINT          NOT NULL DEFAULT 0 COMMENT '>0 = 对局已结算,进度一律拒',
    `updated_at_ms`    BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (`match_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 战斗实时进度水位(去重 + 结算 fencing,实时成长)';

CREATE TABLE IF NOT EXISTS `battle_progress_outbox` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `match_id`        BIGINT UNSIGNED NOT NULL,
    `seq`             BIGINT UNSIGNED NOT NULL COMMENT '批末事件 seq(幂等键组成)',
    `player_id`       BIGINT UNSIGNED NOT NULL,
    `kind`            TINYINT UNSIGNED NOT NULL COMMENT '1=exp(player.AddExperience) 2=item(inventory.GrantInstances)',
    `exp_delta`       BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'kind=1:本行经验(批内聚合)',
    `item_config_ids` VARCHAR(512)    NOT NULL DEFAULT '' COMMENT 'kind=2:CSV item_config_id(每 ID 一件实例)',
    `created_at_ms`   BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_match_seq_player_kind` (`match_id`, `seq`, `player_id`, `kind`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 战斗实时进度发放事务出箱(at-least-once + 下游幂等,实时成长)';
