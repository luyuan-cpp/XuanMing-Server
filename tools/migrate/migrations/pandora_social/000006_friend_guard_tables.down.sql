-- 回滚好友域守卫行表(000006)。守卫行无业务数据(仅锁载体),DROP 无数据损失;
-- 但回滚后依赖守卫行的 friend 版本会在 acquire*Guard 处失败,须先回滚 friend 二进制。
DROP TABLE IF EXISTS `friend_pair_guards`;
DROP TABLE IF EXISTS `friend_player_guards`;
