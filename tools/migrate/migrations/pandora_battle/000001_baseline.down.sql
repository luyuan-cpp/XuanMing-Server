-- Pandora pandora_battle baseline down(⚠️ 仅 dev 回滚用;生产严禁执行)
DROP TABLE IF EXISTS `battle_drop_outbox`;
DROP TABLE IF EXISTS `player_update_outbox`;
DROP TABLE IF EXISTS `battle_player_stats`;
DROP TABLE IF EXISTS `battles`;
