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
--   player_mail_archive 过期未领附件归档(清理 worker 移入,守住"不静默丢失"不变量)
--
-- 约定:
--   - 所有业务 ID BIGINT UNSIGNED(snowflake,不变量 §9.11 对齐 Go uint64)
--   - 系统/公会邮件由单节点生成,channel 内 mail_id 严格递增(游标比较零漏拉)
--   - 邮件正文 + 附件序列化成 proto bytes 存 payload blob(CLAUDE.md §5.8 存储侧)
--   - status:1 unread / 2 read / 3 claimed(对齐 MailStatus)
--   - 增长有界(2026-07-21):所有邮件生命有限(end_ms/expire_ms 为 0 时服务端补默认 TTL),
--     mail 服务 sweep worker 周期批量清理过期行;idx_expire/idx_end/idx_mail 供清理走索引

USE `pandora_social`;

CREATE TABLE IF NOT EXISTS `sys_mail` (
    `mail_id`    BIGINT UNSIGNED NOT NULL COMMENT 'snowflake 系统邮件 ID(uint64,channel 内递增)',
    `start_ms`   BIGINT          NOT NULL DEFAULT 0 COMMENT '生效起 ms(0 立即)',
    `end_ms`     BIGINT          NOT NULL DEFAULT 0 COMMENT '失效止 ms(0 永不过期)',
    `payload`    BLOB            NOT NULL COMMENT 'MailContentStorageRecord 序列化(标题/正文/附件)',
    `created_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`mail_id`),
    KEY `idx_end` (`end_ms`)
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
    KEY `idx_guild` (`guild_id`, `mail_id`),
    KEY `idx_end` (`end_ms`)
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
    KEY `idx_player_status` (`player_id`, `status`),
    KEY `idx_expire` (`expire_ms`)
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
    PRIMARY KEY (`player_id`, `mail_id`),
    KEY `idx_mail` (`mail_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 邮件附件领取幂等(player_id+mail_id 唯一;sweep 按雪花 mail_id cutoff 范围清理)';

-- 过期未领附件归档:sweep 清理 player_mail 时,带未领附件的过期邮件先移入本表再删
-- (mail.md §7.4 "过期附件清理后必有补偿或归档,不静默丢失";已领/无附件的直接删)。
-- 本表自身按 archived_at + archive_retention_days 二次清理,生命同样有限。
CREATE TABLE IF NOT EXISTS `player_mail_archive` (
    `mail_id`     BIGINT UNSIGNED NOT NULL COMMENT '原 player_mail.mail_id',
    `player_id`   BIGINT UNSIGNED NOT NULL COMMENT '收件人',
    `status`      TINYINT         NOT NULL COMMENT '归档时状态(1 unread / 2 read)',
    `expire_ms`   BIGINT          NOT NULL COMMENT '原过期时间 ms',
    `created_ms`  BIGINT          NOT NULL COMMENT '原 created_at 的 Unix ms',
    `payload`     BLOB            NOT NULL COMMENT 'MailContentStorageRecord 序列化',
    `archived_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`mail_id`),
    KEY `idx_player` (`player_id`),
    KEY `idx_archived` (`archived_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 过期未领附件归档(客诉补偿凭据,超保留期后清除)';
