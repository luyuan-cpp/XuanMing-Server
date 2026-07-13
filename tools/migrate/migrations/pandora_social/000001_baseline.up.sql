-- Pandora pandora_social baseline (golang-migrate v000001)
-- 自动从 deploy/mysql-init/*.sql 生成(去 USE 行;DSN 已指定库)。
-- 这是该库的建表基线;后续结构改动新增 000002_*.up.sql,勿改本文件。

-- ===== from 06-social-tables.sql =====
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

-- ===== from 11-guild-tables.sql =====
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
--
-- 约定:
--   - 所有业务 ID 均 BIGINT UNSIGNED(snowflake,不变量 §9.11 对齐 Go uint64)
--   - 公会单归属:guild_members.player_id 为主键,强制玩家只在一个公会(类比"玩家只在一个 DS")
--   - 群组多归属:chat_group_members 主键 (group_id, player_id),玩家可在多个群
--   - guild_members.role / chat_group_members.role 对齐 proto enum 数值
--   - guild_join_requests.status:1 pending / 2 approved / 3 rejected(对齐 GuildJoinStatus)
--   - 表名 chat_groups(非 groups):groups 是部分 SQL 方言保留字,避免转义
--   - 成员关系是结构化列(CLAUDE.md §5.9 关系型表不强制 proto bytes blob)


CREATE TABLE IF NOT EXISTS `guilds` (
    `guild_id`     BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 公会 ID(uint64)',
    `name`         VARCHAR(64)     NOT NULL COMMENT '公会名(唯一)',
    `leader_id`    BIGINT UNSIGNED NOT NULL COMMENT '会长 player_id',
    `member_count` INT             NOT NULL DEFAULT 1 COMMENT '成员数(含会长)',
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

-- ===== from 12-mail-tables.sql =====
-- Pandora 邮件表结构(mail 服务,2026-06-29)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (06-social-tables.sql 建 friend/chat,11-guild-tables.sql 建公会 / 群,本文件接着建邮件表)。
--
-- 设计依据:docs/design/mail.md。
-- 核心:系统 / 公会邮件拉取式(channel + watermark 游标,零写扩散),个人邮件写扩散。
-- 表清单(对齐 pandora_social 库):
--   sys_mail            系统邮件一份(全服共享,mail_id PK)
--   guild_mail          公会邮件一份(每公会一行可拉取,mail_id PK)
--   player_mail         个人收件箱(写扩散,mail_id PK)
--   player_mail_cursor  系统 / 公会邮件拉取游标(player_id PK)
--   player_mail_claim   附件领取幂等(player_id + mail_id PK)
--
-- 约定:
--   - 所有业务 ID BIGINT UNSIGNED(snowflake,不变量 §9.11 对齐 Go uint64)
--   - 系统/公会邮件由单节点生成,channel 内 mail_id 严格递增(游标比较零漏拉)
--   - 邮件正文 + 附件序列化成 proto bytes 存 payload blob(CLAUDE.md §5.8 存储侧)
--   - status:1 unread / 2 read / 3 claimed(对齐 MailStatus)


CREATE TABLE IF NOT EXISTS `sys_mail` (
    `mail_id`    BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 系统邮件 ID(uint64,channel 内递增)',
    `start_ms`   BIGINT          NOT NULL DEFAULT 0 COMMENT '生效起 ms(0 立即)',
    `end_ms`     BIGINT          NOT NULL DEFAULT 0 COMMENT '失效止 ms(0 永不过期)',
    `payload`    BLOB            NOT NULL COMMENT 'MailContentStorageRecord 序列化(标题/正文/附件)',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`mail_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 系统邮件(全服一份,登录拉取)';

CREATE TABLE IF NOT EXISTS `guild_mail` (
    `mail_id`    BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 公会邮件 ID(uint64,channel 内递增)',
    `guild_id`   BIGINT UNSIGNED NOT NULL COMMENT '所属公会',
    `start_ms`   BIGINT          NOT NULL DEFAULT 0,
    `end_ms`     BIGINT          NOT NULL DEFAULT 0,
    `payload`    BLOB            NOT NULL COMMENT 'MailContentStorageRecord 序列化',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`mail_id`),
    KEY `idx_guild` (`guild_id`, `mail_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 公会邮件(每公会一份,成员拉取)';

CREATE TABLE IF NOT EXISTS `player_mail` (
    `mail_id`    BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 个人邮件 ID(uint64)',
    `player_id`  BIGINT UNSIGNED NOT NULL COMMENT '收件人',
    `status`     TINYINT         NOT NULL DEFAULT 1 COMMENT '1 unread / 2 read / 3 claimed',
    `claimed`    TINYINT         NOT NULL DEFAULT 0 COMMENT '附件是否已领',
    `expire_ms`  BIGINT          NOT NULL DEFAULT 0 COMMENT '过期 ms(0 永不过期)',
    `payload`    BLOB            NOT NULL COMMENT 'MailContentStorageRecord 序列化',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`mail_id`),
    KEY `idx_player_status` (`player_id`, `status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 个人邮件收件箱(写扩散,离线可达)';

CREATE TABLE IF NOT EXISTS `player_mail_cursor` (
    `player_id`        BIGINT UNSIGNED NOT NULL COMMENT '玩家',
    `last_sys_mail_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '系统邮件已读到的最大 id',
    `last_guild_mail_id` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '公会邮件已读到的最大 id',
    `updated_at`       DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 系统/公会邮件拉取游标(watermark)';

CREATE TABLE IF NOT EXISTS `player_mail_claim` (
    `player_id`   BIGINT UNSIGNED NOT NULL COMMENT '领取人',
    `mail_id`     BIGINT UNSIGNED NOT NULL COMMENT '被领邮件(任意 channel)',
    `claimed_at`  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`, `mail_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 邮件附件领取幂等(player_id+mail_id 唯一)';

