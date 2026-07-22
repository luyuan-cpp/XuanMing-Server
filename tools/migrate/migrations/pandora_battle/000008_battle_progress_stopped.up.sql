-- 000008_battle_progress_stopped — 实时进度停流标记(2026-07-22 审计 P1)。
--
-- 未知事实类型停流(ErrInvalidState)此前只是本批拒收,没有持久化标记:违纪 DS 继续
-- 上报只含已知事实的批会被重新接受(等效重新开流),违反"整场停流、剩余实时奖励
-- 永久丢失"的已拍板契约。加 stopped_at_ms(>0 = 本场实时通道已停流,后续
-- ReportProgress 一律拒),与 settled_at_ms 同表同语义风格。
-- 纯 additive(§9.16/17):旧副本不读新列,新副本读默认 0 = 未停流。
-- 条件加列(幂等):fresh-init(05-battle-outbox.sql)建表已含。ALGORITHM=INSTANT。

SET @ddl := IF(
    (SELECT COUNT(*) FROM information_schema.COLUMNS
     WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'battle_progress_stream' AND COLUMN_NAME = 'stopped_at_ms') = 0,
    'ALTER TABLE `battle_progress_stream` ADD COLUMN `stopped_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT ''>0 = 实时通道已停流(未知事实/违纪混版),后续进度一律拒'' AFTER `settled_at_ms`, ALGORITHM=INSTANT',
    'SELECT 1');
PREPARE add_stopped_col FROM @ddl;
EXECUTE add_stopped_col;
DEALLOCATE PREPARE add_stopped_col;
