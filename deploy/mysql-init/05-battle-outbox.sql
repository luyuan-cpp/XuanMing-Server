-- Pandora 战斗结算 player.update 事务出箱表 W4 ⑨(2026-06-06)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01 建库,03 建结算表,本文件接着建出箱表)。
--
-- 背景(不变量 §4 + HANDOFF §3 Step 2「可靠补偿收口」):
--   W4 ③ battle_result 落库后直接发 pandora.player.update(best-effort 弱依赖),
--   Kafka 不可用时事件直接丢 → 玩家段位永不更新,补偿不可靠。
--   W4 ⑨ 引入「事务出箱」(transactional outbox):落 battles + battle_player_stats
--   的同一事务里再写一行 player.update 出箱记录,二者原子提交;后台发布器轮询出箱
--   表逐条投递 Kafka,投递成功才删行。配合 player 服务幂等消费(W4 ④ mmr_history
--   uk),整条段位写链是 at-least-once 可靠闭环,可穿越 Kafka 临时不可用。
--
-- 约定:
--   - match_id / player_id 是 snowflake uint64(BIGINT UNSIGNED,不变量 §11)
--   - payload 是 player.v1.PlayerUpdateEvent 的 proto 序列化字节
--   - uk_match_player:同一对局同一玩家只入一行(防重复入箱;落库本身按 match_id 幂等,
--     正常路径不会重入,uk 是防御性兜底)
--   - 投递成功即 DELETE 该行,出箱表只保留待发布事件,不会无限增长

USE `pandora_battle`;

CREATE TABLE IF NOT EXISTS `player_update_outbox` (
    `id`            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `match_id`      BIGINT UNSIGNED  NOT NULL,
    `player_id`     BIGINT UNSIGNED  NOT NULL,
    `payload`       VARBINARY(512)   NOT NULL COMMENT 'player.v1.PlayerUpdateEvent proto bytes',
    `created_at_ms` BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_match_player` (`match_id`, `player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora player.update 事务出箱(at-least-once 可靠补偿,不变量 §4)';

-- ── 战斗装备掉落事务出箱 W5 ④(2026-07-08)────────────────────────────────────
--
-- 背景:DS 上报的战斗装备掉落(BattleResult.PlayerStats.dropped_item_config_ids)
--   必须可靠、幂等地落进 inventory(装备实例,不可丢也不可重复)。沿用 player.update
--   同款「事务出箱」:落 battles + battle_player_stats 的同一事务里,对每个有掉落的玩家
--   再写一行 drop 出箱记录(原子提交);后台 RunDropPublisher 轮询本表,逐行调
--   inventory.GrantInstances(幂等键 battle_drop:{match_id}:{player_id}),成功才删行。
--
-- 与 player.update 出箱的差异:
--   - 掉落无跨玩家保序需求 → 发布器按行独立重试(单行失败不阻塞其他玩家)。
--   - item_config_ids 存 CSV(如 "5001,5002");GrantInstances 幂等,重放安全。
--   - DS 不可信:写入本表前 battle_result 已按 drop_whitelist 过滤,非白名单 ID 不入表;
--     且每玩家按 max_drop_per_player 截断(默认 32,硬上限 46 = VARCHAR(512) 可容纳的
--     最大条数:46 个 10 位 uint32 + 45 逗号 = 505 字符),防超长 CSV 打失败整场结算。
--
-- 约定:
--   - uk_match_player:同对局同玩家只入一行(落库按 match_id 幂等,uk 为防御性兜底)。
--   - 投递成功即 DELETE,本表只保留待发放掉落,不会无限增长(容量满除外,见服务文档)。

CREATE TABLE IF NOT EXISTS `battle_drop_outbox` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `match_id`        BIGINT UNSIGNED  NOT NULL,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `item_config_ids` VARCHAR(512)     NOT NULL COMMENT 'CSV of dropped item_config_id, e.g. 5001,5002',
    `created_at_ms`   BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_match_player` (`match_id`, `player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 战斗装备掉落事务出箱(at-least-once 幂等发放 GrantInstances,W5 ④)';
