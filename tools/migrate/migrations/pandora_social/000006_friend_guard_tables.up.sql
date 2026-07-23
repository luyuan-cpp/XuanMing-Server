-- 好友域并发守卫行表(R8 收口,2026-07-23):friend_player_guards / friend_pair_guards
-- 此前只存在于 fresh-init(deploy/mysql-init/06-social-tables.sql、deploy/tidb-init/
-- 01-social-tidb.sql),既有库升级没有对应迁移——friend 新版本上线后 acquirePairGuard/
-- acquirePlayerGuard 首条 INSERT 即炸(对客户端表现为好友操作全量内部错误)。
--
-- 语义(R5 复审 P1-2/3/4):TiDB 无 gap/next-key 锁,限额校验与关系变更先锁守卫行
-- (存在行的悲观点锁两库语义一致)再进入临界区;单 MySQL 下同样生效。
-- 行数有界:每玩家/每关系对至多 1 行(§9.24 登记豁免,同 auction_owner_guards)。
-- CREATE TABLE IF NOT EXISTS 幂等,fresh 库重放安全。

CREATE TABLE IF NOT EXISTS `friend_player_guards` (
    `player_id` BIGINT UNSIGNED NOT NULL COMMENT '守卫行归属玩家(锁粒度=单玩家限额域)',
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友域每玩家写守卫行(上限校验串行化;§9.24 豁免)';

CREATE TABLE IF NOT EXISTS `friend_pair_guards` (
    `lo_id` BIGINT UNSIGNED NOT NULL COMMENT '关系对较小 player_id',
    `hi_id` BIGINT UNSIGNED NOT NULL COMMENT '关系对较大 player_id',
    PRIMARY KEY (`lo_id`, `hi_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友域关系对写守卫行(Accept/Block/AddFriend 同对串行化;§9.24 豁免)';
