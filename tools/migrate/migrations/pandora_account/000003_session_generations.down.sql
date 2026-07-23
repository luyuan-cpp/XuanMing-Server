-- 000003 回滚:有意 no-op(与 000002 同口径)。
--
-- player_session_generations 属于当前权威表定义(fresh-init 建表自带),up 只是给
-- 存量库补齐缺口。回滚删表/删列会让「fresh 建表 + 回滚」的库比权威定义缺结构,
-- 且会丢弃登录定序权威数据(旧会话 fencing 依据),因此保留,不做任何操作。

SELECT 1;
