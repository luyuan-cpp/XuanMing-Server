-- 000007_battle_retention_indexes — pandora_battle 保留期清理路径索引(§9.24,2026-07-21)。
--
-- battle_result biz/retention.go 周期清理:
--   battles/battle_player_stats  按服务端落库时间 created_at 超期批删 → 需 idx_created
--   progress stream/player       按 settled_at_ms(服务端结算打标)超期批删 → 需 idx_settled
-- 无前导索引时,行全部未到期的稳态下每轮全表扫描(多副本各扫一遍)。
--
-- 条件建索引(幂等):fresh-init(03/05-*.sql)建表已含,只有跑过旧版建表的存量库需补齐。
-- ALGORITHM=INPLACE 在线加索引不锁写。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'battles' AND INDEX_NAME = 'idx_created') = 0,
    'ALTER TABLE `battles` ADD KEY `idx_created` (`created_at`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_battles_created_idx FROM @ddl;
EXECUTE add_battles_created_idx;
DEALLOCATE PREPARE add_battles_created_idx;

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.STATISTICS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'battle_progress_stream' AND INDEX_NAME = 'idx_settled') = 0,
    'ALTER TABLE `battle_progress_stream` ADD KEY `idx_settled` (`settled_at_ms`), ALGORITHM=INPLACE',
    'SELECT 1');
PREPARE add_progress_settled_idx FROM @ddl;
EXECUTE add_progress_settled_idx;
DEALLOCATE PREPARE add_progress_settled_idx;
