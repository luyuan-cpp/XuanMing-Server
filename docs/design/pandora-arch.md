# Pandora 总架构

> 立项决策、玩家流转、服务清单、关键时序。本文档是 §1 必读。

## 1. 项目定位

- **类型**:MOBA + 持续在线大厅(类 Albion / New World 城镇 + LoL 战斗)
- **核心循环**:登录 → 进大厅(可走动 / 互打 / 试技能 / NPC 对话 / 交易 / 组队)→ 匹配 → 进战斗 DS 打一局 → 结算回大厅
- **关键参数**:
  - 大厅 DS:**500 人/实例**,单城镇约 1km²,**全图自由 PvP**
  - 战斗 DS:10 人(5v5),约 25 分钟/局
  - 战斗 tick:30~60 Hz / 大厅 tick:20~30 Hz

## 2. 双仓库结构

- 后端仓库:go 服务 + proto + docs + deploy
- UE 仓库:UE 5.7 客户端 + 大厅 DS + 战斗 DS

**协作纪律**:
- proto **source of truth 在 Pandora 后端仓库**(`Pandora/proto/`)
- `Pandora` CI 在 proto 改动时,自动生成 cpp .pb.h 推送到 UE 仓库的 `Source/Pandora/Generated/Proto/`
- UE 仓库不允许直接改 .proto,所有改动从后端仓库来

## 3. 服务清单(go,共 14 个)

⚠️ **架构演化记录**:
- 2026-06-03 上午:13 个业务服(login + 12 个)
- 2026-06-03 中午:推翻,加 gateway + push → 15 个(2026-06-04 再次推翻)
- **2026-06-04 终版:14 个**(13 业务服 + 1 集中 push 服务)
- gateway 不作为 go 服务(改用 **Envoy** 这个基础设施组件,详见 `gateway-decision.md`)

| # | 服务 | 职责 | 是否有状态 | 依赖 |
|---|---|---|---|---|
| 1 | **login** | 账号 / 登录 / 颁发 DS 票据 | 无 | mysql + redis |
| 2 | **player** | 玩家档案 / 段位 / 英雄池 / 皮肤 | 无(读穿 mysql) | mysql + redis |
| 3 | **data_service** | 玩家数据读写网关 / 缓存 | 无 | mysql + redis |
| 4 | **friend** | 好友 / 黑名单 | 无 | mysql + redis |
| 5 | **chat** | 频道(世界 / 队伍 / 私聊) | 弱(channel 路由) | redis pub/sub + kafka |
| 6 | **player_locator** | 玩家位置(hub_id / battle_id) | 强 | redis |
| 7 | **team** | 组队状态机 | 强 | redis |
| 8 | **matchmaker** | MMR / 队伍合并 / 排队 / bot 降级 | 强 | redis + ds_allocator |
| 9 | **trade** | 两阶段交易 / 审计 | 强 | redis + mysql + kafka |
| 10 | **dialogue** | NPC 对话树运行时 | 无(读配置) | mysql / 配置中心 |
| 11 | **ds_allocator** | 战斗 DS 调度(Agones GameServer) | 弱(etcd) | k8s + agones + etcd |
| 12 | **hub_allocator** | 大厅 DS 分片调度 | 弱(etcd) | k8s + agones + etcd |
| 13 | **battle_result** | 战斗结算消费 / 幂等落库 | 无 | kafka + mysql |
| 14 | **push** ⭐ | gRPC server stream 推送(集中持有客户端 stream + 消费 kafka 转发) | 强(连接索引) | kafka + redis(离线消息) |

⭐ = 2026-06-04 终版新增。push 是 Kratos transport/grpc 暴露的 server stream 服务,客户端通过 Envoy 连过来,详见 `gateway-decision.md` §6。

**框架统一**:13 个业务服 + push 服务**全部用 Kratos**(2026-06-04 推翻 D2.1 go-zero 决策)。Envoy 作为基础设施,不计 go 服务。

**排期说明(2026-06-06)**:`friend` / `chat` 保留在服务清单、端口和 topic 规划中,但当前不进入实现主线。它们等 UE 客户端、Hub DS、Battle DS、Agones 和核心玩法闭环完成后,再作为社交尾部功能实现。

## 4. UE 端模块(共 5 个,在 UE 仓库)

