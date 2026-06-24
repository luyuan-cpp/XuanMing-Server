-- Pandora 全服拍卖行 / 撮合 表结构(2026-06-19)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建表)。
--
-- 设计依据:docs/design/decision-revisit-auction-engine.md
--   - MySQL 为撮合权威库;按 market_id 分片(pkg/mysqlx ShardSet,shard = market_id % N);
--     W1 单库可跑(Shards 为空 = 单库)。撮合是「每个 market 单写者」的交易所模型,
--     不跨分片事务,所以用 MySQL 分库即可,不需要 TiDB。
--   - Redis ZSET 订单簿做活跃撮合索引(pandora:auction:book:{market_id}),MySQL 为权威/审计。
--
-- 表清单:
--   auction_orders   挂单 / 出价(uk owner_id+idempotency_key 防重复挂单,不变量 §9.7)
--   auction_matches  成交流水(uk match_id 防重复结算,不变量 §9.2 / §9.7)
--
-- 约定:
--   - order_id / match_id / owner_id 是雪花 uint64(BIGINT UNSIGNED,§11)
--   - market_id / item_config_id 是配置 ID(uint32,§12)
--   - side 1=SELL 2=BUY;status 见 pandora.auction.v1.AuctionOrderStatus(int)
--   - price / quantity 用 BIGINT(可累积大额);非负由应用层撮合保证

USE `pandora_auction`;

CREATE TABLE IF NOT EXISTS `auction_orders` (
    `order_id`        BIGINT UNSIGNED NOT NULL COMMENT '雪花订单 ID',
    `market_id`       INT UNSIGNED    NOT NULL COMMENT '撮合市场(道具品类),分片维度',
    `owner_id`        BIGINT UNSIGNED NOT NULL COMMENT '挂单 / 出价玩家',
    `side`            TINYINT         NOT NULL COMMENT '1=SELL 2=BUY',
    `item_config_id`  INT UNSIGNED    NOT NULL COMMENT '配置表道具 ID(uint32)',
    `quantity`        BIGINT          NOT NULL COMMENT '挂单总量(>0)',
    `filled_quantity` BIGINT          NOT NULL DEFAULT 0 COMMENT '已成交量(<=quantity)',
    `price`           BIGINT          NOT NULL COMMENT '单价(金币/个,>0)',
    `status`          TINYINT         NOT NULL DEFAULT 1 COMMENT 'AuctionOrderStatus:1 OPEN 2 PARTIAL 3 FILLED 4 CANCELED 5 EXPIRED',
    `idempotency_key` VARCHAR(64)     NOT NULL COMMENT '客户端生成,防重复挂单(不变量 §9.7)',
    `created_at_ms`   BIGINT          NOT NULL COMMENT '挂单时间(毫秒)',
    `updated_at_ms`   BIGINT          NOT NULL COMMENT '最后更新时间(毫秒)',
    PRIMARY KEY (`order_id`),
    UNIQUE KEY `uk_owner_idem` (`owner_id`, `idempotency_key`),
    KEY `idx_market_side_status` (`market_id`, `side`, `status`),
    KEY `idx_owner_status` (`owner_id`, `status`),
    KEY `idx_status_created` (`status`, `created_at_ms`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 拍卖行挂单/出价(撮合权威;uk owner+idem 防重复挂单)';

CREATE TABLE IF NOT EXISTS `auction_matches` (
    `match_id`       BIGINT UNSIGNED NOT NULL COMMENT '雪花成交 ID(结算幂等键,不变量 §9.2)',
    `market_id`      INT UNSIGNED    NOT NULL,
    `sell_order_id`  BIGINT UNSIGNED NOT NULL,
    `buy_order_id`   BIGINT UNSIGNED NOT NULL,
    `seller_id`      BIGINT UNSIGNED NOT NULL,
    `buyer_id`       BIGINT UNSIGNED NOT NULL,
    `item_config_id` INT UNSIGNED    NOT NULL,
    `quantity`       BIGINT          NOT NULL COMMENT '成交量(>0)',
    `price`          BIGINT          NOT NULL COMMENT '成交单价(被动挂单价)',
    `matched_at_ms`  BIGINT          NOT NULL COMMENT '成交时间(毫秒)',
    PRIMARY KEY (`match_id`),
    KEY `idx_market_time` (`market_id`, `matched_at_ms`),
    KEY `idx_sell_order` (`sell_order_id`),
    KEY `idx_buy_order` (`buy_order_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 拍卖行成交流水(PK match_id 防重复结算,不变量 §9.2/§9.7)';
