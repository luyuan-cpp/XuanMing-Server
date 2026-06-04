# Pandora 基础设施规范

> **此文档是写代码前的强制阅读**。所有 MySQL 表 / Redis key / Kafka topic / etcd 路径都按此规范命名,**不允许 ad-hoc**。

## 1. 命名总则

- **资源命名空间统一用 `pandora`(全小写)**,跟仓库名 `Pandora` 区分
- **多段命名按存储引擎习惯**:
  - Redis key:`:` 分隔
  - Kafka topic:`.` 分隔
  - MySQL 表:`_` 分隔(snake_case)
  - etcd path:`/` 分隔
- **小写 + 下划线**,不用驼峰

## 2. MySQL Schema

### 2.1 数据库划分

```
pandora_account        # 账号(login)
pandora_player         # 玩家档案 / 段位 / 英雄池 / 皮肤
pandora_social         # 好友 / 黑名单 / 公会(后期)
pandora_battle         # 战斗结算历史 / 战绩
pandora_trade          # 交易订单 / 审计
pandora_ops            # 运营日志 / 封禁 / 客诉
```

⚠️ **不要把所有表塞 `pandora` 一个库**,按职能分库,后期容易拆服。

### 2.2 通用字段约定

每张业务表必须有:

```sql
id           BIGINT       PRIMARY KEY  AUTO_INCREMENT  -- 自增主键
created_at   DATETIME(3)  NOT NULL  DEFAULT CURRENT_TIMESTAMP(3)
updated_at   DATETIME(3)  NOT NULL  DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
deleted_at   DATETIME(3)  NULL                                   -- 软删
version      INT          NOT NULL  DEFAULT 0                    -- 乐观锁
```

**禁止**:`is_delete` / `del_flag` / `state=999` 之类的软删变体。统一 `deleted_at`。

### 2.3 关键表清单

#### `pandora_account`
| 表 | 用途 | 关键索引 |
|---|---|---|
| `accounts` | 账号 | uniq(account), uniq(email), idx(device_id) |
| `account_devices` | 设备绑定 | idx(account_id), uniq(device_id) |
| `account_bans` | 封禁记录 | idx(account_id, ban_until) |

#### `pandora_player`
| 表 | 用途 | 关键索引 |
|---|---|---|
| `players` | 玩家档案 | uniq(account_id), idx(nickname), idx(mmr) |
| `player_heroes` | 英雄解锁 | uniq(player_id, hero_id) |
| `player_skins` | 皮肤 | uniq(player_id, skin_id) |
| `player_currencies` | 金币 / 钻石 / 各种货币 | uniq(player_id, currency_type) |
| `player_inventory` | 道具背包 | idx(player_id), uniq(player_id, item_uid) |

#### `pandora_battle`
| 表 | 用途 | 关键索引 |
|---|---|---|
| `battles` | 一局对局元数据 | uniq(match_id), idx(ended_at) |
| `battle_player_stats` | 每个玩家的战绩 | idx(player_id, ended_at), idx(match_id) |
| `mmr_history` | MMR 变化历史 | idx(player_id, created_at) |

#### `pandora_trade`
| 表 | 用途 | 关键索引 |
|---|---|---|
| `trade_orders` | 交易订单 | uniq(order_id), idx(seller_id), idx(buyer_id) |
| `trade_audit` | 审计日志(append-only) | idx(order_id), idx(created_at) |

### 2.4 字符集 / 引擎

```sql
ENGINE=InnoDB
DEFAULT CHARSET=utf8mb4
COLLATE=utf8mb4_0900_ai_ci      -- MySQL 8.x 默认
```

⚠️ **不许用 utf8**(实际 3 字节),emoji 和复杂字符存不进。

## 3. Redis Key Schema

### 3.1 命名格式

```
pandora:<domain>:<entity>:<id>[:<field>]
```

**强制规则**:
- 全小写
- `:` 分隔
- 单段不超过 32 字符,总长不超过 128 字符
- **不准用动词**(`pandora:get_player:123` ❌,`pandora:player:123` ✅)

### 3.2 Key 清单(W1 规划)

#### Session / Token
| Key | 类型 | TTL | 用途 |
|---|---|---|---|
| `pandora:sess:<player_id>` | hash | 24h | 玩家 session |
| `pandora:ticket:<jti>` | string | 5min | DS 票据(防重放) |
| `pandora:locator:<player_id>` | hash | 30s heartbeat | 玩家位置 |

#### Team
| Key | 类型 | TTL | 用途 |
|---|---|---|---|
| `pandora:team:<team_id>` | hash | 1h idle | 队伍状态 |
| `pandora:team:player:<player_id>` | string | 1h idle | 玩家所在队伍 |
| `pandora:team:invites:<player_id>` | set | 5min | 收到的邀请 |

#### Match
| Key | 类型 | TTL | 用途 |
|---|---|---|---|
| `pandora:match:queue:<bracket>:<region>` | sorted set | - | 匹配队列 |
| `pandora:match:<match_id>` | hash | 30min | 匹配实例状态机 |
| `pandora:match:player:<player_id>` | string | 30min | 玩家所在 match_id |

