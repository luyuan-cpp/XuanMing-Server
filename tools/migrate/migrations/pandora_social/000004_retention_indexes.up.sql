-- 保留期清理路径索引(CLAUDE.md §9 不变量 24,2026-07-21)。
-- fresh 库由 mysql-init 建表自带;已建 volume 的存量库由本版本条件补齐(幂等,在线加索引不锁写)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'friend_requests' AND INDEX_NAME = 'idx_status_updated') = 0,
    'ALTER TABLE `friend_requests` ADD KEY `idx_status_updated` (`status`, `updated_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_status_updated_friend_requests FROM @ddl;
EXECUTE stmt_idx_status_updated_friend_requests;
DEALLOCATE PREPARE stmt_idx_status_updated_friend_requests;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'guild_join_requests' AND INDEX_NAME = 'idx_status_updated') = 0,
    'ALTER TABLE `guild_join_requests` ADD KEY `idx_status_updated` (`status`, `updated_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_status_updated_guild_join_requests FROM @ddl;
EXECUTE stmt_idx_status_updated_guild_join_requests;
DEALLOCATE PREPARE stmt_idx_status_updated_guild_join_requests;