| 模块 | 用途 | 编译目标 |
|---|---|---|
| `Source/Pandora/` | 客户端(玩家本地运行) | Win64 Game / Linux Game |
| `Source/PandoraShared/` | 客户端 + DS 共用(GAS、proto、票据) | 全部 |
| `Source/PandoraHubServer/` | 大厅 DS 专属(GameMode、AOI、跨分片) | Linux Server |
| `Source/PandoraBattleServer/` | 战斗 DS 专属(GameMode、结算上报) | Linux Server |
| `Source/PandoraEditor/` | 编辑器扩展(技能数据 DataAsset 编辑器) | Editor |

## 5. 玩家流转图

```
┌─────────┐
│ Client  │
└────┬────┘
     │ 1. POST /login(账号 + 密码)
     ▼
┌─────────┐  2. 查 mysql 验证        ┌─────────┐
│  login  │ ◀─────────────────────▶ │  mysql  │
└────┬────┘                          └─────────┘
     │ 3. 调 hub_allocator 分配 hub
     ▼
┌──────────────┐  4. 查 etcd 选 hub  ┌──────────┐
│ hub_allocator│ ◀─────────────────▶│ Agones K8s│
└──────┬───────┘                     └──────────┘
       │ 5. 返回 hub_ds_addr + JWT 票据
       ▼
┌─────────┐
│ Client  │ 6. 直连 hub DS(UDP / Unreal NetDriver)
└────┬────┘
     ▼
┌──────────────┐  7. 校验票据(无状态 JWT)
│   Hub DS     │
│ (Linux UE)   │  8. 玩家在大厅走动 / 放技能 / 互打
└──────┬───────┘  9. NPC / 商店 / 交易 / 组队 → gRPC 调后端
       │
       │ 10. 玩家点"开始匹配" → gRPC 调 matchmaker
       ▼
┌──────────────┐  11. MMR 撮合 5v5
│  matchmaker  │
└──────┬───────┘
       │ 12. 凑齐 10 人 → 调 ds_allocator
       ▼
┌──────────────┐  13. Agones 拉起 battle DS pod
│ ds_allocator │
└──────┬───────┘
       │ 14. battle_ds_addr 推回 hub DS
       ▼
┌──────────────┐  15. hub DS 把地址发给客户端,断开连接
│   Hub DS     │
└──────┬───────┘
       │ 16. 客户端用新票据连 battle DS
       ▼
┌──────────────┐  17. 战斗(25 分钟)
│  Battle DS   │
└──────┬───────┘  18. 结束 → kafka 发 BattleResult
       │
       ▼
┌──────────────┐  19. 消费 → 幂等落库 → 段位更新
│battle_result │
└──────────────┘
       │
       ▼
       玩家从 battle DS 退出 → 重新连 hub DS(回大厅)
```

## 6. 协议矩阵

⚠️ **架构决策 2026-06-04 终版**:
- 客户端 **2 条连接**(① UE NetDriver / ② FHttpModule gRPC-Web over HTTP/2 TLS)
- 后端框架 **Kratos**(替代 go-zero)
- Edge Gateway 用 **Envoy**(替代 go-zero gateway)
- 推送走 **gRPC server stream**(集中 push 服务持有客户端 stream)

详见 `gateway-decision.md`。

| Caller → Callee | 协议 | 节奏 | 备注 |
|---|---|---|---|
| **Client → Envoy**(8443 HTTPS)| gRPC-Web over **HTTP/2 + TLS** | unary 1~10 req/s/玩家;stream 长连 | UE FHttpModule 自带,自研 grpc-web frame 解析 |
| **Client → Hub DS / Battle DS** | UE NetDriver(UDP-like)| 高频 30~60Hz | 仅游戏内同步,GAS / Replication |
| Envoy → 各 Kratos 业务服 | 标准 gRPC unary + server stream | 业务请求触发 / stream 长连 | k8s Service + DNS 服务发现 |
| matchmaker → ds_allocator | gRPC unary | 匹配成功一次 | 拉起战斗 DS |
| Hub DS → hub_allocator | gRPC **unary** Heartbeat | **每 5s** | 单向心跳,response 携带控制指令 |
| Battle DS → ds_allocator | gRPC **unary** Heartbeat | **每 5s** | 同上 |
| 业务服 → kafka | 生产推送事件 | 业务变更触发 | push 服务消费 |
| push → kafka | 消费推送 topics | 持续 | consumer group: pandora-push |
| Battle DS → battle_result | Kafka(at-least-once)| 战斗结束一次 | `pandora.battle.result` topic |
| 各服务 ↔ etcd | gRPC | 服务发现 / 配置 | k8s Service 也可代替 |
| 各服务 ↔ Kafka | Kafka 协议 | 异步事件 | sarama |

## 7. 关键时序

### 时序 1:玩家从 Hub 进 Battle(最复杂的链路)