#### DS Allocator
| Key | 类型 | TTL | 用途 |
|---|---|---|---|
| `pandora:ds:battle:<pod_name>` | hash | 30s heartbeat | 战斗 DS 实例状态 |
| `pandora:ds:hub:<pod_name>` | hash | 30s heartbeat | 大厅 DS 实例状态 |
| `pandora:ds:battle:idle` | set | - | 空闲战斗 DS 池 |

#### Lock / Cache
| Key | 类型 | TTL | 用途 |
|---|---|---|---|
| `pandora:lock:<resource>` | string(NX EX) | ≤30s | 分布式锁 |
| `pandora:cache:player:<player_id>` | hash | 5min | 玩家档案缓存 |
| `pandora:cache:hero:list` | string(json) | 1h | 英雄列表配置缓存 |

⚠️ **lock TTL 严禁超过 30s**,业务跑完必须主动释放。

### 3.3 反模式禁令

- ❌ 不许用 `KEYS *` 遍历(用 `SCAN`)
- ❌ 不许把大对象塞 string(>1MB),用 hash 拆分
- ❌ 不许无 TTL 长期存(除了 sorted set 队列)
- ❌ 不许直接 `DEL` 大 key(用 `UNLINK`)

## 4. Kafka Topic Schema

### 4.1 命名格式

```
pandora.<domain>.<event>
pandora.dlq.<original_topic>     # 死信队列
```

### 4.2 Topic 清单

| Topic | 分区 | 保留 | 生产者 | 消费者 | 备注 |
|---|---|---|---|---|---|
| `pandora.login.event` | 8 | 7d | login | 风控、审计 | 登录登出 |
| `pandora.match.found` | 4 | 3d | matchmaker | ds_allocator | 匹配成功 |
| `pandora.match.failed` | 4 | 3d | matchmaker | (告警) | 匹配失败/超时 |
| `pandora.match.progress` ⭐ | 8 | 1h | matchmaker | **push** | 匹配进度推送(key=player_id)|
| `pandora.team.update` ⭐ | 8 | 1h | team | **push** | 队伍状态变更推送(key=player_id)|
| `pandora.chat.world` | 16 | 1d | chat | **push** | 世界聊天推送 |
| `pandora.chat.team` ⭐ | 8 | 1h | chat | **push** | 队伍聊天推送(key=player_id)|
| `pandora.chat.private` ⭐ | 8 | 1d | chat | **push** | 私聊推送(key=target_player_id)|
| `pandora.player.update` | 8 | 7d | player / data_service | **push** + 缓存失效 | 玩家档案变更 |
| `pandora.friend.event` ⭐ | 4 | 1d | friend | **push** | 好友请求 / 上线提醒 |
| `pandora.system.notify` ⭐ | 4 | 7d | 运营 / 各 go | **push** | 系统公告 / 邮件 / 红点 |
| `pandora.ds.lifecycle` | 4 | 7d | ds_allocator / hub_allocator | 监控 | DS 拉起/回收/崩溃 |
| `pandora.battle.result` | 16 | 30d | Battle DS | battle_result | ⭐ 核心,at-least-once + 幂等落库 |
| `pandora.trade.audit` | 4 | 90d | trade | 审计、风控 | 交易日志(append-only) |
| `pandora.locator.update` | 8 | 1h | hub DS / battle DS | player_locator | 玩家位置变更 |

⭐ = 2026-06-03 新增推送 topic,见 `gateway-decision.md` §5。所有标 ⭐ 的 topic 都被 **pandora-push** 服务消费,统一推 WebSocket 给客户端。

### 4.3 分区键约定

- **玩家相关 topic**:`key = player_id`(同一玩家事件有序)
- **战斗结算**:`key = match_id`(同一局事件有序,且能幂等去重)
- **DS lifecycle**:`key = pod_name`

### 4.4 死信策略

每个核心 topic 配套 `pandora.dlq.<topic>`,保留 30 天。消费失败 3 次进 DLQ,人工介入。

⚠️ **`pandora.battle.result` 必须有 DLQ**,丢战绩等于丢钱。

## 5. etcd Path Schema

### 5.1 路径格式

```
/pandora/<env>/<category>/<entity>
```

`<env>` = `dev` / `staging` / `prod`,**禁止跨环境共用 etcd cluster**。

### 5.2 路径清单

#### 服务发现
```
/pandora/dev/services/login/<instance_id>          → endpoint json
/pandora/dev/services/matchmaker/<instance_id>
/pandora/dev/services/ds_allocator/<instance_id>
...
```

#### 配置中心
```
/pandora/dev/config/login                          → toml/json 配置
/pandora/dev/config/matchmaker
/pandora/dev/config/global                         → 全局通用(MMR 公式参数等)
```

#### Leader Election(只 ds_allocator / hub_allocator 需要)
```
/pandora/dev/leader/ds_allocator
/pandora/dev/leader/hub_allocator
```

