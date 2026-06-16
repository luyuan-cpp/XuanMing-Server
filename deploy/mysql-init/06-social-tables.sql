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
    KEY `idx_target_status` (`target_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 好友请求(挂起 / 接受 / 拒绝)';

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
