-- 000005_battle_progress 回滚:删实时进度两张表(仅开发期使用;会丢在途发放行,慎用)。

DROP TABLE IF EXISTS `battle_progress_outbox`;
DROP TABLE IF EXISTS `battle_progress_stream`;
