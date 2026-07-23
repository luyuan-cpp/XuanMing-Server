-- 000006 回滚:有意 no-op(additive-only,同 000004/000005 约定)。
--
-- 守卫行表属于当前 fresh-init 表定义(deploy/mysql-init/06-social-tables.sql 建表
-- 自带),up 只是给存量升级库补齐缺口。在线 DROP TABLE 会让仍在跑的 friend 旧
-- 副本 acquire*Guard 即炸(好友操作全量内部错误),且使「fresh 建表 + 回滚」与
-- 表定义不一致,因此不做任何操作。

SELECT 1;
