-- bag phase 2 DS 三段式邮件领取:player_mail_claim 加 claimed / intent_payload 列
-- (2026-07-22,bag-domain.md §7 / mail.proto GetClaimableAttachments 注释)。
--
-- 纯 additive + 默认值兼容:旧 mail 二进制 INSERT 不写新列 → claimed 默认 1,
-- 与历史"行存在即已领"语义逐行等价;旧二进制 SELECT 1 判已领会把意图行(claimed=0)
-- 也视为已领 → 旧直连领取被挡(fail-closed,不双发)。发布顺序:先迁移,再滚动发布
-- 新 mail 版本,DS 领取开关(UE 侧)必须最后开。
-- fresh 库由 mysql-init/12-mail-tables.sql 建最终结构;条件加列幂等
-- (MySQL 8 无 ADD COLUMN IF NOT EXISTS,用 information_schema 守卫)。

SET @pandora_col_exists := (
    SELECT COUNT(*)
    FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'player_mail_claim'
      AND column_name = 'claimed'
);
SET @pandora_sql := IF(
    @pandora_col_exists = 0,
    'ALTER TABLE `player_mail_claim` ADD COLUMN `claimed` TINYINT NOT NULL DEFAULT 1 COMMENT ''1=终态已领 0=DS 领取意图(进行中)'', ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_col_exists := (
    SELECT COUNT(*)
    FROM information_schema.columns
    WHERE table_schema = DATABASE()
      AND table_name = 'player_mail_claim'
      AND column_name = 'intent_payload'
);
SET @pandora_sql := IF(
    @pandora_col_exists = 0,
    'ALTER TABLE `player_mail_claim` ADD COLUMN `intent_payload` BLOB NULL COMMENT ''DS 领取意图(pb MailClaimIntentStorageRecord;直连链为 NULL)''',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;
