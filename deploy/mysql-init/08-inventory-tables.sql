-- Pandora 背包 / 经济库 W5 ③ 表结构(2026-06-18)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建表)。
--
-- 表清单(对齐 docs/design/infra.md pandora_trade):
--   player_currency   玩家货币余额(player_id PK,金币)
--   player_items      背包道具堆叠(uk player_id+item_config_id)
--   inventory_ledger  发放 / 使用 / 出售幂等流水(uk player_id+idempotency_key,不变量 §9.7)
--
-- 约定:
--   - player_id 由 login 用 snowflake 生成(BIGINT UNSIGNED),inventory 不生成
--   - item_config_id 是配置表道具 ID(uint32,§12)
--   - 货币 / 道具数量用 BIGINT(可累积大额);非负由应用层事务保证
--   - GrantItems / UseItem / SellItem 幂等:inventory_ledger uk 命中即视为已处理(不变量 §9.7)
--   - 背包是大厅态持久化;战斗内即时道具走 UE GAS,不落本库(ds-arch §0.1)

USE `pandora_trade`;

CREATE TABLE IF NOT EXISTS `player_currency` (
    `player_id`  BIGINT UNSIGNED  NOT NULL,
    `gold`       BIGINT           NOT NULL DEFAULT 0 COMMENT '金币余额(>=0,应用层事务保证)',
    `updated_at` DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家货币余额';

CREATE TABLE IF NOT EXISTS `player_items` (
    `id`             BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`      BIGINT UNSIGNED  NOT NULL,
    `item_config_id` INT UNSIGNED     NOT NULL COMMENT '配置表道具 ID(uint32)',
    `count`          BIGINT           NOT NULL DEFAULT 0 COMMENT '持有数量(>=0;0 行可保留也可清理)',
    `updated_at`     DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_item` (`player_id`, `item_config_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家背包道具堆叠';

CREATE TABLE IF NOT EXISTS `inventory_ledger` (
    `id`                  BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`           BIGINT UNSIGNED  NOT NULL,
    `idempotency_key`     VARCHAR(64)      NOT NULL COMMENT '防重复入账/扣减(如 drop:<match_id> / use:<uuid>)',
    `op`                  VARCHAR(16)      NOT NULL COMMENT 'grant | use | sell | auction_sell | auction_buy',
    `request_fingerprint` CHAR(64)         NOT NULL DEFAULT '' COMMENT '请求指纹 sha256(op+item+count+gold);同 key 不同指纹判冲突',
    `result_remaining`    BIGINT           NOT NULL DEFAULT 0 COMMENT '首次执行后剩余数量快照(use/sell 用,回放返回)',
    `result_gold`         BIGINT           NOT NULL DEFAULT 0 COMMENT '首次执行后金币快照(grant/sell 用,回放返回)',
    `detail`              VARCHAR(255)     NOT NULL DEFAULT '' COMMENT '人读摘要(审计用,非业务字段)',
    `created_at`          DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_idem` (`player_id`, `idempotency_key`),
    KEY `idx_player_created` (`player_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 背包发放/使用/出售幂等流水(不变量 §9.7;指纹防 key 复用,快照可回放)';