```
Client    Hub DS    matchmaker    ds_allocator    Agones    Battle DS    battle_result
  │         │           │              │             │          │             │
  │ Match   │           │              │             │          │             │
  │────────▶│           │              │             │          │             │
  │         │ StartMatch│              │             │          │             │
  │         │──────────▶│              │             │          │             │
  │         │           │ (MMR 撮合)   │             │          │             │
  │         │           │ Allocate     │             │          │             │
  │         │           │─────────────▶│             │          │             │
  │         │           │              │ CreateGameSrv│         │             │
  │         │           │              │────────────▶│          │             │
  │         │           │              │             │ k8s 起 pod│            │
  │         │           │              │             │──────────▶│            │
  │         │           │              │             │          │ Ready       │
  │         │           │              │             │◀─────────│             │
  │         │           │              │  ds_addr    │          │             │
  │         │           │              │◀────────────│          │             │
  │         │           │ ds_addr+票据 │             │          │             │
  │         │           │◀─────────────│             │          │             │
  │         │  推送通知 │              │             │          │             │
  │         │◀──────────│              │             │          │             │
  │ ds_addr │           │              │             │          │             │
  │◀────────│           │              │             │          │             │
  │   断开 hub          │              │             │          │             │
  │────×    │           │              │             │          │             │
  │ 连 battle DS(带票据)                            │          │             │
  │─────────────────────────────────────────────────────────────▶│            │
  │                                                              │ 校验票据   │
  │              战斗开始(25 分钟)                              │            │
  │ ◀══════════════════════════════════════════════════════════▶ │            │
  │                                                              │            │
  │                                                              │ 战斗结束   │
  │                                                              │ Kafka 发   │
  │                                                              │──────────▶│
  │                                                              │            │ 幂等落库
  │ 客户端断开 battle DS,重连 hub DS 回大厅                                  │
```

### 时序 2:大厅内的技能命中(500 人 PvP 关键路径)

```
Client A          Hub DS                Client B (在 A 50 米内)
   │                │                       │
   │ CastAbility    │                       │
   │───────────────▶│                       │
   │                │ GAS Predict(本地)    │
   │                │                       │
   │                │ Activate Ability      │
   │                │ (服务端权威)          │
   │                │                       │
   │                │ 执行 Cost / CD        │
   │                │ 命中判定(网格 trace) │
   │                │                       │
   │                │ ApplyGameplayEffect   │
   │                │ to Target B           │
   │                │                       │
   │                │ AOI 广播 GameplayCue  │
   │                │──────────────────────▶│
   │                │                       │ 表现层(特效/音效)
   │                │ Replicate B 血量      │
   │                │──────────────────────▶│
   │ Replicate A    │                       │
   │ ability state  │                       │
   │◀───────────────│                       │
```

## 8. 部署拓扑(本地开发期)

```
开发机 (Windows F:)
├── docker-compose:
│   ├── mysql       :3307
│   ├── redis       :6380
│   ├── kafka       :9093
│   ├── etcd        :2380
│   ├── prometheus  :9091
│   └── grafana     :3001
│
├── go services(各自一个进程,monorepo go.work):
│   ├── login           :50001
│   ├── player          :50002
│   ├── data_service    :50003
│   ├── friend          :50004
│   ├── chat            :50005
│   ├── player_locator  :50006
│   ├── team            :50010
│   ├── matchmaker      :50011
│   ├── trade           :50012
│   ├── dialogue        :50013
│   ├── ds_allocator    :50020
│   ├── hub_allocator   :50021
│   └── battle_result   :50022 (kafka consumer)
│
├── minikube(本地 k8s):
│   ├── agones-system
│   └── pandora-ds:
│       ├── hub-fleet     (Hub DS pods, replicas=N)
│       └── battle-fleet  (Battle DS pods, allocate on demand)
│
└── UE 编辑器(C:/work/Pandora/)
    ├── Editor 跑客户端(PIE)
    └── Linux Server target → docker image → minikube
```

## 9. 关键不变量(任何改动都要满足,继承 CLAUDE.md §9)

