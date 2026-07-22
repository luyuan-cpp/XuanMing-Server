-- 000003_retention_indexes — pandora_player 保留期清理路径索引(§9.24,2026-07-21)。
--
-- 清理任务按 `DELETE ... WHERE created_at < ? LIMIT ?` 回收超期行(exp_history 由
-- RunExpHistoryJanitor,mmr_history / attr_point_grants / talent_point_grants 由
-- RunHistoryJanitor,均默认关):无 created_at 前导索引时,行全部未到期的稳态下每轮
-- 全表扫描(多副本各扫一遍)。
--
-- 条件建索引(幂等):deploy/mysql-init/04-player-tables.sql fresh-init 建表已含
-- idx_created,只有跑过旧版建表的存量库需要补齐。ALGORITHM=INPLACE 在线加索引不锁写。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'exp_history' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `exp_history` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_exp_history_idx FROM @ddl;
EXECUTE add_exp_history_idx;
DEALLOCATE PREPARE add_exp_history_idx;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'mmr_history' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `mmr_history` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_mmr_history_idx FROM @ddl;
EXECUTE add_mmr_history_idx;
DEALLOCATE PREPARE add_mmr_history_idx;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'attr_point_grants' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `attr_point_grants` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_attr_grants_idx FROM @ddl;
EXECUTE add_attr_grants_idx;
DEALLOCATE PREPARE add_attr_grants_idx;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'talent_point_grants' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `talent_point_grants` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_talent_grants_idx FROM @ddl;
EXECUTE add_talent_grants_idx;
DEALLOCATE PREPARE add_talent_grants_idx;
