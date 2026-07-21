-- additive-only:应用版本回滚时保留清理索引与归档表及其中数据。
-- 旧 mail 二进制会忽略它们;在线 DROP INDEX / DROP TABLE 反而破坏新旧副本共存与回滚,
-- 且归档行是"过期未领附件"的补偿凭据,不能随版本回滚丢失。
SELECT 1;