1. **玩家在线只能在一个 DS**(hub 或 battle,不能两个)— player_locator 强制
2. **战斗结果幂等**(同一 match_id 只能落库一次)— battle_result 用 mysql unique key
3. **DS 票据短时效**(JWT exp 5 分钟,防止泄漏)— login 颁发,DS 校验
4. **DS 崩溃必有补偿**(15s 心跳超时 → ds_allocator 标记 abandoned → 玩家段位回滚)
5. **proto 字段编号上线后不复用**(上线后 deprecate 不删除;开发期间已删除字段可复用编号,但必须重新生成 proto 并完整编译所有已启用 module)
6. **MMR 计算在 battle_result**(不在 DS 算,DS 不可信)
7. **Snowflake 业务 ID 一律 uint64,配置表 ID 默认 uint32,proto enum / 状态常量保持生成 enum 类型或 int32 语义**;ID unsigned 规则不扩展到 `TEAM_STATE_*` / `STATE_*` / `*_REASON_*` 等枚举常量
8. **客户端只拿客户端可见结构**:不得把服务端存储快照 / 数据库整行 / Redis value 原样返回或推送给客户端;服务端按客户端当前需求的最小数据单位组装视图,必要时重新计算派生字段。

## 10. 风险登记册

| 风险 | 级别 | 缓解 |
|---|---|---|
| 500 人 hub Replication 性能 | 🔴 高 | Iris + AOI 网格 + 限流;早压测 |
| GAS + Iris 兼容性坑 | 🔴 高 | 留 2 周 buffer;不行回退 RepGraph |
| DS 崩溃数据丢失 | 🟡 中 | kafka at-least-once + 幂等 + 死信 |
| 跨 hub 分片可见性 | 🟡 中 | 先做"看不到"最简方案 |
| 防作弊 | 🟡 中 | 服务端权威 + 移动速度校验 + 审计日志 |
| UE 5.7 API 不稳定 | 🟡 中 | 关注 release notes,必要时降到 5.6 |
| 单人开发节奏 | 🟡 中 | 严格遵守 PROGRESS.md + 每日 commit |

## 11. 决策行(只追加)

