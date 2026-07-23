-- Pandora 社交库表结构(friend 服务,2026-06-15)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建 pandora_social 表)。
--
-- 表清单(对齐 docs/design/infra.md §2.1 pandora_social):
--   friendships     好友关系(双向各一行,uk player_id+friend_id,便于 ListFriends 单表查)
--   friend_requests 好友请求(request_id PK = snowflake,uk requester+target 防重复挂起)
--   blocks          黑名单(uk player_id+blocked_id;Block 时清好友关系 + 取消挂起请求)
--
-- 约定:
--   - 所有玩家 ID 均 BIGINT UNSIGNED(snowflake,不变量 §9.11 对齐 Go uint64)
--   - friendships 双向冗余:接受好友时插 (a,b) + (b,a) 两行,ListFriends 直接 WHERE player_id=?
--   - friend_requests.status:1 pending / 2 accepted / 3 rejected / 4 expired
--     (对齐 proto FriendRequestStatus enum 数值)
--   - 重复请求(同 requester→target 已挂起)走 ON DUPLICATE KEY 复用,返回既有 request_id 幂等
--   - 好友图是结构化列(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)

USE `pandora_social`;

CREATE TABLE IF NOT EXISTS `friendships` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `player_id`  BIGINT UNSIGNED NOT NULL COMMENT '关系归属玩家',
    `friend_id`  BIGINT UNSIGNED NOT NULL COMMENT '好友玩家',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '成为好友时间(since_ms 来源)',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_friend` (`player_id`, `friend_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友关系(双向各一行)';

CREATE TABLE IF NOT EXISTS `friend_requests` (
    `request_id`   BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 好友请求 ID(uint64)',
    `requester_id` BIGINT UNSIGNED NOT NULL COMMENT '发起方',
    `target_id`    BIGINT UNSIGNED NOT NULL COMMENT '接收方',
    `status`       TINYINT         NOT NULL DEFAULT 1 COMMENT '1 pending / 2 accepted / 3 rejected / 4 expired',
    `created_at`   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`request_id`),
    UNIQUE KEY `uk_requester_target` (`requester_id`, `target_id`),
    KEY `idx_target_status` (`target_id`, `status`),
    KEY `idx_status_updated` (`status`, `updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友请求(挂起 / 接受 / 拒绝;终态行保留 90 天由 friend sweep 清理,pending 永不清,§9.24)';
-- idx_status_updated 服务保留期清理(DELETE WHERE status<>pending AND updated_at<cutoff)。
-- 既有库需手动补:ALTER TABLE friend_requests ADD KEY idx_status_updated (status, updated_at);

CREATE TABLE IF NOT EXISTS `blocks` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `player_id`  BIGINT UNSIGNED NOT NULL COMMENT '拉黑发起方',
    `blocked_id` BIGINT UNSIGNED NOT NULL COMMENT '被拉黑玩家',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_blocked` (`player_id`, `blocked_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 黑名单';

-- 好友域并发守卫行(R5 复审 P1-2/3/4,2026-07-22):TiDB 无 gap/next-key 锁,
-- 限额校验与关系变更先锁守卫行(存在行的悲观点锁两库语义一致)再进入临界区;
-- 单 MySQL 下同样生效(与 TiDB DDL deploy/tidb-init/01-social-tidb.sql 同步维护)。
-- 行数有界:每玩家/每关系对至多 1 行(§9.24 登记豁免,同 auction_owner_guards)。
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

-- chat 服务私聊历史(2026-06-16)。
--   只有私聊(PRIVATE)落库支持离线 PullHistory;世界 / 队伍是即时频道,不持久化。
--   message_id PK = snowflake(uint64);按收发双方 + 时间倒序查历史。
--   content 是结构化列(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)。
CREATE TABLE IF NOT EXISTS `chat_private_messages` (
    `message_id`   BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 消息 ID(uint64)',
    `sender_id`    BIGINT UNSIGNED NOT NULL COMMENT '发送方玩家',
    `receiver_id`  BIGINT UNSIGNED NOT NULL COMMENT '接收方玩家',
    `content`      VARCHAR(512)    NOT NULL COMMENT '消息内容(服务端已校验长度 + 敏感词)',
    `send_time_ms` BIGINT          NOT NULL COMMENT '发送时间(毫秒,排序 / 翻页游标)',
    PRIMARY KEY (`message_id`),
    KEY `idx_pair_time` (`sender_id`, `receiver_id`, `send_time_ms`),
    KEY `idx_receiver_time` (`receiver_id`, `send_time_ms`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 私聊历史(离线 PullHistory)';
