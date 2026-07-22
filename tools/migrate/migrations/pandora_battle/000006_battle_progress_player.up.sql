-- 000006_battle_progress_player — 实时进度单场单玩家累计表(2026-07-21 发布前审计)。
--
-- 设计要求的单玩家上限权威依据(realtime-progression.md 反作弊上限):只按场累计不够,
-- 失陷 DS 可把全场额度灌给一人。原拟原地修订 000005,但已versioned 迁移不可变
-- (是否所有环境都停留在 <5 无法持续证明),按迁移纪律另起本版本。
-- fresh-init 见 deploy/mysql-init/05-battle-outbox.sql 同名表(CREATE IF NOT EXISTS 幂等,双路兼容)。

CREATE TABLE IF NOT EXISTS `battle_progress_player` (
    `match_id`      BIGINT UNSIGNED NOT NULL,
    `player_id`     BIGINT UNSIGNED NOT NULL,
    `total_exp`     BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '本场该玩家累计入账经验(单玩家上限封顶依据)',
    `total_items`   INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '本场该玩家累计入账掉落件数',
    `total_kills`   INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '本场该玩家累计击杀数(反作弊上限依据)',
    `updated_at_ms` BIGINT          NOT NULL DEFAULT 0,
    PRIMARY KEY (`match_id`, `player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 战斗实时进度单玩家累计(单场单玩家 经验/掉落/击杀 上限,实时成长)';
