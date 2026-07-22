-- additive-only:应用版本回滚时保留托管表及其中数据。
-- 在途托管行是玩家实例资产的唯一持有处(player_item_instance 已扣出),
-- DROP TABLE 会直接销毁资产;旧 inventory 二进制忽略本表,保留无副作用。
SELECT 1;
