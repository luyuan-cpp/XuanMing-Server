-- additive-only：应用版本回滚时保留计数列 / 计数表及 backfill 数据。
-- 旧 guild 二进制会忽略它们；在线 DROP COLUMN / DROP TABLE 反而会破坏新旧副本共存与回滚。
SELECT 1;
