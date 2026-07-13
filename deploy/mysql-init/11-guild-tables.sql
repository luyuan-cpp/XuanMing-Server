-- Pandora 公会 / 临时群表结构(guild 服务,2026-06-27)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库,06-social-tables.sql 建 friend/chat 表,本文件接着建公会 / 群表)。
--
-- 设计依据:docs/design/decision-revisit-chat-group.md。
-- 表清单(对齐 pandora_social 库):
--   guilds              公会(guild_id PK = snowflake;uk name 公会名唯一)
--   guild_members       公会成员(player_id PK = 单归属:玩家只属一个公会)
--   guild_join_requests 公会加入申请(request_id PK = snowflake;uk guild+player 防重复挂起)
--   chat_groups         临时群(group_id PK = snowflake)
--   chat_group_members  临时群成员(uk group+player = 多归属:玩家可在多个群)
--   player_group_counts 玩家所在群计数(max_groups_per_player 上限校验;与 TiDB 版一致)
--
-- 约定:
--   - 所有业务 ID 均 BIGINT UNSIGNED(snowflake,不变量 §9.11 对齐 Go uint64)
--   - 公会单归属:guild_members.player_id 为主键,强制玩家只在一个公会(类比"玩家只在一个 DS")
--   - 群组多归属:chat_group_members 主键 (group_id, player_id),玩家可在多个群
--   - guild_members.role / chat_group_members.role 对齐 proto enum 数值
--   - guild_join_requests.status:1 pending / 2 approved / 3 rejected(对齐 GuildJoinStatus)
--   - 表名 chat_groups(非 groups):groups 是部分 SQL 方言保留字,避免转义
--   - 成员关系是结构化列(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)

USE `pandora_social`;

CREATE TABLE IF NOT EXISTS `guilds` (
    `guild_id`     BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 公会 ID(uint64)',
    `name`         VARCHAR(64)     NOT NULL COMMENT '公会名(唯一)',
    `leader_id`    BIGINT UNSIGNED NOT NULL COMMENT '会长 player_id',
    `member_count` INT             NOT NULL DEFAULT 1 COMMENT '成员数(含会长)',
    `pending_request_count` INT    NOT NULL DEFAULT 0 COMMENT '挂起加入申请数(pending 上限校验计数列,与 TiDB 版一致)',
    `max_members`  INT             NOT NULL DEFAULT 100 COMMENT '成员上限',
    `created_at`   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间(created_ms 来源)',
    PRIMARY KEY (`guild_id`),
    UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 公会';

CREATE TABLE IF NOT EXISTS `guild_members` (
    `player_id` BIGINT UNSIGNED NOT NULL COMMENT '成员 player_id(单归属 → 主键)',
    `guild_id`  BIGINT UNSIGNED NOT NULL COMMENT '所属公会',
    `role`      TINYINT         NOT NULL DEFAULT 3 COMMENT '1 leader / 2 officer / 3 member',
    `joined_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '加入时间(joined_ms 来源)',
    PRIMARY KEY (`player_id`),
    KEY `idx_guild_role` (`guild_id`, `role`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 公会成员(单归属)';

CREATE TABLE IF NOT EXISTS `guild_join_requests` (
    `request_id` BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 申请 ID(uint64)',
    `guild_id`   BIGINT UNSIGNED NOT NULL COMMENT '目标公会',
    `player_id`  BIGINT UNSIGNED NOT NULL COMMENT '申请人',
    `status`     TINYINT         NOT NULL DEFAULT 1 COMMENT '1 pending / 2 approved / 3 rejected',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`request_id`),
    UNIQUE KEY `uk_guild_player` (`guild_id`, `player_id`),
    KEY `idx_guild_status` (`guild_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 公会加入申请(挂起 / 通过 / 拒绝)';

CREATE TABLE IF NOT EXISTS `chat_groups` (
    `group_id`     BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 群 ID(uint64)',
    `name`         VARCHAR(64)     NOT NULL COMMENT '群名',
    `owner_id`     BIGINT UNSIGNED NOT NULL COMMENT '群主 player_id',
    `member_count` INT             NOT NULL DEFAULT 1 COMMENT '成员数(含群主)',
    `max_members`  INT             NOT NULL DEFAULT 50 COMMENT '成员上限',
    `created_at`   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间(created_ms 来源)',
    PRIMARY KEY (`group_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 临时群';

CREATE TABLE IF NOT EXISTS `chat_group_members` (
    `group_id`  BIGINT UNSIGNED NOT NULL COMMENT '所属群',
    `player_id` BIGINT UNSIGNED NOT NULL COMMENT '成员 player_id',
    `role`      TINYINT         NOT NULL DEFAULT 2 COMMENT '1 owner / 2 member',
    `joined_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '加入时间(joined_ms 来源)',
    PRIMARY KEY (`group_id`, `player_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 临时群成员(多归属)';

-- per-player「我所在的群」计数表(max_groups_per_player 上限校验用;与 TiDB 版
-- deploy/tidb-init/01-social-tidb.sql 保持一致)。入群 / 退群同事务里锁该玩家计数行 + 增减
-- group_count,取代依赖间隙锁的 COUNT(*)...FOR UPDATE(TiDB 无间隙锁,MySQL 下亦是等价且更明确的写法)。
CREATE TABLE IF NOT EXISTS `player_group_counts` (
    `player_id`   BIGINT UNSIGNED NOT NULL COMMENT '玩家 player_id',
    `group_count` INT             NOT NULL DEFAULT 0 COMMENT '该玩家当前所在临时群数(max_groups_per_player 校验用)',
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家所在群计数(上限校验)';