### 5.3 TTL / lease

- 服务注册:lease 10s,5s 续约一次
- 配置:无 TTL,变更触发 watch
- Leader:lease 15s

## 6. 端口分配(开发环境)

### 6.1 基础设施(docker-compose)

| 服务 | 端口 | 备注 |
|---|---|---|
| MySQL | 3307 | 跟 mmorpg 3306 错开 |
| Redis | 6380 | 跟 mmorpg 6379 错开 |
| Kafka | 9093 | 跟 mmorpg 9092 错开 |
| Zookeeper | 2182 | |
| etcd client | 2380 | 跟 mmorpg 2379 错开 |
| etcd peer | 2381 | |
| Prometheus | 9091 | 跟 mmorpg 9090 错开 |
| Grafana | 3001 | 跟 mmorpg 3000 错开 |
| Jaeger UI | 16687 | 跟 mmorpg 16686 错开 |

### 6.2 Go 服务 gRPC 端口

| 服务 | gRPC 端口 | metrics 端口(+1000) |
|---|---|---|
| login | 50001 | 51001 |
| player | 50002 | 51002 |
| data_service | 50003 | 51003 |
| friend | 50004 | 51004 |
| chat | 50005 | 51005 |
| player_locator | 50006 | 51006 |
| team | 50010 | 51010 |
| matchmaker | 50011 | 51011 |
| trade | 50012 | 51012 |
| dialogue | 50013 | 51013 |
| **push** ⭐ | **50014**(gRPC server stream)| **51014** |
| ds_allocator | 50020 | 51020 |
| hub_allocator | 50021 | 51021 |
| battle_result | 50022 | 51022 |

⭐ = 2026-06-04 终版新增。push 服务用 Kratos transport/grpc 暴露 server stream,客户端经 Envoy 连过来(gRPC-Web → gRPC 转换)。

**所有 14 个 go 服务全部用 gRPC 端口**(50001-50022 段),协议统一。

### 6.3 Edge Gateway(Envoy)

| 服务 | 端口 | 用途 |
|---|---|---|
| Envoy(HTTPS)| **8443** | 客户端入口,gRPC-Web over HTTP/2 TLS |
| Envoy admin | **9901** | 配置 / metrics / 健康检查 |

Envoy 是基础设施组件,**不是 go 服务**。它做:
- TLS 终止(客户端 HTTPS → 内网明文 gRPC)
- gRPC-Web ↔ gRPC 协议转换(envoy `grpc_web` filter)
- JWT 鉴权(envoy `jwt_authn` filter)
- 限流 / 熔断 / 重试

详见 `gateway-decision.md` §5。

### 6.4 UE DS 端口

- Hub DS:Agones 从 7000-7500 动态分配
- Battle DS:Agones 从 7501-8000 动态分配

## 7. 时间约定

- **所有时间戳用 Unix milliseconds**(int64)
- **DB 字段类型 `DATETIME(3)`**(毫秒精度)
- **proto 字段命名 `xxx_at_ms`**(明确单位)
- **永远存 UTC**,展示时再转时区

⚠️ 禁止 `DATETIME` 不带精度(默认秒级,丢数据)。

## 8. ID 生成

- **player_id / team_id / match_id**:snowflake(`pkg/snowflake`)
- **trade_order_id**:snowflake + 业务前缀(`T` + 18 位)
- **数据库自增 id**:仅做物理主键,**不对外暴露**
- **session_token / jti**:UUID v4

⚠️ 禁止用自增 id 当业务标识对外。

## 9. 字符串长度上限(数据库 VARCHAR)

| 字段类型 | 上限 |
|---|---|
| nickname | 32 |
| account | 64 |
| email | 128 |
| device_id | 64 |
| ip_v6 | 64 |
| reason / remark | 256 |
| 长文本 / json | TEXT / JSON 类型 |

## 10. 监控指标命名(Prometheus)

```
pandora_<service>_<metric>{<labels>}
```

例:
```
pandora_login_request_total{method="Login",code="0"}
pandora_login_request_duration_seconds_bucket{method="Login",le="0.1"}
pandora_matchmaker_queue_size{bracket="diamond",region="cn"}
pandora_ds_allocator_pod_count{state="running"}
pandora_kafka_consumer_lag{topic="pandora.battle.result",group="battle_result"}
```

**强制 label**:`service`, `instance`(由抓取端加)
**禁止高基数 label**:不要把 `player_id` 放 label!

## 11. 日志格式(zap structured)

```json
{
  "ts": "2026-06-03T10:00:00.123Z",
  "level": "info",
  "service": "matchmaker",
  "trace_id": "abc123",
  "player_id": 1001,
  "match_id": "M_xxx",
  "msg": "match found",
  "queue_seconds": 42
}
```

**强制字段**:`ts` / `level` / `service` / `msg`
**业务字段**:`trace_id`, `player_id`, `match_id`, `team_id`, `error`
**禁止**:`fmt.Sprintf` 拼字符串到 msg(用 zap field)