| Round | 日期 | 决策 | 数据 |
|---|---|---|---|
| 0 | 2026-06-03 | 立项,新建 Pandora 项目 | - |
| 0 | 2026-06-03 | 后端 monorepo go.work,UE 独立仓库 | - |
| 0 | 2026-06-03 | 大厅 DS 化,500 人/实例,全图自由 PvP | - |
| 0 | 2026-06-03 | UE 5.7 + Iris + GAS,Agones 调度 | - |
| 0 | 2026-06-03 | License MIT,Go 1.23,基础设施全新搭一套 | - |
| 0 | 2026-06-03 | 后端框架继续用 go-zero(历史决策,后续已切换 Kratos) | - |
| 0 | 2026-06-03 | **否决"严格 A:客户端只连 DS"** | 见 `architecture-rejected-strict-ds-only.md`,6 个不可接受后果(故障域过大 / 500 人 PvP 性能预算被破 / UE 代码量爆炸 / 大厂无先例) |
| 0 | 2026-06-03 | 业务请求走独立通道(不经过 DS),具体方案待定 | 候选:WebSocket gateway / 客户端直连各 go 服务 / 专用 push 服务,详见 `gateway-decision.md`(待写) |
| 0 | 2026-06-03 | 推送方案选定 P3:**专用 push 服务** | 业务 → kafka → push(go,新增第 14 个服务)→ 客户端;Hub DS 不兼任推送中转 |
| 0 | 2026-06-03 | **RPC response 与 kafka push 乱序问题确认 = 协议设计问题**(非架构问题) | 见 `protocol-ordering-rules.md`,固化 4 个原则 |
| 0 | 2026-06-03 | 4 协议原则 | Response 同步完整 / push 不发给 caller / 已受理型显式标注 / proto 注释强制 |
| 0 | **2026-06-04** | **切换后端框架:go-zero → Kratos**(推翻 D2.1)| go-zero 不支持 gRPC stream,推送架构受限;Kratos 基于原生 grpc-go,完整支持 unary + stream |
| 0 | 2026-06-04 | 引入 **Envoy 作为 Edge Gateway** | 标准 gRPC-Web ↔ gRPC 协议转换,替代 go-zero/gateway |
| 0 | 2026-06-04 | 客户端协议:**gRPC-Web over HTTP/2 TLS** | UE 5.7 FHttpModule 已暴露(`SetOption("HttpVersion","2TLS")`),源码挖掘验证 |
| 0 | 2026-06-04 | 推送架构:**集中 push 服务 + gRPC server stream** | 替代之前规划的 WebSocket 自研 + envelope,标准 gRPC 协议 |
| W3 ⑦.0 | 2026-06-05 | **协议类型边界固化** | Snowflake 业务 ID 一律 `uint64`;配置表 ID 默认 `uint32`;proto enum / 状态常量保持生成 enum 类型或 `int32` 语义,不按非负常量改 `uint32` |
| W4 文档 | 2026-06-06 | **客户端可见结构与服务端存储快照硬隔离** | 面向客户端的 response / push 不得直接返回 `*StorageRecord`、数据库整行、Redis value、内部 Kafka envelope 或审计字段;由服务端按客户端最小需求组装 / 计算视图 |
| 0 | 2026-06-04 | 客户端实现:**自研 grpc-web 客户端基于 FHttpModule** | 不引入第三方 UE gRPC 插件(80MB+ / SSL 冲突 / UE 5.x 兼容性差);大厂(米哈游/腾讯/网易/Riot/Epic)客户端都不直连 gRPC |
| 0 | 2026-06-04 | 服务清单 13 → **14**(新增 push)| Envoy 作为基础设施不计 go 服务 |
| 0 | 2026-06-04 | 客户端连接最终值 = **2 条**(NetDriver + FHttpModule)| 用户铁律确认 |
| 排期 | 2026-06-06 | **friend / chat 暂缓到最后** | 社交好友(:50004)和聊天(:50005)当前只保留协议/端口/topic规划;实现等 UE 与核心链路全部完成后再做 |
| TLS/发布 | 2026-06-10 | **生产连接 ② TLS 使用公网 CA + 真实域名;dev mkcert 自签只通过 DebuggingCertificatePath 叠加公开 dev CA** | 玩家设备默认信任公网 CA,零配置握手;dev 的 mkcert 信任问题不带到生产。详见 `gateway-decision.md` §14 |
| ID 生成 | 2026-06-11 | **拒绝 Redis INCR 发号;当前继续静态 `node.zone_id` + 本地 snowflake,未来动态多副本用 etcd Lease 分配 nodeID** | Redis INCR 慢 4~5 个数量级且有持久化/主从切换计数回退发重号风险;Redis `SETNX+TTL+看门狗` 不能可靠 fencing。etcd 方案仍需 KeepAlive/session monitor,失租必须停发并退出。详见 `infra.md` §8.1 |
| UE push | 2026-06-15 | **push stream 当前保持 AsyncTask 回传成品帧;解析器锁只保护 StreamParser 生命周期** | push 是低频事件流,双缓冲队列不能替代解析器生命周期同步;若未来追求零锁,改为每条 HTTP stream 闭包独占解析器 + 队列回传帧。详见 `gateway-decision.md` §15 |
| friend 扩展 | 2026-06-18 | **全服分片好友图不做跨玩家分布式事务,改为 request 单点权威 CAS + Kafka 异步幂等建边** | 当前 `AcceptFriend` 仅在单 MySQL `pandora_social` 内成立;Redis Cluster / 分片 MySQL 都不能原样承载跨 requester/target 原子事务。好友图权威主存推荐按 owner `player_id` 分片 MySQL,Redis 只做热点缓存。详见 `go-services.md` §2.4 |
| 存储扩容 | 2026-06-18 | **好友图扩容存储路线选 (A) TiDB 过渡;否决 (B) 分片 MySQL + dtm、(C) 其他分布式 ACID 库** | 阶段 2(千万级早期)TiDB 代码改动最小、保跨人强一致与硬上限;现阶段仍单 MySQL 不提前引入;阶段 3 极限体量再把热路径拆 §5 CAS + Kafka 异步建边卸 2PC。雪花主键热点须 `AUTO_RANDOM` 打散。详见 `friend-distributed-scaling.md` §8 / §14 |
| 存储扩容 | 2026-06-18 | **人工拍板推翻“不提前引入”:现就把 friend(及同库 chat)切 TiDB** | 项目内已落地:TiDB 版 `pandora_social` DDL(§8.2 热点调优)+ friend TiDB 连接配置;Go 业务零改动(TiDB 兼容 MySQL 协议)。起集群 / 装载 DDL / 数据迁移 = Codex / 人(§11.1)。详见 `friend-distributed-scaling.md` §14 “落地修订” + `deploy/tidb-init/README.md` |
| 全服扩容 | 2026-06-19 | **DAU 200万全区全服:zone_id 是 snowflake 机器号非选区(不删);单 Redis→Cluster、单 MySQL→分库、nodeID etcd 自动分配、push 横扩、Agones 池化** | 抗压取决于 CCU(~30万)非注册量(1000万)。已落地能力:`pkg/snowflake/etcdnode`(etcd Lease 抢 nodeID)、`redisx.NewUniversalClient`(Cluster)、`mysqlx.ShardSet`(分库),均非破坏式。push 横扩走定向路由(注册表+分区),Agones 走 Ready 池化。社交库仍走 TiDB 不套 ShardSet。详见 `scale-dau-2m.md` |
