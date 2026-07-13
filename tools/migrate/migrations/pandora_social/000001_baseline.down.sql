-- Pandora pandora_social baseline down(⚠️ 仅 dev 回滚用;生产严禁执行)
DROP TABLE IF EXISTS `player_mail_claim`;
DROP TABLE IF EXISTS `player_mail_cursor`;
DROP TABLE IF EXISTS `player_mail`;
DROP TABLE IF EXISTS `guild_mail`;
DROP TABLE IF EXISTS `sys_mail`;
DROP TABLE IF EXISTS `chat_group_members`;
DROP TABLE IF EXISTS `chat_groups`;
DROP TABLE IF EXISTS `guild_join_requests`;
DROP TABLE IF EXISTS `guild_members`;
DROP TABLE IF EXISTS `guilds`;
DROP TABLE IF EXISTS `blocks`;
DROP TABLE IF EXISTS `friend_requests`;
DROP TABLE IF EXISTS `friendships`;
