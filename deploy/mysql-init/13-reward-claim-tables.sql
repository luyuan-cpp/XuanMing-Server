-- Pandora 玩家领奖记录表(2026-06-30)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行,
-- 本文件在 04-player-tables.sql(建 pandora_player 库与玩家档案表)之后执行。
--
-- 表清单(对齐 docs/design/infra.md §2.1 pandora_player):
--   player_reward_claims  玩家领奖记录(player_id PK;record 为 RewardClaimStorageRecord
--                         序列化 bytes;version 乐观锁)
--
-- 设计要点:
--   - 与玩家档案同库(pandora_player)同 player_id:回档随库一起恢复、合服随 player_id
--     一起迁移,领奖状态与玩家数据天然原子(不变量 §2 / §7 幂等;回档不撕裂)
--   - record 列是上层 pkg/rewardclaim 序列化好的 RewardClaimStorageRecord 不透明 bytes
--     (CLAUDE.md §5.8 存储快照 / §5.9 blob 场景用 proto bytes),DB 不解释其内容
--   - version 乐观锁:UPDATE ... WHERE player_id=? AND version=? 防并发覆盖
--   - 永久类(签到 / 成就 / 新手 / 永久任务)与活动类(按活动实例 ID,下线整条回收)
--     都编码在同一条 record bytes 内,见 proto RewardClaimStorageRecord 注释

USE `pandora_player`;

CREATE TABLE IF NOT EXISTS `player_reward_claims` (
    `player_id`  BIGINT UNSIGNED NOT NULL,
    `record`     LONGBLOB        NOT NULL COMMENT 'RewardClaimStorageRecord 序列化 bytes(永久+活动领取位图)',
    `version`    INT             NOT NULL DEFAULT 0 COMMENT '乐观锁版本',
    `updated_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家领奖记录(永久+活动 bitmap 快照)';
