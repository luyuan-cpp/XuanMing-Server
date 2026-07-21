-- 000002_experience 回滚:删经验列与两张新表(仅开发期使用;生产回滚会丢经验数据,慎用)。

DROP TABLE IF EXISTS `player_push_outbox`;
DROP TABLE IF EXISTS `exp_history`;
ALTER TABLE `players` DROP COLUMN `exp`;
