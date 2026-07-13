-- Pandora 全服拍卖行 / 撮合 表结构(2026-06-19)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建表)。
--
-- 设计依据:docs/design/decision-revisit-auction-engine.md
--   - MySQL 为撮合权威库;按 market_id 分片(pkg/mysqlx ShardSet,shard = market_id % N);
--     W1 单库可跑(Shards 为空 = 单库)。撮合是「每个 market 单写者」的交易所模型,
--     不跨分片事务,所以用 MySQL 分库即可,不需要 TiDB。
--   - MySQL 按 market_id+item_config_id 精确选择撮合候选；Redis ZSET 仅作旧版本兼容缓存。
--
-- 表清单:
--   auction_orders   挂单 / 出价(uk owner_id+idempotency_key 防重复挂单,不变量 §9.7)
--   auction_matches  成交流水(uk match_id 防重复结算,不变量 §9.2 / §9.7)
--   auction_owner_guards / auction_idempotency_keys  owner 维度跨 market 全局幂等协调
--   auction_shard_topology  有序物理分片拓扑启动门禁
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
    `status`          TINYINT         NOT NULL DEFAULT 1 COMMENT '内部状态:0 PENDING;客户端状态:1 OPEN 2 PARTIAL 3 FILLED 4 CANCELED 5 EXPIRED',
    `release_pending` TINYINT         NOT NULL DEFAULT 1 COMMENT '终态 escrow 待按 order_id 幂等释放;默认 1 让旧二进制天然登记,新 Claim 显式写 0',
    `match_pending`   TINYINT         NOT NULL DEFAULT 0 COMMENT 'incoming 部分成交后的主动撮合续跑标记;默认 0 兼容旧二进制',
    `escrow_verified` TINYINT         NOT NULL DEFAULT 0 COMMENT '1=已验证剩余 escrow 足够;旧二进制/历史单默认 0，验证后才准入撮合',
    `reconcile_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT 'PENDING/撮合续跑失败后的下次就绪时间;0=立即',
    `release_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT '释放失败后的下次就绪时间;0=立即',
    `idempotency_key` VARCHAR(64)     NOT NULL COMMENT '客户端生成,防重复挂单(不变量 §9.7)',
    `created_at_ms`   BIGINT          NOT NULL COMMENT '挂单时间(毫秒)',
    `updated_at_ms`   BIGINT          NOT NULL COMMENT '最后更新时间(毫秒)',
    PRIMARY KEY (`order_id`),
    UNIQUE KEY `uk_owner_idem` (`owner_id`, `idempotency_key`),
    KEY `idx_instrument_match` (`market_id`, `item_config_id`, `side`, `status`, `price`, `order_id`),
    KEY `idx_verified_instrument_match` (`market_id`, `item_config_id`, `side`, `escrow_verified`, `price`, `order_id`),
    KEY `idx_verified_instrument_buy` (`market_id`, `item_config_id`, `side`, `escrow_verified`, `price` DESC, `order_id` ASC),
    KEY `idx_release_terminal` (`release_pending`, `status`, `order_id`),
    KEY `idx_release_ready` (`release_pending`, `release_next_attempt_at_ms`, `status`, `order_id`),
    KEY `idx_pending_reconcile` (`status`, `reconcile_next_attempt_at_ms`, `order_id`),
    KEY `idx_match_pending_ready` (`match_pending`, `reconcile_next_attempt_at_ms`, `order_id`),
    KEY `idx_unverified_reconcile` (`escrow_verified`, `reconcile_next_attempt_at_ms`, `status`, `order_id`),
    KEY `idx_market_side_status` (`market_id`, `side`, `status`),
    KEY `idx_owner_status` (`owner_id`, `status`),
    KEY `idx_owner_order` (`owner_id`, `order_id`),
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
    `settlement_status` TINYINT      NOT NULL DEFAULT 1 COMMENT '0=PENDING 待结算 1=COMPLETED;默认 1 兼容旧二进制 insert',
    `settlement_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT '结算失败后的下次就绪时间;0=立即',
    `event_pending`   TINYINT         NOT NULL DEFAULT 1 COMMENT '成交事件待按 match_id 至少一次投递;旧二进制省略列时默认登记,新 Reserve 显式写 0',
    `event_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT '事件投递失败后的下次就绪时间;0=立即',
    PRIMARY KEY (`match_id`),
    KEY `idx_settlement_pending` (`settlement_status`, `settlement_next_attempt_at_ms`, `matched_at_ms`, `match_id`),
    KEY `idx_match_event_ready` (`event_pending`, `event_next_attempt_at_ms`, `matched_at_ms`, `match_id`),
    KEY `idx_market_time` (`market_id`, `matched_at_ms`),
    KEY `idx_sell_order` (`sell_order_id`),
    KEY `idx_buy_order` (`buy_order_id`),
    KEY `idx_sell_settlement` (`sell_order_id`, `settlement_status`),
    KEY `idx_buy_settlement` (`buy_order_id`, `settlement_status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 拍卖行成交流水(PK match_id 防重复结算,不变量 §9.2/§9.7)';

-- owner 维度的幂等协调表放在 owner_id 对应的确定性分片。新二进制先锁 guard 行，再广播
-- 一次兼容历史订单并登记 canonical 请求，保证同一 owner+key 跨 market 分片仍只能映射一单。
CREATE TABLE IF NOT EXISTS `auction_owner_guards` (
    `owner_id`   BIGINT UNSIGNED NOT NULL,
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`owner_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 owner 维度跨分片幂等串行 guard';

CREATE TABLE IF NOT EXISTS `auction_idempotency_keys` (
    `owner_id`        BIGINT UNSIGNED NOT NULL,
    `idempotency_key` VARCHAR(64)     NOT NULL,
    `order_id`        BIGINT UNSIGNED NOT NULL,
    `market_id`       INT UNSIGNED    NOT NULL,
    `side`            TINYINT         NOT NULL,
    `item_config_id`  INT UNSIGNED    NOT NULL,
    `quantity`        BIGINT          NOT NULL,
    `price`           BIGINT          NOT NULL,
    `created_at_ms`   BIGINT          NOT NULL,
    PRIMARY KEY (`owner_id`, `idempotency_key`),
    UNIQUE KEY `uk_idempotency_order` (`order_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 owner+idempotency_key 跨 market 分片 canonical 映射';

CREATE TABLE IF NOT EXISTS `auction_shard_topology` (
    `singleton_id`          TINYINT UNSIGNED NOT NULL COMMENT '固定为 1',
    `topology_generation`   VARCHAR(64) NOT NULL,
    `topology_hash`         CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `shard_count`           INT UNSIGNED NOT NULL,
    `shard_index`           INT UNSIGNED NOT NULL,
    `shard_identity_hash`   CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `initialized_at`        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`singleton_id`),
    CONSTRAINT `chk_auction_topology_singleton` CHECK (`singleton_id` = 1),
    CONSTRAINT `chk_auction_topology_index` CHECK (`shard_count` > 0 AND `shard_index` < `shard_count`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 id%N 有序物理分片拓扑 marker；启动 exact-match，不允许静默 rehash';
