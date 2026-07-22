-- 保留期清理路径索引(CLAUDE.md §9 不变量 24,2026-07-21)。
-- fresh 库由 mysql-init 建表自带;已建 volume 的存量库由本版本条件补齐(幂等,在线加索引不锁写)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'auction_orders' AND INDEX_NAME = 'idx_terminal_purge') = 0,
    'ALTER TABLE `auction_orders` ADD KEY `idx_terminal_purge` (`status`, `updated_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_terminal_purge_auction_orders FROM @ddl;
EXECUTE stmt_idx_terminal_purge_auction_orders;
DEALLOCATE PREPARE stmt_idx_terminal_purge_auction_orders;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'auction_matches' AND INDEX_NAME = 'idx_settled_purge') = 0,
    'ALTER TABLE `auction_matches` ADD KEY `idx_settled_purge` (`settlement_status`, `event_pending`, `matched_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_settled_purge_auction_matches FROM @ddl;
EXECUTE stmt_idx_settled_purge_auction_matches;
DEALLOCATE PREPARE stmt_idx_settled_purge_auction_matches;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'auction_idempotency_keys' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `auction_idempotency_keys` ADD KEY `idx_created` (`created_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_created_auction_idempotency_keys FROM @ddl;
EXECUTE stmt_idx_created_auction_idempotency_keys;
DEALLOCATE PREPARE stmt_idx_created_auction_idempotency_keys;
