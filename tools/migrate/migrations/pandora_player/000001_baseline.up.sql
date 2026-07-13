-- Pandora pandora_player baseline (golang-migrate v000001)
-- 自动从 deploy/mysql-init/*.sql 生成(去 USE 行;DSN 已指定库)。
-- 这是该库的建表基线;后续结构改动新增 000002_*.up.sql,勿改本文件。

-- ===== from 04-player-tables.sql =====
-- Pandora 玩家库 W4 ④ 表结构(2026-06-06)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建表)。
--
-- 表清单(对齐 docs/design/infra.md §2.1 pandora_player):
--   players       玩家档案(player_id PK,昵称 / 等级 / 段位 mmr / 战绩计数 / 出战英雄 / 属性点)
--   player_heroes 英雄解锁记录(uk player_id+hero_id)
--   mmr_history   MMR 变化历史 + 幂等键(uk player_id+idempotency_key,不变量 §2)
--   player_attributes 属性加点已分配点(uk player_id+attr_key)
--   attr_point_grants 属性点授予幂等表(uk player_id+idempotency_key)
--   player_equipment  出战装备预设(uk player_id+slot)
--   player_talents    天赋树已点分配(uk player_id+talent_id,re-spec 全量替换)
--   talent_point_grants 天赋点授予幂等表(uk player_id+idempotency_key)
--
-- 约定:
--   - player_id 由 login 服务用 snowflake 生成(BIGINT UNSIGNED),player 服务不生成
--   - mmr 缺省 1500(与 battle_result base_mmr 对齐),floor 0 由应用层保证
--   - UpdateMMR 幂等:idempotency_key 一般是 match_id;mmr_history uk 命中即视为已处理
--   - 默认昵称 = 配置前缀 + player_id,保证 uk_nickname 不冲突


CREATE TABLE IF NOT EXISTS `players` (
    `player_id`     BIGINT UNSIGNED  NOT NULL,
    `nickname`      VARCHAR(64)      NOT NULL COMMENT '玩家昵称,uk_nickname 唯一',
    `level`         INT              NOT NULL DEFAULT 1,
    `mmr`           INT              NOT NULL DEFAULT 1500 COMMENT '段位分,floor 0',
    `avatar`        VARCHAR(255)     NOT NULL DEFAULT '',
    `total_battles` INT              NOT NULL DEFAULT 0,
    `total_wins`    INT              NOT NULL DEFAULT 0,
    `active_hero_id`      INT UNSIGNED NOT NULL DEFAULT 0 COMMENT '选定出战英雄 hero_id(0=未选定)',
    `unspent_attr_points` INT          NOT NULL DEFAULT 0 COMMENT '未分配属性点',
    `total_talent_points` INT          NOT NULL DEFAULT 0 COMMENT '累计授予天赋点(可点 = total - SUM(player_talents.level))',
    `created_at`    DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `last_seen_at`  DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`),
    UNIQUE KEY `uk_nickname` (`nickname`),
    KEY `idx_mmr` (`mmr`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家档案表';

CREATE TABLE IF NOT EXISTS `player_heroes` (
    `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`   BIGINT UNSIGNED  NOT NULL,
    `hero_id`     INT UNSIGNED     NOT NULL COMMENT '配置表英雄 ID(uint32)',
    `source`      VARCHAR(32)      NOT NULL DEFAULT '' COMMENT 'purchase | reward | freebie',
    `unlocked_at` DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_hero` (`player_id`, `hero_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家英雄解锁记录';

CREATE TABLE IF NOT EXISTS `mmr_history` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `idempotency_key` VARCHAR(64)      NOT NULL COMMENT '幂等键,一般是 match_id',
    `delta`           INT              NOT NULL COMMENT '本次 MMR 变化(可负)',
    `reason`          VARCHAR(32)      NOT NULL DEFAULT '' COMMENT 'win | lose | draw | abandon | rollback',
    `old_mmr`         INT              NOT NULL,
    `new_mmr`         INT              NOT NULL,
    `created_at`      DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_idem` (`player_id`, `idempotency_key`),
    KEY `idx_player_created` (`player_id`, `created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家 MMR 变化历史 + 幂等键(不变量 §2)';

CREATE TABLE IF NOT EXISTS `player_attributes` (
    `id`         BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`  BIGINT UNSIGNED  NOT NULL,
    `attr_key`   VARCHAR(32)      NOT NULL COMMENT '属性键: str | agi | int | vit 等',
    `points`     INT              NOT NULL DEFAULT 0 COMMENT '该属性已分配点(>=0)',
    `updated_at` DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_attr` (`player_id`, `attr_key`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家属性加点已分配点';

CREATE TABLE IF NOT EXISTS `attr_point_grants` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `idempotency_key` VARCHAR(64)      NOT NULL COMMENT '防重复授予(如 level_up:<level> / reward:<id>)',
    `points`          INT              NOT NULL COMMENT '本次授予点数(>0)',
    `created_at`      DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_grant` (`player_id`, `idempotency_key`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 属性点授予幂等表';

CREATE TABLE IF NOT EXISTS `player_equipment` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `slot`            INT UNSIGNED     NOT NULL COMMENT '出战装备预设槽位序号',
    `item_config_id`  INT UNSIGNED     NOT NULL COMMENT '装备配置 ID(uint32)',
    `updated_at`      DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_slot` (`player_id`, `slot`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家出战装备预设(大厅态;开战前转初始 GameplayEffect)';

CREATE TABLE IF NOT EXISTS `player_talents` (
    `id`         BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`  BIGINT UNSIGNED  NOT NULL,
    `talent_id`  INT UNSIGNED     NOT NULL COMMENT '天赋配置 ID(uint32)',
    `level`      INT              NOT NULL DEFAULT 0 COMMENT '该天赋节点已点等级(>0)',
    `updated_at` DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_talent` (`player_id`, `talent_id`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家天赋树已点分配(re-spec 全量替换)';

CREATE TABLE IF NOT EXISTS `talent_point_grants` (
    `id`              BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`       BIGINT UNSIGNED  NOT NULL,
    `idempotency_key` VARCHAR(64)      NOT NULL COMMENT '防重复授予(如 level_up:<level> / reward:<id>)',
    `points`          INT              NOT NULL COMMENT '本次授予点数(>0)',
    `created_at`      DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_talent_grant` (`player_id`, `idempotency_key`),
    KEY `idx_player` (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 天赋点授予幂等表';

-- ===== from 13-reward-claim-tables.sql =====
-- Pandora 玩家领奖记录表(2026-06-30)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行,
-- 本文件在 04-player-tables.sql(建 pandora_player 库与玩家档案表)之后执行。
--
-- 表清单(对齐 docs/design/infra.md §2.1 pandora_player):
--   player_reward_claims  玩家领奖记录(player_id PK;record 为 RewardClaimStorageRecord
--                         序列化 bytes;version 乐观锁)
--
-- 设计要点:
--   - 与玩家档案同库(pandora_player)同 player_id:回档随库一起恢复、合服随 player_id
--     一起迁移,领奖状态与玩家数据天然原子(不变量 §2 / §7 幂等;回档不撕裂)
--   - record 列是上层 pkg/rewardclaim 序列化好的 RewardClaimStorageRecord 不透明 bytes
--     (CLAUDE.md §5.8 存储快照 / §5.9 blob 场景用 proto bytes),DB 不解释其内容
--   - version 乐观锁:UPDATE ... WHERE player_id=? AND version=? 防并发覆盖
--   - 永久类(签到 / 成就 / 新手 / 永久任务)与活动类(按活动实例 ID,下线整条回收)
--     都编码在同一条 record bytes 内,见 proto RewardClaimStorageRecord 注释


CREATE TABLE IF NOT EXISTS `player_reward_claims` (
    `player_id`  BIGINT UNSIGNED NOT NULL,
    `record`     LONGBLOB        NOT NULL COMMENT 'RewardClaimStorageRecord 序列化 bytes(永久+活动领取位图)',
    `version`    INT             NOT NULL DEFAULT 0 COMMENT '乐观锁版本',
    `updated_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家领奖记录(永久+活动 bitmap 快照)';

