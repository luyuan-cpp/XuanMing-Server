-- additive-only:应用版本回滚时保留 claimed / intent_payload 列及数据。
-- 旧 mail 二进制不引用新列(默认值兼容);在线 DROP COLUMN 反而破坏新旧副本共存,
-- 且意图行(claimed=0)是 DS 三段式领取的进行中状态,删列会丢失互斥与重放依据。
SELECT 1;
