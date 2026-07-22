-- 000008 回滚:有意 no-op。
--
-- stopped_at_ms 属当前权威表定义(fresh-init 自带);纯 additive 列对旧版本二进制
-- 无害,回滚删列反而丢停流审计事实。保留列,不做任何操作。

SELECT 1;
