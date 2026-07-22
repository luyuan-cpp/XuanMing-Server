-- Pandora 账号库 W3 ② 表结构(2026-06-05)
--
-- 装载方式:容器 entrypoint 自动扫 /docker-entrypoint-initdb.d/*.sql 顺序执行
-- (01-create-databases.sql 先建库 + grant,本文件接着建表)。
--
-- 表清单:
--   accounts         账号身份(account → player_id + 密码哈希 + 状态)
--   account_devices  设备绑定(同一账号允许多设备,记录最近登录)
--   account_bans     封禁记录(账号 / 设备 维度,可永久或定时)
--
-- 约定:
--   - player_id 由 login 服务用 snowflake 生成(BIGINT UNSIGNED)
--   - password_hash 是 bcrypt(client_digest) 的结果(60 字节字符串)
--   - 所有 *_at 都用 DATETIME(秒级,UTC 由应用层保证)

USE `pandora_account`;

CREATE TABLE IF NOT EXISTS `accounts` (
    `player_id`     BIGINT UNSIGNED  NOT NULL,
    `account`       VARCHAR(64)      NOT NULL,
    `password_hash` VARCHAR(80)      NOT NULL COMMENT 'bcrypt(client_digest),含 cost 前缀,固定 60 字节',
    `status`        TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0=normal,1=banned,2=disabled',
    `created_at`    DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`    DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`),
    UNIQUE KEY `uk_account` (`account`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 账号身份表';

CREATE TABLE IF NOT EXISTS `account_devices` (
    `id`            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`     BIGINT UNSIGNED  NOT NULL,
    `device_id`     VARCHAR(128)     NOT NULL,
    `last_login_at` DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `last_login_ip` VARCHAR(45)      NOT NULL DEFAULT '' COMMENT 'IPv4/IPv6 字符串',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_player_device` (`player_id`, `device_id`),
    KEY `idx_device` (`device_id`),
    KEY `idx_last_login` (`last_login_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 账号设备绑定与最近登录(不活跃行保留 90 天由 login 保留期清理回收,§9.24)';
-- idx_last_login 服务保留期清理(DELETE WHERE last_login_at<cutoff)。
-- 既有库需手动补:ALTER TABLE account_devices ADD KEY idx_last_login (last_login_at);

CREATE TABLE IF NOT EXISTS `account_bans` (
    `id`         BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    `player_id`  BIGINT UNSIGNED       NULL COMMENT 'NULL 表示该 ban 仅按 device 维度',
    `device_id`  VARCHAR(128)          NULL COMMENT 'NULL 表示该 ban 仅按 player 维度',
    `reason`     VARCHAR(255)     NOT NULL DEFAULT '',
    `banned_at`  DATETIME         NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `expires_at` DATETIME              NULL COMMENT 'NULL=永久',
    PRIMARY KEY (`id`),
    KEY `idx_player_active`  (`player_id`,  `expires_at`),
    KEY `idx_device_active`  (`device_id`,  `expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 账号 / 设备 封禁记录';

-- 选角权威化(2026-07-08):玩家已选角色,login 服务是数据权威。
--   - 登录时读出 → LoginResponse.selected_role_id(客户端选角界面预选中)。
--   - SelectRole RPC upsert → 再经 hub_allocator 把 role_id 签进 hub 票据。
--   - role_id 是配置表 ID(CfgRole.Id,uint32,CLAUDE.md §9.12),非 snowflake。
CREATE TABLE IF NOT EXISTS `player_roles` (
    `player_id`  BIGINT UNSIGNED NOT NULL,
    `role_id`    INT UNSIGNED    NOT NULL COMMENT 'CfgRole.Id,配置表角色 ID',
    `updated_at` DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`player_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci
  COMMENT='Pandora 玩家已选角色(选角权威数据)';

-- 注:开发期账号不在此 init.sql 写入(bcrypt cost 不固定,且需要应用层 hash)。
-- login 开 dev_skip_password / dev_auto_register 时,客户端首次登录即自动懒注册账号。
