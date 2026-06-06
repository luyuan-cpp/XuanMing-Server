-- Pandora 战斗结算库 W4 ③ 表结构(2026-06-06)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建 pandora_battle 库 + grant,本文件接着建表)。
--
-- 表清单:
--   battles               一场对局的结算头(幂等键 = match_id,不变量 §2)
--   battle_player_stats   每个玩家在该对局的战绩 + MMR 变化(MMR 在后端算,不变量 §6)
--
-- 约定:
--   - match_id / player_id 是 snowflake uint64(BIGINT UNSIGNED,不变量 §11)
--   - *_at_ms 是毫秒时间戳(BIGINT,由 DS / 应用层提供,UTC)
--   - outcome:1=normal 正常结算,2=abandoned DS 崩溃补偿(mmr_delta 全 0,不变量 §4)
--   - winner_team:0=A 胜,1=B 胜,2=平/无效
--   - mmr_delta:battle_result 用 Elo 计算后回填(DS 上报值不可信,被覆盖)

USE `pandora_battle`;

CREATE TABLE IF NOT EXISTS `battles` (
    `match_id`     BIGINT UNSIGNED  NOT NULL COMMENT 'snowflake match_id,幂等键',
    `started_at_ms` BIGINT          NOT NULL DEFAULT 0,
    `ended_at_ms`   BIGINT          NOT NULL DEFAULT 0,
    `winner_team`   TINYINT          NOT NULL DEFAULT 2 COMMENT '0=A 胜,1=B 胜,2=平/无效',
    `outcome`       TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '1=normal,2=abandoned(DS 崩溃补偿)',
    `ds_pod_name`   VARCHAR(128)     NOT NULL DEFAULT '',
    `game_mode`     VARCHAR(32)      NOT NULL DEFAULT '',
    `map_id`        INT UNSIGNED     NOT NULL DEFAULT 0,
    `created_at`    DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`match_id`),
    KEY `idx_ended` (`ended_at_ms`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 对局结算头(幂等键 match_id)';

CREATE TABLE IF NOT EXISTS `battle_player_stats` (
    `id`           BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `match_id`     BIGINT UNSIGNED  NOT NULL,
    `player_id`    BIGINT UNSIGNED  NOT NULL,
    `hero_id`      INT UNSIGNED     NOT NULL DEFAULT 0,
    `team`         TINYINT          NOT NULL DEFAULT 0 COMMENT '0 或 1',
    `kills`        INT              NOT NULL DEFAULT 0,
    `deaths`       INT              NOT NULL DEFAULT 0,
    `assists`      INT              NOT NULL DEFAULT 0,
    `damage_dealt` BIGINT           NOT NULL DEFAULT 0,
    `damage_taken` BIGINT           NOT NULL DEFAULT 0,
    `healing`      BIGINT           NOT NULL DEFAULT 0,
    `gold`         BIGINT           NOT NULL DEFAULT 0,
    `mmr_delta`    INT              NOT NULL DEFAULT 0 COMMENT '段位变化(后端 Elo 算,不变量 §6)',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_match_player` (`match_id`, `player_id`),
    KEY `idx_player_match` (`player_id`, `match_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 对局玩家战绩 + MMR 变化';
