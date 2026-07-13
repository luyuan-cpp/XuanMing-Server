-- 公会 / 临时群上限计数 expand 迁移(MySQL 8 / TiDB)。
--
-- 旧二进制只读写 guild_join_requests / chat_group_members，不会引用下面的新结构；
-- 因此必须先在 MySQL 完成本迁移，再滚动发布兼容自愈版 guild。backfill 只是初始快照：
-- 混跑期旧 Pod 的后续写仍可能让 counter 暂时漂移，兼容新版每次写都会锁旧版明细范围并按
-- 实际明细绝对值校正。所有旧 Pod 排空前禁止把 guild 切到不提供 gap lock 的 TiDB。
-- docker-init / TiDB fresh schema 已含最终结构，所有 DDL 都允许在该形态下幂等执行。

SET @pandora_pending_request_count_exists := (
    SELECT COUNT(*)
    FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'guilds'
      AND column_name = 'pending_request_count'
);
SET @pandora_sql := IF(
    @pandora_pending_request_count_exists = 0,
    'ALTER TABLE `guilds` ADD COLUMN `pending_request_count` INT NOT NULL DEFAULT 0 COMMENT ''挂起加入申请数(pending 上限校验计数列,与 TiDB 版一致)'' AFTER `member_count`',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

CREATE TABLE IF NOT EXISTS `player_group_counts` (
    `player_id`   BIGINT UNSIGNED NOT NULL COMMENT '玩家 player_id',
    `group_count` INT             NOT NULL DEFAULT 0 COMMENT '该玩家当前所在临时群数(max_groups_per_player 校验用)',
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家所在群计数(上限校验)';

-- 从旧 schema 明细表确定性重建计数。状态 1 才是 pending；已批准 / 已拒绝申请不计入。
UPDATE `guilds` AS g
LEFT JOIN (
    SELECT `guild_id`, COUNT(*) AS `pending_count`
    FROM `guild_join_requests`
    WHERE `status` = 1
    GROUP BY `guild_id`
) AS p ON p.`guild_id` = g.`guild_id`
SET g.`pending_request_count` = COALESCE(p.`pending_count`, 0);

-- 先移除明细中已无成员关系的陈旧计数行，再按 chat_group_members 权威明细 upsert。
-- 正常 old-schema 升级时该表刚创建且为空；这两步同时保证 fresh schema / 预建表形态结果一致。
DELETE pgc
FROM `player_group_counts` AS pgc
LEFT JOIN (
    SELECT DISTINCT `player_id`
    FROM `chat_group_members`
) AS m ON m.`player_id` = pgc.`player_id`
WHERE m.`player_id` IS NULL;

INSERT INTO `player_group_counts` (`player_id`, `group_count`)
SELECT `player_id`, COUNT(*)
FROM `chat_group_members`
GROUP BY `player_id`
ON DUPLICATE KEY UPDATE `group_count` = VALUES(`group_count`);
