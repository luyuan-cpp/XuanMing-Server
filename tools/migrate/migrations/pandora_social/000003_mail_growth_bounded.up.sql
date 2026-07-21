-- 邮件增长有界 expand 迁移(docs/design/mail.md §2.4,2026-07-21)。
--
-- 全部 additive:sweep 清理索引 ×4 + 过期未领附件归档表。旧 mail 二进制不引用
-- 新索引 / 新表,先完成本迁移再滚动发布带 sweep 的 mail 版本即可,混跑期无兼容问题。
-- docker-init / TiDB fresh schema(12-mail-tables.sql)已含最终结构,所有 DDL 都允许
-- 在该形态下幂等执行(MySQL 8 的 ADD INDEX 无 IF NOT EXISTS,用 information_schema 守卫)。

-- player_mail.idx_expire:sweep 按 expire_ms 捞过期个人邮件
SET @pandora_idx_exists := (
    SELECT COUNT(*)
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'player_mail'
      AND index_name = 'idx_expire'
);
SET @pandora_sql := IF(
    @pandora_idx_exists = 0,
    'ALTER TABLE `player_mail` ADD INDEX `idx_expire` (`expire_ms`)',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- sys_mail.idx_end:sweep 按 end_ms 删失效系统邮件
SET @pandora_idx_exists := (
    SELECT COUNT(*)
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'sys_mail'
      AND index_name = 'idx_end'
);
SET @pandora_sql := IF(
    @pandora_idx_exists = 0,
    'ALTER TABLE `sys_mail` ADD INDEX `idx_end` (`end_ms`)',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- guild_mail.idx_end:sweep 按 end_ms 删失效公会邮件
SET @pandora_idx_exists := (
    SELECT COUNT(*)
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'guild_mail'
      AND index_name = 'idx_end'
);
SET @pandora_sql := IF(
    @pandora_idx_exists = 0,
    'ALTER TABLE `guild_mail` ADD INDEX `idx_end` (`end_ms`)',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- player_mail_claim.idx_mail:sweep 按雪花 mail_id cutoff 范围删领取记录
SET @pandora_idx_exists := (
    SELECT COUNT(*)
    FROM information_schema.statistics
    WHERE table_schema = DATABASE()
      AND table_name = 'player_mail_claim'
      AND index_name = 'idx_mail'
);
SET @pandora_sql := IF(
    @pandora_idx_exists = 0,
    'ALTER TABLE `player_mail_claim` ADD INDEX `idx_mail` (`mail_id`)',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- 过期未领附件归档:sweep 清理 player_mail 时带未领附件的过期邮件先移入本表再删
-- (mail.md §7.4 "不静默丢失");本表按 archived_at + archive_retention_days 二次清理。
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
