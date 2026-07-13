-- 拍卖撮合持久意图 expand 迁移(MySQL 8.4)。结构变更均为 additive，并可与旧二进制共存：
--   * release_pending 默认 1，旧二进制省略该列也会天然登记；新 Claim 显式写 0；
--   * settlement_status 默认 COMPLETED(1)，旧二进制省略该列时保持原先「已结算流水」语义；
--   * 新二进制显式写 PENDING(0)，并以幂等账本调用完成补偿。
-- docker-init 新库已包含这些结构，因此每项都先查 information_schema，允许幂等执行。

SET @pandora_release_pending_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_orders' AND column_name = 'release_pending'
);
SET @pandora_sql := IF(
    @pandora_release_pending_exists = 0,
    'ALTER TABLE `auction_orders` ADD COLUMN `release_pending` TINYINT NOT NULL DEFAULT 1 COMMENT ''终态 escrow 待按 order_id 幂等释放;默认 1 让旧二进制天然登记,新 Claim 显式写 0'' AFTER `status`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- 旧表 status 注释只列客户端状态；新状态机还使用内部 PENDING=0。仅改元数据，
-- 让 docker-init 新库与版本迁移旧库的 SHOW CREATE 保持一致。
ALTER TABLE `auction_orders`
    MODIFY COLUMN `status` TINYINT NOT NULL DEFAULT 1 COMMENT '内部状态:0 PENDING;客户端状态:1 OPEN 2 PARTIAL 3 FILLED 4 CANCELED 5 EXPIRED',
    ALGORITHM=INSTANT;

CREATE TABLE IF NOT EXISTS `auction_owner_guards` (
    `owner_id` BIGINT UNSIGNED NOT NULL,
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`owner_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 owner 维度跨分片幂等串行 guard';

CREATE TABLE IF NOT EXISTS `auction_idempotency_keys` (
    `owner_id` BIGINT UNSIGNED NOT NULL,
    `idempotency_key` VARCHAR(64) NOT NULL,
    `order_id` BIGINT UNSIGNED NOT NULL,
    `market_id` INT UNSIGNED NOT NULL,
    `side` TINYINT NOT NULL,
    `item_config_id` INT UNSIGNED NOT NULL,
    `quantity` BIGINT NOT NULL,
    `price` BIGINT NOT NULL,
    `created_at_ms` BIGINT NOT NULL,
    PRIMARY KEY (`owner_id`, `idempotency_key`),
    UNIQUE KEY `uk_idempotency_order` (`order_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 owner+idempotency_key 跨 market 分片 canonical 映射';

CREATE TABLE IF NOT EXISTS `auction_shard_topology` (
    `singleton_id` TINYINT UNSIGNED NOT NULL COMMENT '固定为 1',
    `topology_generation` VARCHAR(64) NOT NULL,
    `topology_hash` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `shard_count` INT UNSIGNED NOT NULL,
    `shard_index` INT UNSIGNED NOT NULL,
    `shard_identity_hash` CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    `initialized_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`singleton_id`),
    CONSTRAINT `chk_auction_topology_singleton` CHECK (`singleton_id` = 1),
    CONSTRAINT `chk_auction_topology_index` CHECK (`shard_count` > 0 AND `shard_index` < `shard_count`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='拍卖 id%N 有序物理分片拓扑 marker；启动 exact-match，不允许静默 rehash';

-- 若曾执行过早期 default=0 草案，幂等修正默认值。MySQL 8.4 修改默认值是 INSTANT DDL。
ALTER TABLE `auction_orders`
    ALTER COLUMN `release_pending` SET DEFAULT 1,
    ALGORITHM=INSTANT;

SET @pandora_match_pending_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_orders' AND column_name = 'match_pending'
);
SET @pandora_sql := IF(
    @pandora_match_pending_exists = 0,
    'ALTER TABLE `auction_orders` ADD COLUMN `match_pending` TINYINT NOT NULL DEFAULT 0 COMMENT ''incoming 部分成交后的主动撮合续跑标记;默认 0 兼容旧二进制'' AFTER `release_pending`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_escrow_verified_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_orders' AND column_name = 'escrow_verified'
);
SET @pandora_sql := IF(
    @pandora_escrow_verified_exists = 0,
    'ALTER TABLE `auction_orders` ADD COLUMN `escrow_verified` TINYINT NOT NULL DEFAULT 0 COMMENT ''1=已验证剩余 escrow 足够;旧二进制/历史单默认 0，验证后才准入撮合'' AFTER `match_pending`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_reconcile_retry_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_orders' AND column_name = 'reconcile_next_attempt_at_ms'
);
SET @pandora_sql := IF(
    @pandora_reconcile_retry_exists = 0,
    'ALTER TABLE `auction_orders` ADD COLUMN `reconcile_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT ''PENDING/撮合续跑失败后的下次就绪时间;0=立即'' AFTER `escrow_verified`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_release_retry_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_orders' AND column_name = 'release_next_attempt_at_ms'
);
SET @pandora_sql := IF(
    @pandora_release_retry_exists = 0,
    'ALTER TABLE `auction_orders` ADD COLUMN `release_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT ''释放失败后的下次就绪时间;0=立即'' AFTER `reconcile_next_attempt_at_ms`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- 不在版本化 SQL 内做无界全表 UPDATE。正常旧库首次 ADD COLUMN 时 DEFAULT 1 会覆盖现存行；
-- 本迁移尚未发布，禁止把任何早期 default=0 草案当作受支持基线。发布后修正只能新增版本。

SET @pandora_settlement_status_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_matches' AND column_name = 'settlement_status'
);
SET @pandora_sql := IF(
    @pandora_settlement_status_exists = 0,
    'ALTER TABLE `auction_matches` ADD COLUMN `settlement_status` TINYINT NOT NULL DEFAULT 1 COMMENT ''0=PENDING 待结算 1=COMPLETED;默认 1 兼容旧二进制 insert'' AFTER `matched_at_ms`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_settlement_retry_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_matches' AND column_name = 'settlement_next_attempt_at_ms'
);
SET @pandora_sql := IF(
    @pandora_settlement_retry_exists = 0,
    'ALTER TABLE `auction_matches` ADD COLUMN `settlement_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT ''结算失败后的下次就绪时间;0=立即'' AFTER `settlement_status`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_event_pending_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_matches' AND column_name = 'event_pending'
);
SET @pandora_sql := IF(
    @pandora_event_pending_exists = 0,
    'ALTER TABLE `auction_matches` ADD COLUMN `event_pending` TINYINT NOT NULL DEFAULT 1 COMMENT ''成交事件待按 match_id 至少一次投递;旧二进制省略列时默认登记,新 Reserve 显式写 0'' AFTER `settlement_next_attempt_at_ms`, ALGORITHM=INSTANT',
    'SELECT 1'
);
-- 直接以兼容默认 1 原子新增：迁移期间仍运行的旧 RecordMatch 省略该列时不会漏登记。
-- 历史 COMPLETED 成交也会进入有界事件 worker 重放；消费者必须按 match_id 幂等。
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_event_retry_exists := (
    SELECT COUNT(*) FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'auction_matches' AND column_name = 'event_next_attempt_at_ms'
);
SET @pandora_sql := IF(
    @pandora_event_retry_exists = 0,
    'ALTER TABLE `auction_matches` ADD COLUMN `event_next_attempt_at_ms` BIGINT NOT NULL DEFAULT 0 COMMENT ''事件投递失败后的下次就绪时间;0=立即'' AFTER `event_pending`, ALGORITHM=INSTANT',
    'SELECT 1'
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

-- 二级索引按物理表合并为一次在线 ALTER，避免旧大表为每个索引重复扫描。CONCAT_WS 会
-- 忽略已存在索引对应的 NULL，因此同一 SQL 也可安全跑在 docker-init 已建好最终结构的新库。
SET @pandora_order_index_clauses := CONCAT_WS(', ',
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_owner_order')=0,
       'ADD INDEX `idx_owner_order` (`owner_id`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_unverified_reconcile')=0,
       'ADD INDEX `idx_unverified_reconcile` (`escrow_verified`, `reconcile_next_attempt_at_ms`, `status`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_pending_reconcile')=0,
       'ADD INDEX `idx_pending_reconcile` (`status`, `reconcile_next_attempt_at_ms`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_match_pending_ready')=0,
       'ADD INDEX `idx_match_pending_ready` (`match_pending`, `reconcile_next_attempt_at_ms`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_verified_instrument_match')=0,
       'ADD INDEX `idx_verified_instrument_match` (`market_id`, `item_config_id`, `side`, `escrow_verified`, `price`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_verified_instrument_buy')=0,
       'ADD INDEX `idx_verified_instrument_buy` (`market_id`, `item_config_id`, `side`, `escrow_verified`, `price` DESC, `order_id` ASC)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_release_ready')=0,
       'ADD INDEX `idx_release_ready` (`release_pending`, `release_next_attempt_at_ms`, `status`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_instrument_match')=0,
       'ADD INDEX `idx_instrument_match` (`market_id`, `item_config_id`, `side`, `status`, `price`, `order_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_orders' AND index_name='idx_release_terminal')=0,
       'ADD INDEX `idx_release_terminal` (`release_pending`, `status`, `order_id`)', NULL)
);
SET @pandora_sql := IF(
    @pandora_order_index_clauses = '',
    'SELECT 1',
    CONCAT('ALTER TABLE `auction_orders` ', @pandora_order_index_clauses, ', ALGORITHM=INPLACE, LOCK=NONE')
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;

SET @pandora_match_index_clauses := CONCAT_WS(', ',
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_matches' AND index_name='idx_match_event_ready')=0,
       'ADD INDEX `idx_match_event_ready` (`event_pending`, `event_next_attempt_at_ms`, `matched_at_ms`, `match_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_matches' AND index_name='idx_settlement_pending')=0,
       'ADD INDEX `idx_settlement_pending` (`settlement_status`, `settlement_next_attempt_at_ms`, `matched_at_ms`, `match_id`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_matches' AND index_name='idx_sell_settlement')=0,
       'ADD INDEX `idx_sell_settlement` (`sell_order_id`, `settlement_status`)', NULL),
    IF((SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='auction_matches' AND index_name='idx_buy_settlement')=0,
       'ADD INDEX `idx_buy_settlement` (`buy_order_id`, `settlement_status`)', NULL)
);
SET @pandora_sql := IF(
    @pandora_match_index_clauses = '',
    'SELECT 1',
    CONCAT('ALTER TABLE `auction_matches` ', @pandora_match_index_clauses, ', ALGORITHM=INPLACE, LOCK=NONE')
);
PREPARE pandora_stmt FROM @pandora_sql;
EXECUTE pandora_stmt;
DEALLOCATE PREPARE pandora_stmt;
