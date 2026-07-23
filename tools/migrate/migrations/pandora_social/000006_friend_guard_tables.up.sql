-- 好友域并发守卫行表(R8 收口,2026-07-23):friend_player_guards / friend_pair_guards
-- 此前只存在于 fresh-init(deploy/mysql-init/06-social-tables.sql、deploy/tidb-init/
-- 01-social-tidb.sql),既有库升级没有对应迁移——friend 新版本上线后 acquirePairGuard/
-- acquirePlayerGuard 首条 INSERT 即炸(对客户端表现为好友操作全量内部错误)。
--
-- 语义(R5 复审 P1-2/3/4):TiDB 无 gap/next-key 锁,限额校验与关系变更先锁守卫行
-- (存在行的悲观点锁两库语义一致)再进入临界区;单 MySQL 下同样生效。
-- 行数界定(R9 复审 P1):
--   - friend_player_guards 每玩家至多 1 行,被玩家数有界(§9.24 登记豁免,同
--     auction_owner_guards);
--   - friend_pair_guards 每关系对 1 行,关系对随社交图 O(n²) 累积无上界 → 不能豁免。
--     守卫行只是锁载体无业务数据,任意时刻删除都安全(正在被持有的行锁会阻塞
--     DELETE 到提交;下次 acquire 重新 INSERT),故增设 created_at + 保留期 sweep
--     (friend 服务与 friend_requests 同轮清理,§9.24 登记为 swept)。
-- CREATE TABLE IF NOT EXISTS 幂等,fresh 库重放安全。

CREATE TABLE IF NOT EXISTS `friend_player_guards` (
    `player_id` BIGINT UNSIGNED NOT NULL COMMENT '守卫行归属玩家(锁粒度=单玩家限额域)',
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友域每玩家写守卫行(上限校验串行化;§9.24 豁免)';

CREATE TABLE IF NOT EXISTS `friend_pair_guards` (
    `lo_id` BIGINT UNSIGNED NOT NULL COMMENT '关系对较小 player_id',
    `hi_id` BIGINT UNSIGNED NOT NULL COMMENT '关系对较大 player_id',
    `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '首次取守卫时间(保留期 sweep 依据)',
    PRIMARY KEY (`lo_id`, `hi_id`),
    KEY `idx_created` (`created_at`) COMMENT '保留期 sweep 扫描索引(§9.24)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友域关系对写守卫行(Accept/Block/AddFriend 同对串行化;保留期 sweep,§9.24)';

-- 存量补齐:旧 fresh-init/早期 R8 形态可能已建了**无 created_at** 的 friend_pair_guards,
-- CREATE TABLE IF NOT EXISTS 对已存在表不生效,须按 information_schema 守卫补列/补索引
-- (对齐 000003 的幂等 DDL 模式;MySQL 8 的 ADD COLUMN/ADD INDEX 无 IF NOT EXISTS)。
SET @pandora_col_exists := (
    SELECT COUNT(*)
    FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'friend_pair_guards'
      AND column_name = 'created_at'
);
SET @pandora_sql := IF(
    @pandora_col_exists = 0,
    'ALTER TABLE `friend_pair_guards` ADD COLUMN `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT ''首次取守卫时间(保留期 sweep 依据)''',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_idx_exists := (
    SELECT COUNT(*)
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'friend_pair_guards'
      AND index_name = 'idx_created'
);
SET @pandora_sql := IF(
    @pandora_idx_exists = 0,
    'ALTER TABLE `friend_pair_guards` ADD INDEX `idx_created` (`created_at`)',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;
