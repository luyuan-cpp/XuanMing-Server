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
