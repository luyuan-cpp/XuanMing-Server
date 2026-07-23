-- 会话代际表存量补齐(R7 收口,2026-07-23)。
--
-- fresh 库由 deploy/mysql-init/02-account-tables.sql 建表自带;已建 volume 的存量库
-- 走本版本条件补齐(幂等):
--   ① 表不存在(000001/000002 时代的库)→ 整表创建;
--   ② 表存在但缺 generation 列(R7 首版 init SQL 建的 dev 库)→ 条件加列。
-- generation 是并发 Login 的定序权威(每次登录事务内 +1),SetRole 同事务 FOR UPDATE
-- 复核 sess_jti 挡被顶旧会话;强制复核由 login.session_generation_enforce 分阶段激活。

CREATE TABLE IF NOT EXISTS `player_session_generations` (
    `player_id`  BIGINT UNSIGNED NOT NULL,
    `sess_jti`   VARCHAR(64)     NOT NULL COMMENT '当前会话 JWT 的 jti(uuid v4)',
    `generation` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '登录单调代际(每次 Login upsert +1,并发登录定序权威)',
    `updated_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家会话代际(登录定序 + 业务写事务 fencing;每玩家 1 行,量级被玩家数有界)';

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'player_session_generations' AND COLUMN_NAME = 'generation') = 0,
    'ALTER TABLE `player_session_generations` ADD COLUMN `generation` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT ''登录单调代际(每次 Login upsert +1,并发登录定序权威)'' AFTER `sess_jti`',
    'SELECT 1');
PREPARE stmt_add_generation FROM @ddl;
EXECUTE stmt_add_generation;
DEALLOCATE PREPARE stmt_add_generation;
