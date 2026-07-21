-- 000002_experience — 玩家等级经验(实时成长,2026-07-20 realtime-progression.md)。
--
-- 纯 additive(不变量 §16/§17 不停服):加列 + 两张新表,滚动更新期间旧副本不受影响
-- (旧 SQL 不引用新列;新副本读默认值 0 = Lv 内 0 经验,懒生效)。
--
--   players.exp        级内经验列(满级恒 0)
--   exp_history        经验入账历史 + 幂等键(uk player_id+idempotency_key,不变量 §2)
--   player_push_outbox 经验推送事务出箱(与入账同事务原子提交,不变量 §4)
--
-- exp 列条件加列:deploy/mysql-init/04-player-tables.sql 的 fresh-init 已直接建出 exp 列,
-- 该库随后纳入 migrator 时无条件 ADD COLUMN 会 duplicate column 报错(审计 P1)。
-- ALGORITHM=INSTANT 显式声明在线 DDL(MySQL 8.0 加带默认值列为 INSTANT,不锁表不重建;
-- 若目标实例不支持会明确失败而不是静默退化成锁表拷贝)。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'players' AND COLUMN_NAME = 'exp') = 0,
    'ALTER TABLE `players` ADD COLUMN `exp` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT ''级内经验(实时成长;满级恒 0)'' AFTER `level`, ALGORITHM=INSTANT',
    'SELECT 1');
PREPARE add_exp_column FROM @ddl;
EXECUTE add_exp_column;
DEALLOCATE PREPARE add_exp_column;

CREATE TABLE IF NOT EXISTS `exp_history` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `idempotency_key` VARCHAR(64)      NOT NULL COMMENT '幂等键(来源单点入口清单见 realtime-progression.md §4.4)',
    `exp_delta`       BIGINT UNSIGNED  NOT NULL COMMENT '本次入账经验(>0)',
    `reason`          VARCHAR(32)      NOT NULL DEFAULT '' COMMENT 'monster_kill | quest | gm',
    `old_level`       INT              NOT NULL,
    `old_exp`         BIGINT UNSIGNED  NOT NULL,
    `new_level`       INT              NOT NULL,
    `new_exp`         BIGINT UNSIGNED  NOT NULL,
    `created_at`      DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_idem` (`player_id`, `idempotency_key`),
    KEY `idx_player_created` (`player_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家经验入账历史 + 幂等键(实时成长,不变量 §2)';

CREATE TABLE IF NOT EXISTS `player_push_outbox` (
    `id`            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`     BIGINT UNSIGNED  NOT NULL,
    `event_type`    INT UNSIGNED     NOT NULL COMMENT 'PlayerPushEventType(1=EXPERIENCE)',
    `payload`       VARBINARY(512)   NOT NULL COMMENT '对应事件 message 的 proto bytes',
    `created_at_ms` BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家推送事务出箱(经验等 player.update 域内事件,不变量 §4)';
