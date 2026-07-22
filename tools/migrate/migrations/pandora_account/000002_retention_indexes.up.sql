-- 保留期清理路径索引(CLAUDE.md §9 不变量 24,2026-07-21)。
-- fresh 库由 mysql-init 建表自带;已建 volume 的存量库由本版本条件补齐(幂等,在线加索引不锁写)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'account_devices' AND INDEX_NAME = 'idx_last_login') = 0,
    'ALTER TABLE `account_devices` ADD KEY `idx_last_login` (`last_login_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_last_login_account_devices FROM @ddl;
EXECUTE stmt_idx_last_login_account_devices;
DEALLOCATE PREPARE stmt_idx_last_login_account_devices;
