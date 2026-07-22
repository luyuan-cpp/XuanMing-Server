-- 保留期清理路径索引(CLAUDE.md §9 不变量 24,2026-07-21)。
-- fresh 库由 mysql-init 建表自带;已建 volume 的存量库由本版本条件补齐(幂等,在线加索引不锁写)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'inventory_ledger' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `inventory_ledger` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_created_inventory_ledger FROM @ddl;
EXECUTE stmt_idx_created_inventory_ledger;
DEALLOCATE PREPARE stmt_idx_created_inventory_ledger;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'auction_escrow' AND INDEX_NAME = 'idx_status_updated') = 0,
    'ALTER TABLE `auction_escrow` ADD KEY `idx_status_updated` (`status`, `updated_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE stmt_idx_status_updated_auction_escrow FROM @ddl;
EXECUTE stmt_idx_status_updated_auction_escrow;
DEALLOCATE PREPARE stmt_idx_status_updated_auction_escrow;
