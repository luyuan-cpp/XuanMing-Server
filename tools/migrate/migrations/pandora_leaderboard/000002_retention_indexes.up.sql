-- 保留期清理路径索引(CLAUDE.md §9 不变量 24,2026-07-21)。
-- fresh 库由 mysql-init 建表自带;已建 volume 的存量库由本版本条件补齐(幂等,在线加索引不锁写)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'leaderboard_snapshot' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `leaderboard_snapshot` ADD KEY `idx_created` (`created_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_created_leaderboard_snapshot FROM @ddl;
EXECUTE stmt_idx_created_leaderboard_snapshot;
DEALLOCATE PREPARE stmt_idx_created_leaderboard_snapshot;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'leaderboard_reward_log' AND INDEX_NAME = 'idx_status_updated') = 0,
    'ALTER TABLE `leaderboard_reward_log` ADD KEY `idx_status_updated` (`status`, `updated_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_status_updated_leaderboard_reward_log FROM @ddl;
EXECUTE stmt_idx_status_updated_leaderboard_reward_log;
DEALLOCATE PREPARE stmt_idx_status_updated_leaderboard_reward_log;
