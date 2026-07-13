-- Pandora pandora_account baseline down(⚠️ 仅 dev 回滚用;生产严禁执行 —— 会清空账号表)
DROP TABLE IF EXISTS `player_roles`;
DROP TABLE IF EXISTS `account_bans`;
DROP TABLE IF EXISTS `account_devices`;
DROP TABLE IF EXISTS `accounts`;
